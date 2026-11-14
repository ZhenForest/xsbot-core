package app

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/admin"
	"github.com/ZhenForest/xsbot-core/internal/botapi"
	"github.com/ZhenForest/xsbot-core/internal/config"
	"github.com/ZhenForest/xsbot-core/internal/dispatch"
	"github.com/ZhenForest/xsbot-core/internal/logging"
	"github.com/ZhenForest/xsbot-core/internal/media"
	"github.com/ZhenForest/xsbot-core/internal/message"
	"github.com/ZhenForest/xsbot-core/internal/onebot"
	"github.com/ZhenForest/xsbot-core/internal/plugins"
	"github.com/ZhenForest/xsbot-core/internal/router"
	run "github.com/ZhenForest/xsbot-core/internal/runtime"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type App struct {
	Root           string
	cfg            config.Config
	formal         config.Config
	mu             sync.Mutex
	rules          *config.Rules
	router         *router.Router
	dispatch       *dispatch.Queue
	manager        *plugins.Manager
	shared         *botapi.Shared
	log            *logging.Logger
	web            *admin.Server
	initializing   atomic.Bool
	closing        atomic.Bool
	stop           chan struct{}
	stopOnce       sync.Once
	restart        atomic.Bool
	lock           *run.Lock
	installed      map[string]bool
	writeConfig    func(string, []byte) error
	shutdown       context.Context
	shutdownCancel context.CancelFunc
}

