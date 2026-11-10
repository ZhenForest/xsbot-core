// Package media implements bounded, generation-scoped disk resources with explicit handles.
package media

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"time"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type Config struct {
	Directory          string
	Enabled            bool
	Limit, Capacity    int64
	Concurrent         int
	Timeout, TTL, Scan time.Duration
}
type Open func(context.Context) (io.ReadCloser, error)
type item struct {
	key, path        string
	size             int64
	ready            chan struct{}
	cancel           context.CancelFunc
	err              error
	loading, revoked bool
	network          bool
	cleaning         bool
	waiters, holds   int
	last             time.Time
	readers          map[*reader]bool
}
type Manager struct {
	mu      sync.Mutex
	cfg     Config
	items   map[string]*item
	orphans map[*item]bool
	used    int64
	active  int
	closing bool
	closed  bool
	stop    chan struct{}
	stopped sync.Once
}

func New(cfg Config) (*Manager, error) {
	if cfg.Limit <= 0 || cfg.Capacity < cfg.Limit || cfg.Concurrent < 1 || cfg.Timeout <= 0 || cfg.TTL <= 0 || cfg.Scan <= 0 {
		return nil, fmt.Errorf("invalid media limits")
	}
	if err := os.MkdirAll(cfg.Directory, 0700); err != nil {
		return nil, err
	}
	m := &Manager{cfg: cfg, items: map[string]*item{}, orphans: map[*item]bool{}, stop: make(chan struct{})}
	go m.sweeper()
	return m, nil
}
func fault(k sdk.ErrorKind, reason string) error {
	return &sdk.Error{Kind: k, Operation: "media", Reason: reason}
}
func (m *Manager) Acquire(ctx context.Context, key string, open Open, network bool) (sdk.MediaHandle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return nil, fault(sdk.ShuttingDown, "media manager closed")
	}
	if network && (!m.cfg.Enabled || m.closing) {
		m.mu.Unlock()
		if m.closing {
			return nil, fault(sdk.ShuttingDown, "network media closed")
		}
		return nil, fault(sdk.MediaDisabled, "media disabled")
	}
	if !network && m.closing { // local preparation remains admissible while plugin Close runs.
	}
	if it := m.items[key]; it != nil && !it.loading && !it.revoked {
		if time.Since(it.last) > m.cfg.TTL && it.holds == 0 {
			m.evict(it)
		} else {
			h := m.handle(it)
			m.mu.Unlock()
			return h, nil
		}
	}
	it := m.items[key]
	if it != nil && it.loading {
		it.waiters++
		m.mu.Unlock()
		return m.wait(ctx, it)
	}
	if network && m.active >= m.cfg.Concurrent {
		m.mu.Unlock()
		return nil, fault(sdk.Busy, "different-resource limit reached")
	}
	if !m.space(m.cfg.Limit) {
		m.mu.Unlock()
		return nil, fault(sdk.NoSpace, "cache budget unavailable")
	}
	free, e := available(m.cfg.Directory)
	if e != nil {
		m.mu.Unlock()
		return nil, e
	}
	for free < uint64(m.cfg.Limit) {
		var victim *item
		for _, candidate := range m.items {
			if !candidate.loading && !candidate.revoked && (victim == nil || candidate.last.Before(victim.last)) {
				victim = candidate
			}
		}
		if victim == nil {
			m.mu.Unlock()
			return nil, fault(sdk.NoSpace, "filesystem space unavailable")
		}
		m.evict(victim)
		free, e = available(m.cfg.Directory)
		if e != nil {
			m.mu.Unlock()
			return nil, e
		}
	}
	file, err := os.CreateTemp(m.cfg.Directory, "resource-*")
	if err != nil {
		m.mu.Unlock()
		return nil, err
	}
	name := file.Name()
	file.Close()
	task, cancel := context.WithTimeout(context.Background(), m.cfg.Timeout)
	it = &item{key: key, path: name, ready: make(chan struct{}), cancel: cancel, loading: true, network: network, waiters: 1, readers: map[*reader]bool{}}
	m.items[key] = it
	m.used += m.cfg.Limit
	if network {
		m.active++
	}
	m.mu.Unlock()
	go m.download(task, it, open, network)
	return m.wait(ctx, it)
}
func (m *Manager) wait(ctx context.Context, it *item) (sdk.MediaHandle, error) {
	select {
	case <-ctx.Done():
		m.mu.Lock()
		it.waiters--
		if it.loading && it.waiters == 0 {
			it.cancel()
			if m.items[it.key] == it {
				delete(m.items, it.key)
			}
		}
		m.mu.Unlock()
		return nil, ctx.Err()
	case <-it.ready:
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	it.waiters--
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if it.err != nil {
		return nil, it.err
	}
	if it.revoked {
		return nil, fault(sdk.Revoked, "resource revoked")
	}
	return m.handle(it), nil
}
func (m *Manager) download(ctx context.Context, it *item, open Open, network bool) {
	source, err := open(ctx)
	if err == nil {
		closeSource := context.AfterFunc(ctx, func() { source.Close() })
		file, e := os.OpenFile(it.path, os.O_WRONLY|os.O_TRUNC, 0600)
		if e != nil {
			err = e
		} else {
			n, e := io.Copy(file, io.LimitReader(source, m.cfg.Limit+1))
			it.size = n
			err = e
			if n > m.cfg.Limit {
				err = fault(sdk.TooLarge, "actual bytes exceed limit")
			}
			if err == nil {
				err = file.Sync()
			}
			if e = file.Close(); err == nil {
				err = e
			}
		}
		closeSource()
		source.Close()
	}
	if err == nil {
		err = ctx.Err()
	}
	it.cancel()
	m.mu.Lock()
	defer m.mu.Unlock()
	if network {
		m.active--
	}
	it.loading = false
	m.used -= m.cfg.Limit
	if err != nil || it.waiters == 0 || m.items[it.key] != it {
		if err == nil {
			err = context.Canceled
		}
		it.err = err
		m.used += it.size
		m.evict(it)
	} else {
		m.used += it.size
		it.last = time.Now()
	}
	close(it.ready)
}
func (m *Manager) handle(it *item) *handle {
	it.holds++
	it.last = time.Now()
	return &handle{manager: m, item: it, readers: map[*reader]bool{}}
}
func (m *Manager) space(need int64) bool {
	for m.used+need > m.cfg.Capacity {
		var candidate *item
		for _, it := range m.items {
			if it.loading || it.revoked {
				continue
			}
			if candidate == nil || (it.holds == 0 && candidate.holds != 0) || (it.holds == candidate.holds && it.last.Before(candidate.last)) || (it.holds > 0 && candidate.holds > 0 && it.last.Before(candidate.last)) {
				candidate = it
			}
		}
		if candidate == nil {
			return false
		}
		m.evict(candidate)
	}
	return true
}
func (m *Manager) evict(it *item) {
	if it.cleaning || it.path == "" {
		return
	}
	it.cleaning = true
	it.revoked = true
	var readers []*reader
	for r := range it.readers {
		r.closed = true
		readers = append(readers, r)
	}
	it.readers = map[*reader]bool{}
	if m.items[it.key] == it {
		delete(m.items, it.key)
	}
	path := it.path
	// Mark first, then perform potentially blocking filesystem work without the
	// global lock. Bytes remain charged until unlink succeeds.
	m.mu.Unlock()
	for _, r := range readers {
		r.file.Close()
	}
	e := os.Remove(path)
	m.mu.Lock()
	it.cleaning = false
	if e == nil || os.IsNotExist(e) {
		m.used -= it.size
		it.path = ""
		delete(m.orphans, it)
	} else {
		m.orphans[it] = true
	}
}
func (m *Manager) retryOrphans() {
	for it := range m.orphans {
		m.evict(it)
	}
}
func (m *Manager) sweeper() {
	tick := time.NewTicker(m.cfg.Scan)
	defer tick.Stop()
	for {
		select {
		case <-m.stop:
			return
		case <-tick.C:
			m.mu.Lock()
			m.retryOrphans()
			for _, it := range m.items {
				if !it.loading && it.holds == 0 && time.Since(it.last) > m.cfg.TTL {
					m.evict(it)
				}
			}
			m.mu.Unlock()
		}
	}
}
func (m *Manager) StartClosing() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closing = true
	for _, it := range m.items {
		if it.loading && it.network {
			it.cancel()
		}
	}
}
func (m *Manager) Close() {
	m.StartClosing()
	m.stopped.Do(func() { close(m.stop) })
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	m.retryOrphans()
	for _, it := range m.items {
		if !it.loading {
			m.evict(it)
		} else {
			it.cancel()
		}
	}
}

