// Package dispatch owns Core admission and conversation order, not plugin work.
package dispatch

import (
	"context"
	"sync"
	"github.com/ZhenForest/xsbot-core/sdk"
)

const (
	MaxPending             = 128
	MaxConversations       = 32
	PerConversation        = 16
	MaxBytes         int64 = 16 << 20
)

type Event struct {
	Message    sdk.Message
	Generation uint64
	Weight     int64
}
type conversation struct{ jobs []Event }
type Queue struct {
	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	groups  map[sdk.Target]*conversation
	handle  func(context.Context, Event)
	pending int
	bytes   int64
	closing bool
	wg      sync.WaitGroup
	done    chan struct{}
}

func New(handle func(context.Context, Event)) *Queue {
	ctx, cancel := context.WithCancel(context.Background())
	return &Queue{ctx: ctx, cancel: cancel, handle: handle, groups: map[sdk.Target]*conversation{}, done: make(chan struct{})}
}

// Submit does not wait. Each active conversation has one bounded runner, so
// slow I/O in other conversations cannot occupy its execution worker.
func (q *Queue) Submit(event Event) string {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closing {
		return "core dispatch closing"
	}
	key := event.Message.Ref.Target
	g := q.groups[key]
	if event.Weight <= 0 || event.Weight > MaxBytes-q.bytes {
		return "core dispatch byte budget exceeded"
	}
	if q.pending >= MaxPending {
		return "core dispatch queue full"
	}
	if g == nil && len(q.groups) >= MaxConversations {
		return "too many core conversations"
	}
	if g != nil && len(g.jobs) >= PerConversation {
		return "core conversation queue full"
	}
	if g == nil {
		g = &conversation{}
		q.groups[key] = g
		q.wg.Add(1)
		defer func() { go q.run(key, g) }()
	}
	g.jobs = append(g.jobs, event)
	q.pending++
	q.bytes += event.Weight
	return ""
}
func (q *Queue) run(key sdk.Target, g *conversation) {
	defer q.wg.Done()
	for {
		q.mu.Lock()
		if q.closing || len(g.jobs) == 0 {
			for _, event := range g.jobs {
				q.pending--
				q.bytes -= event.Weight
			}
			g.jobs = nil
			delete(q.groups, key)
			q.mu.Unlock()
			return
		}
		event := g.jobs[0]
		q.mu.Unlock()
		if q.ctx.Err() == nil {
			q.handle(q.ctx, event)
		}
		q.mu.Lock()
		g.jobs[0] = Event{}
		g.jobs = g.jobs[1:]
		q.pending--
		q.bytes -= event.Weight
		q.mu.Unlock()
	}
}
func (q *Queue) Stop() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closing {
		return
	}
	q.closing = true
	q.cancel()
	// Seal admission and WaitGroup.Add before waiting.
	go func() { q.wg.Wait(); close(q.done) }()
}
func (q *Queue) Wait(ctx context.Context) error {
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