func privateDirs(root string) error {
	for _, name := range []string{"plugins", "logs", "data/runtime", "data/auth", "data/tls", "data/plugins", "data/rule_config", "data/rule_drafts"} {
		if e := os.MkdirAll(filepath.Join(root, name), 0700); e != nil {
			return e
		}
	}
	return nil
}
func Load(root string) (*App, error) {
	return load(root, config.WriteAtomic)
}
func load(root string, write func(string, []byte) error) (*App, error) {
	root, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	if e = privateDirs(root); e != nil {
		return nil, e
	}
	cfg, e := config.Read(filepath.Join(root, "config.yaml"))
	if e != nil {
		if errors.Is(e, os.ErrNotExist) {
			if e = config.Template(filepath.Join(root, "config.yaml")); e != nil {
				return nil, e
			}
			return nil, fmt.Errorf("template created; fill required settings")
		}
		return nil, e
	}
	lock, e := run.AcquireLock(filepath.Join(root, "data/runtime/core.lock"))
	if e != nil {
		return nil, e
	}
	success := false
	defer func() {
		if !success {
			lock.Close()
		}
	}()
	cache := filepath.Join(root, "data/media_cache")
	if info, e := os.Lstat(cache); e == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("cache directory must not be symlink")
	}
	if e = os.RemoveAll(cache); e != nil {
		return nil, e
	}
	log, e := logging.New(filepath.Join(root, "logs"), cfg.LogLevel)
	if e != nil {
		fmt.Fprintln(os.Stderr, e)
		log = logging.Fallback(filepath.Join(root, "logs"), cfg.LogLevel)
	}
	transport, e := onebot.New(cfg.Transport())
	if e != nil {
		return nil, e
	}
	mc := cfg.Media
	resources, e := media.New(media.Config{Directory: cache, Enabled: mc.Enabled, Limit: mc.ResourceBytes, Capacity: mc.CapacityBytes, Concurrent: mc.Downloads, Timeout: time.Duration(mc.DownloadSeconds) * time.Second, TTL: time.Duration(mc.TTLSeconds) * time.Second, Scan: time.Duration(mc.ScanSeconds) * time.Second})
	if e != nil {
		return nil, e
	}
	a := &App{Root: root, cfg: cfg, formal: cfg, router: router.New(), log: log, stop: make(chan struct{}), lock: lock, installed: map[string]bool{}, writeConfig: write}
	a.dispatch = dispatch.New(a.routeEvent)
	a.initializing.Store(true)
	a.shared = botapi.NewShared(transport, resources, run.NewOperations(), log.Write)
	ids := []string{}
	for _, p := range cfg.Plugins {
		ids = append(ids, p.ID)
	}
	a.rules, e = config.OpenRules(root, ids)
	if e != nil {
		resources.Close()
		return nil, e
	}
	candidates := plugins.Discover(filepath.Join(root, "plugins"))
	known := map[string]bool{}
	var max int64
	for _, p := range cfg.Plugins {
		known[p.ID] = true
		if p.Priority > max {
			max = p.Priority
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Descriptor.ID < candidates[j].Descriptor.ID })
	for _, c := range candidates {
		id := c.Descriptor.ID
		if !plugins.ValidID(id) {
			continue
		}
		a.installed[id] = true
		if known[id] {
			continue
		}
		known[id] = true
		if max == math.MaxInt64 {
			log.Write("ERROR", id, "registration disabled: priority overflow")
			continue
		}
		if e = a.rules.Register(id); e != nil {
			log.Write("ERROR", id, "initial Rule persistence failed: "+e.Error())
			continue
		}
		max++
		cfg.Plugins = append(cfg.Plugins, config.Plugin{ID: id, Priority: max, Enabled: true})
		data, _ := config.Encode(cfg)
		if e = a.writeConfig(filepath.Join(root, "config.yaml"), data); e != nil {
			cfg.Plugins = cfg.Plugins[:len(cfg.Plugins)-1]
			log.Write("ERROR", id, "registration persistence failed: "+e.Error())
		}
	}
	a.cfg = cfg
	a.formal = cfg
	_, _, problems := a.rules.Snapshot()
	var selections []plugins.Selection
	for _, p := range cfg.Plugins {
		_, bad := problems[p.ID]
		selections = append(selections, plugins.Selection{ID: p.ID, Priority: p.Priority, Enabled: p.Enabled && !bad})
	}
	a.manager, e = plugins.New(candidates, selections, func(id string) sdk.Host {
		dir := filepath.Join(root, "data/plugins", id)
		if e := os.MkdirAll(dir, 0700); e != nil {
			log.Write("ERROR", id, e.Error())
		}
		return &botapi.Host{Shared: a.shared, ID: id, Directory: dir}
	}, time.Duration(cfg.CloseSeconds)*time.Second, func(id string, e error) { log.Write("ERROR", id, e.Error()) })
	if e != nil {
		resources.Close()
		return nil, e
	}
	success = true
	return a, nil
}
func (a *App) entries(files map[string]config.RuleFile) []router.Entry {
	out := []router.Entry{}
	for _, i := range a.manager.Instances {
		if f, ok := files[i.Descriptor.ID]; ok {
			out = append(out, router.Entry{ID: i.Descriptor.ID, Priority: i.Priority, Rules: f.Rules, Deliver: i.Deliver, NeedsQuotedMessage: i.NeedsQuotedMessage})
		}
	}
	return out
}
func (a *App) Stop(restart bool) {
	a.stopOnce.Do(func() {
		a.shutdown, a.shutdownCancel = context.WithTimeout(context.Background(), time.Duration(a.cfg.CloseSeconds)*time.Second)
		a.restart.Store(restart)
		a.closing.Store(true)
		a.router.Stop()
		if a.dispatch != nil {
			a.dispatch.Stop()
		}
		a.shared.Client.StopReconnect()
		a.shared.Operations.StartClosing()
		close(a.stop)
		go a.shared.Closing()
	})
}
func (a *App) Run(ctx context.Context, ready func() error) int {
	defer a.lock.Close()
	a.startWeb()
	initDone := make(chan struct{})
	initCtx, cancelInit := context.WithCancel(context.Background())
	defer cancelInit()
	go func() { a.manager.Init(initCtx); close(initDone) }()
	initialized := false
	select {
	case <-ctx.Done():
		a.Stop(false)
	case <-a.stop:
	case <-initDone:
		initialized = true
	}
	eventCtx, cancelEvents := context.WithCancel(context.Background())
	defer cancelEvents()
	if initialized && !a.closing.Load() {
		_, files, _ := a.rules.Snapshot()
		if e := a.router.Update(a.entries(files)); e != nil {
			a.log.Write("ERROR", "core", e.Error())
		}
		a.initializing.Store(false)
		if e := ready(); e != nil {
			a.log.Write("ERROR", "core", "ready budget failure: "+e.Error())
			a.Stop(false)
		} else {
			go a.shared.Client.Events(eventCtx, a.event, func(e error) { a.log.Write("WARN", "protocol", e.Error()) })
		}
	}
	select {
	case <-ctx.Done():
		a.Stop(false)
	case <-a.stop:
	}
	cancelInit()
	closingCtx := a.shutdown
	defer a.shutdownCancel()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		a.manager.Close(closingCtx)
		if a.dispatch != nil {
			if e := a.dispatch.Wait(closingCtx); e != nil {
				a.log.Write("WARN", "core", "dispatch shutdown deadline")
			}
		}
		if e := a.shared.Operations.Finish(closingCtx); e != nil {
			a.log.Write("WARN", "core", "shutdown deadline: "+e.Error())
		}
		a.shared.Media.Close()
		cancelEvents()
		a.shared.Client.Close()
		if a.web != nil {
			a.web.Close(closingCtx)
		}
		a.log.Close()
	}()
	select {
	case <-finished:
	case <-closingCtx.Done():
	}
	if a.restart.Load() {
		return 75
	}
	return 0
}
func (a *App) event(event onebot.Event) {
	if a.closing.Load() {
		return
	}
	tree, e := strictjson.Parse(event.Data)
	if e == nil {
		kind, system, err := message.System(tree)
		if system {
			if err != nil {
				a.log.Write("WARN", "protocol", err.Error())
			}
			_ = kind
			return
		}
	}
	m, e := message.Decode(event.Data, event.ReceivedAt)
	if e != nil {
		var failure *message.Failure
		if errors.As(e, &failure) {
			a.log.Write("WARN", "protocol", failure.Error()+" Text="+strings.Join(failure.Text, ""))
		} else {
			a.log.Write("WARN", "protocol", e.Error())
		}
		return
	}
	if !a.cfg.Connection.SelfMessages && m.BotID.State == sdk.Present && m.Sender.ID == m.BotID.Value {
		return
	}
	var body strings.Builder
	for _, s := range m.Segments {
		if s.Kind == sdk.Text {
			body.WriteString(s.Text)
		} else {
			fmt.Fprintf(&body, "[%s]", s.Kind)
		}
	}
	a.log.Write("INFO", "message", fmt.Sprintf("scene=%s target=%d sender=%d message=%d %s", m.Ref.Target.Scene, m.Ref.Target.ID, m.Sender.ID, m.Ref.ID, body.String()))
	if reason := a.dispatch.Submit(dispatch.Event{Message: m, Generation: event.Generation, Weight: message.Weight(m)}); reason != "" {
		a.log.Write("WARN", "core", fmt.Sprintf("scene=%s target=%d message=%d %s", m.Ref.Target.Scene, m.Ref.Target.ID, m.Ref.ID, reason))
	}
}