type handle struct {
	manager  *Manager
	item     *item
	released bool
	readers  map[*reader]bool
}

func (h *handle) check() error {
	if h.released {
		return fault(sdk.Released, "handle released")
	}
	if h.item.revoked {
		return fault(sdk.Revoked, "resource revoked")
	}
	return nil
}
func (h *handle) Path() (string, error) {
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	if e := h.check(); e != nil {
		return "", e
	}
	return h.item.path, nil
}
func (h *handle) Reader() (io.ReadCloser, error) {
	h.manager.mu.Lock()
	defer h.manager.mu.Unlock()
	if e := h.check(); e != nil {
		return nil, e
	}
	f, e := os.Open(h.item.path)
	if e != nil {
		return nil, e
	}
	r := &reader{file: f, handle: h}
	h.readers[r] = true
	h.item.readers[r] = true
	return r, nil
}
func (h *handle) Release() error {
	m := h.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if h.released {
		return nil
	}
	h.released = true
	var readers []*reader
	for r := range h.readers {
		r.closed = true
		readers = append(readers, r)
		delete(h.item.readers, r)
	}
	h.readers = map[*reader]bool{}
	h.item.holds--
	if h.item.holds == 0 && !h.item.revoked && time.Since(h.item.last) > m.cfg.TTL {
		m.evict(h.item)
	}
	m.mu.Unlock()
	for _, r := range readers {
		r.file.Close()
	}
	m.mu.Lock()
	return nil
}

