// Package plugins owns discovery and lifecycle, not plugin business queues.
package plugins

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	"time"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type Candidate struct {
	File       string
	Descriptor sdk.Descriptor
	New        func() sdk.Plugin
	Problem    error
}
type Selection struct {
	ID       string
	Priority int64
	Enabled  bool
}
type Instance struct {
	started            atomic.Bool
	Descriptor         sdk.Descriptor
	Priority           int64
	NeedsQuotedMessage bool
	plugin             sdk.Plugin
	host               sdk.Host
	initDone           chan struct{}
	initialized        atomic.Bool
	failed             atomic.Bool
	closing            atomic.Bool
	deliveryMu         sync.Mutex
	closeOnce          sync.Once
	closeDone          chan struct{}
	log                func(string, error)
}

func (i *Instance) State() string {
	if i.failed.Load() {
		return "initialization_failed"
	}
	if i.initialized.Load() {
		return "ready"
	}
	return "initializing"
}

type Manager struct {
	startMu       sync.Mutex
	cleanupMu     sync.Mutex
	cleanupCtx    context.Context
	cleanupCancel map[*Instance]context.CancelFunc
	Instances     []*Instance
	initCtx       context.Context
	initCancel    context.CancelFunc
	cleanup       time.Duration
	log           func(string, error)
}

