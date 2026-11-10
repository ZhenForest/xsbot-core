package app

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"github.com/ZhenForest/xsbot-core/internal/admin"
	"github.com/ZhenForest/xsbot-core/internal/config"
	"github.com/ZhenForest/xsbot-core/internal/plugins"
)

type coreDraft struct {
	Schema int    `yaml:"schema_version"`
	Base   string `yaml:"base_revision"`
	Text   string `yaml:"text"`
}

func configDigest(c config.Config) string {
	b, _ := config.Encode(c)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}
func (a *App) coreBase() string { return configDigest(a.formal) }
func (a *App) backend() admin.Backend {
	return admin.Backend{
		PluginConfiguration: func(ctx context.Context, id string) (any, error) {
			if id == "" {
				return a.manager.ConfigurationPlugins(), nil
			}
			return a.manager.Configuration(ctx, id)
		},
		SavePluginConfiguration:   a.manager.SaveConfiguration,
		PluginConfigurationAction: a.manager.ConfigurationAction,
		Initializing:              func() bool { return a.initializing.Load() || a.closing.Load() }, Logger: a.log, Restart: func() { a.Stop(true) },
		State: func() any {
			a.mu.Lock()
			defer a.mu.Unlock()
			revision, _, problems := a.rules.Snapshot()
			states := map[string]string{}
			for _, p := range a.manager.Instances {
				states[p.Descriptor.ID] = p.State()
			}
			running, formal := a.cfg, a.formal
			running.Connection.Token = ""
			formal.Connection.Token = ""
			return map[string]any{"connection": a.shared.Client.State(), "initializing": a.initializing.Load(), "closing": a.closing.Load(), "plugins": a.formal.Plugins, "installed": a.installed, "plugin_states": states, "rule_revision": revision, "rule_problems": problems, "restart_required": a.coreBase() != configDigest(a.cfg), "running_config": running, "formal_config": formal}
		},
		CoreDraft: func() (any, error) {
			a.mu.Lock()
			defer a.mu.Unlock()
			c := a.formal
			data, e := os.ReadFile(filepath.Join(a.Root, "data/config_draft.yaml"))
			base := a.coreBase()
			if e == nil {
				var d coreDraft
				if e = config.DecodeStrict(data, &d); e != nil {
					return nil, e
				}
				if d.Schema != 1 {
					return nil, fmt.Errorf("invalid Core draft schema")
				}
				c = config.Config{}
				if e = config.DecodeStrict([]byte(d.Text), &c); e != nil {
					return nil, e
				}
				if e = c.Validate(); e != nil {
					return nil, fmt.Errorf("invalid Core draft: %w", e)
				}
				base = d.Base
				if configDigest(c) == a.coreBase() {
					base = a.coreBase()
				}
			} else if !os.IsNotExist(e) {
				return nil, e
			}
			draftDiffers := configDigest(c) != a.coreBase()
			c.Connection.Token = ""
			formal := a.formal
			formal.Connection.Token = ""
			return map[string]any{"config": c, "formal": formal, "base": base, "current_base": a.coreBase(), "draft_differs": draftDiffers}, nil
		},
		RebaseCore: func(from, current string) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			if current != a.coreBase() {
				return fmt.Errorf("formal Core changed; review again")
			}
			path := filepath.Join(a.Root, "data/config_draft.yaml")
			data, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			var d coreDraft
			if e = config.DecodeStrict(data, &d); e != nil || d.Schema != 1 {
				return fmt.Errorf("Core draft damaged; explicit repair required")
			}
			if d.Base != from {
				return fmt.Errorf("Core draft changed; reload before reviewing")
			}
			d.Base = current
			encoded, e := config.Encode(d)
			if e != nil {
				return e
			}
			return config.WriteAtomic(path, encoded)
		},
		SaveCore: func(data []byte) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			var request struct {
				Base   string        `json:"base"`
				Config config.Config `json:"config"`
			}
			if e := config.DecodeWebJSON(data, &request); e != nil {
				return e
			}
			if request.Base != a.coreBase() {
				return fmt.Errorf("stale Core baseline")
			}
			path := filepath.Join(a.Root, "data/config_draft.yaml")
			preservedToken := a.formal.Connection.Token
			if previous, e := os.ReadFile(path); e == nil {
				var prior coreDraft
				if e = config.DecodeStrict(previous, &prior); e != nil {
					return fmt.Errorf("existing Core draft damaged; repair required")
				}
				if prior.Schema != 1 {
					return fmt.Errorf("invalid Core draft schema")
				}
				var priorConfig config.Config
				if config.DecodeStrict([]byte(prior.Text), &priorConfig) != nil || priorConfig.Validate() != nil {
					return fmt.Errorf("existing Core draft damaged; explicit repair required")
				}
				if prior.Base != request.Base {
					var applied config.Config
					if config.DecodeStrict([]byte(prior.Text), &applied) != nil || configDigest(applied) != a.coreBase() {
						return fmt.Errorf("Core draft baseline conflict")
					}
				}
				preservedToken = priorConfig.Connection.Token
			} else if !os.IsNotExist(e) {
				return e
			}
			if request.Config.Connection.Token == "" {
				request.Config.Connection.Token = preservedToken
			}
			if e := request.Config.Validate(); e != nil {
				return e
			}
			present := map[string]bool{}
			for _, p := range request.Config.Plugins {
				present[p.ID] = true
			}
			for _, p := range a.formal.Plugins {
				if a.installed[p.ID] && !present[p.ID] {
					return fmt.Errorf("only missing plugin entries may be removed")
				}
			}
			text, e := config.Encode(request.Config)
			if e != nil {
				return e
			}
			d := coreDraft{Schema: 1, Base: request.Base, Text: string(text)}
			encoded, _ := config.Encode(d)
			return config.WriteAtomic(path, encoded)
		},
		ApplyCore: func() error {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.closing.Load() {
				return fmt.Errorf("closing")
			}
			data, e := os.ReadFile(filepath.Join(a.Root, "data/config_draft.yaml"))
			if e != nil {
				return e
			}
			var d coreDraft
			if e = config.DecodeStrict(data, &d); e != nil {
				return e
			}
			if d.Schema != 1 {
				return fmt.Errorf("invalid Core draft schema")
			}
			if d.Base != a.coreBase() {
				return fmt.Errorf("Core baseline conflict")
			}
			var c config.Config
			if e = config.DecodeStrict([]byte(d.Text), &c); e != nil {
				return e
			}
			if e = c.Validate(); e != nil {
				return e
			}
			seen := map[int64]bool{}
			for _, plugin := range c.Plugins {
				if seen[plugin.Priority] {
					return fmt.Errorf("duplicate formal priority")
				}
				seen[plugin.Priority] = true
			}
			if a.closing.Load() {
				return fmt.Errorf("closing; candidate not committed")
			}
			if e = a.writeConfig(filepath.Join(a.Root, "config.yaml"), []byte(d.Text)); e != nil {
				// Rename can succeed before directory sync fails. Track the actual
				// formal file while still reporting the uncertain durability result.
				if disk, readErr := config.Read(filepath.Join(a.Root, "config.yaml")); readErr == nil && configDigest(disk) == configDigest(c) {
					a.formal = disk
				}
				return e
			}
			a.formal = c
			return nil
		},
		RuleDraft: a.rules.Draft, SaveRule: a.rules.SaveDraft,
		RuleRevision: a.rules.Revision,
		RuleCurrent: func(id string) (any, error) {
			if !plugins.ValidID(id) {
				return nil, fmt.Errorf("invalid plugin ID")
			}
			rev, text, e := a.rules.Current(id)
			problem := ""
			if e != nil {
				problem = e.Error()
			}
			return map[string]any{"revision": rev, "text": text, "problem": problem}, nil
		},
		PreviewRules: a.rules.Preview,
		ApplyRules: func(base, review string) error {
			a.mu.Lock()
			defer a.mu.Unlock()
			if a.closing.Load() {
				return fmt.Errorf("closing")
			}
			return a.rules.CommitReviewed(base, review, func(files map[string]config.RuleFile, persist func() error) error {
				if a.closing.Load() {
					return fmt.Errorf("closing")
				}
				return a.router.Commit(a.entries(files), func() error {
					if a.closing.Load() {
						return fmt.Errorf("closing; candidate not committed")
					}
					return persist()
				})
			})
		},
		RebaseRule: a.rules.Rebase,
		RuleForm: func(id string) (any, error) {
			d, e := a.rules.Draft(id)
			if e != nil {
				return nil, e
			}
			return config.ParseRules([]byte(d.Text), id)
		},
		EncodeRule: func(data []byte) (string, error) {
			var f config.RuleFile
			if e := config.DecodeWebJSON(data, &f); e != nil {
				return "", e
			}
			b, e := config.Encode(f)
			if e != nil {
				return "", e
			}
			return string(b), nil
		},
	}
}