type reader struct {
	file   *os.File
	handle *handle
	closed bool
}

func (r *reader) Read(p []byte) (int, error) {
	m := r.handle.manager
	m.mu.Lock()
	e := r.handle.check()
	if e == nil && r.closed {
		e = fault(sdk.Released, "reader closed")
	}
	m.mu.Unlock()
	if e != nil {
		return 0, e
	}
	n, e := r.file.Read(p)
	if e != nil {
		m.mu.Lock()
		revoked := r.handle.item.revoked
		m.mu.Unlock()
		if revoked {
			return n, fault(sdk.Revoked, "reader revoked")
		}
	}
	return n, e
}
func (r *reader) Close() error {
	m := r.handle.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	delete(r.handle.readers, r)
	delete(r.handle.item.readers, r)
	return r.file.Close()
}
func URLKey(raw string) (string, error) {
	u, e := url.Parse(raw)
	if e != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", fault(sdk.Invalid, "invalid media URL")
	}
	u.Fragment = ""
	return "outbound:" + u.String(), nil
}
func HTTPOpen(raw string) Open {
	return func(ctx context.Context) (io.ReadCloser, error) {
		request, e := http.NewRequestWithContext(ctx, "GET", raw, nil)
		if e != nil {
			return nil, e
		}
		client := &http.Client{}
		response, e := client.Do(request)
		if e != nil {
			return nil, e
		}
		if response.StatusCode != 200 {
			response.Body.Close()
			return nil, fault(sdk.ProtocolFailure, "resource HTTP failure")
		}
		return response.Body, nil
	}
}
func RandomKey() string { var b [16]byte; rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func LocalOpen(path string) Open {
	return func(ctx context.Context) (io.ReadCloser, error) {
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		return os.Open(filepath.Clean(path))
	}
}