func ValidID(id string) bool {
	if len(id) < 1 || len(id) > 127 || id[0] < 'a' || id[0] > 'z' {
		return false
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
func Discover(dir string) []Candidate {
	files, _ := filepath.Glob(filepath.Join(dir, "*.so"))
	sort.Strings(files)
	result := make([]Candidate, 0, len(files))
	for _, f := range files {
		result = append(result, load(f))
	}
	counts := map[string]int{}
	for _, c := range result {
		if ValidID(c.Descriptor.ID) {
			counts[c.Descriptor.ID]++
		}
	}
	for i := range result {
		if counts[result[i].Descriptor.ID] > 1 {
			result[i].Problem = fmt.Errorf("all duplicate plugin IDs are disabled")
		}
	}
	return result
}
func New(candidates []Candidate, selections []Selection, host func(string) sdk.Host, cleanup time.Duration, log func(string, error)) (*Manager, error) {
	if cleanup <= 0 {
		return nil, fmt.Errorf("invalid cleanup deadline")
	}
	m := &Manager{cleanup: cleanup, log: log, cleanupCancel: map[*Instance]context.CancelFunc{}}
	m.initCtx, m.initCancel = context.WithCancel(context.Background())
	byID := map[string]Selection{}
	priorities := map[int64]int{}
	for _, s := range selections {
		if _, ok := byID[s.ID]; ok {
			return nil, fmt.Errorf("duplicate config ID")
		}
		byID[s.ID] = s
		priorities[s.Priority]++
	}
	for _, s := range selections {
		if priorities[s.Priority] > 1 {
			log(s.ID, fmt.Errorf("priority %d conflicts with another configured ID; disabled", s.Priority))
		}
	}
	for _, c := range candidates {
		if c.Problem != nil {
			identity := c.File
			if ValidID(c.Descriptor.ID) {
				identity = c.Descriptor.ID
			}
			log(identity, c.Problem)
			continue
		}
		sel, exists := byID[c.Descriptor.ID]
		if !exists || !sel.Enabled || sel.Priority < 1 || priorities[sel.Priority] > 1 {
			continue
		}
		p, e := construct(c.New)
		if e != nil {
			log(c.Descriptor.ID, e)
			continue
		}
		m.Instances = append(m.Instances, &Instance{Descriptor: c.Descriptor, Priority: sel.Priority, plugin: p, host: host(c.Descriptor.ID), initDone: make(chan struct{}), closeDone: make(chan struct{}), log: log})
	}
	sort.Slice(m.Instances, func(i, j int) bool { return m.Instances[i].Priority < m.Instances[j].Priority })
	return m, nil
}
func construct(factory func() sdk.Plugin) (p sdk.Plugin, err error) {
	defer func() {
		if x := recover(); x != nil {
			err = fmt.Errorf("New panic: %v", x)
		}
	}()
	p = factory()
	if p == nil {
		return nil, fmt.Errorf("New returned nil")
	}
	value := reflect.ValueOf(p)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Chan, reflect.Func, reflect.Interface:
		if value.IsNil() {
			return nil, fmt.Errorf("New returned typed nil")
		}
	}
	return p, nil
}
func call(fn func() error) (err error) {
	defer func() {
		if x := recover(); x != nil {
			err = fmt.Errorf("plugin panic: %v", x)
		}
	}()
	return fn()
}

// Init is serial; failed cleanup runs in parallel and does not delay later Init or readiness.
func (m *Manager) Init(ctx context.Context) {
	context.AfterFunc(ctx, m.initCancel)
	initCtx := m.initCtx
	for _, i := range m.Instances {
		m.startMu.Lock()
		if initCtx.Err() != nil {
			m.startMu.Unlock()
			break
		}
		i.started.Store(true)
		m.startMu.Unlock()
		err := call(func() error {
			if e := i.plugin.Init(initCtx, i.host); e != nil {
				return e
			}
			if consumer, ok := i.plugin.(sdk.QuotedMessageConsumer); ok {
				i.NeedsQuotedMessage = consumer.NeedsQuotedMessage()
			}
			return nil
		})
		if err != nil {
			i.failed.Store(true)
			i.log(i.Descriptor.ID, err)
		} else {
			i.initialized.Store(true)
		}
		close(i.initDone)
		if err != nil {
			go func(i *Instance) {
				cleanup, cancel := context.WithTimeout(context.Background(), m.cleanup)
				defer cancel()
				m.cleanupMu.Lock()
				m.cleanupCancel[i] = cancel
				closing := m.cleanupCtx
				m.cleanupMu.Unlock()
				if closing != nil {
					context.AfterFunc(closing, cancel)
				}
				i.close(cleanup)
				m.cleanupMu.Lock()
				delete(m.cleanupCancel, i)
				m.cleanupMu.Unlock()
			}(i)
		}
	}
}
func (i *Instance) Deliver(msg sdk.Message) sdk.DeliveryResult {
	i.deliveryMu.Lock()
	defer i.deliveryMu.Unlock()
	if !i.initialized.Load() || i.failed.Load() || i.closing.Load() {
		return sdk.DeliveryResult{Reason: "plugin not eligible"}
	}
	return i.plugin.Deliver(msg)
}
func (i *Instance) close(ctx context.Context) {
	i.closeOnce.Do(func() {
		defer close(i.closeDone)
		i.closing.Store(true)
		// Instance owns callback lifecycle: Close cannot overlap a short Deliver.
		// Manager still bounds waiting by the shared shutdown deadline.
		i.deliveryMu.Lock()
		defer i.deliveryMu.Unlock()
		if e := call(func() error { return i.plugin.Close(ctx) }); e != nil {
			i.log(i.Descriptor.ID, e)
		}
	})
}

// Close never overlaps an unfinished Init on the same instance.
func (m *Manager) Close(ctx context.Context) {
	if m.initCancel != nil {
		m.initCancel()
	}
	m.cleanupMu.Lock()
	if m.cleanupCtx == nil {
		m.cleanupCtx = ctx
	}
	for _, cancel := range m.cleanupCancel {
		context.AfterFunc(ctx, cancel)
	}
	m.cleanupMu.Unlock()
	m.startMu.Lock()
	defer m.startMu.Unlock()
	var wg sync.WaitGroup
	for _, i := range m.Instances {
		if !i.started.Load() {
			continue
		}
		wg.Add(1)
		go func(i *Instance) {
			defer wg.Done()
			select {
			case <-i.initDone:
			case <-ctx.Done():
				return
			}
			done := make(chan struct{})
			go func() { i.close(ctx); close(done) }()
			select {
			case <-done:
			case <-ctx.Done():
			}
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
}