func (a *App) routeEvent(ctx context.Context, event dispatch.Event) {
	m := a.shared.ResourcesAt(event.Message, event.Generation)
	for _, r := range a.router.RouteWith(ctx, m, func(ctx context.Context, m sdk.Message) sdk.Message {
		enriched := a.shared.EnrichQuotes(ctx, m, event.Generation)
		for _, segment := range enriched.Segments {
			if segment.Kind == sdk.Reply && segment.QuoteStatus != "" && segment.QuoteStatus != sdk.QuoteReady {
				a.log.Write("WARN", "core", fmt.Sprintf("quote status=%s scene=%s target=%d message=%d", segment.QuoteStatus, m.Ref.Target.Scene, m.Ref.Target.ID, m.Ref.ID))
			}
		}
		return enriched
	}) {
		a.log.Write("DEBUG", r.Plugin, fmt.Sprintf("rule=%d %s %s", r.Rule, r.Event, r.Reason))
	}
}
func (a *App) startWeb() {
	identity, temp, e := admin.InitIdentity(a.Root, a.cfg.Web.InitialUsername)
	if e != nil {
		a.log.Write("ERROR", "web", e.Error())
		return
	}
	if temp != "" {
		fmt.Fprintln(os.Stderr, "Initial Core username:", a.cfg.Web.InitialUsername, "temporary password:", temp)
	}
	cert, key, e := admin.Certificate(a.Root, a.cfg.Web.PublicIP, false)
	if e != nil {
		a.log.Write("ERROR", "web", e.Error())
		return
	}
	a.web = admin.NewServer(identity, a.backend(), a.cfg.Web.Port, a.cfg.Web.PublicIP)
	done, e := a.web.Start(cert, key)
	if e != nil {
		a.log.Write("ERROR", "web", e.Error())
		a.web = nil
		return
	}
	go func() {
		a.webStopped(<-done)
	}()
}

func (a *App) webStopped(err error) {
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		a.log.Write("ERROR", "web", err.Error())
	}
}
