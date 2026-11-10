package runtime

import (
	"context"
	"sync"
	"time"
	"github.com/ZhenForest/xsbot-core/sdk"
)

// Operations registers the entire SDK operation, including local preparation.
type Operations struct {
	mu      sync.Mutex
	closing bool
	sealed  bool
	active  int
	changed chan struct{}
	ctx     context.Context
	cancel  context.CancelFunc
}

func NewOperations() *Operations {
	ctx, cancel := context.WithCancel(context.Background())
	return &Operations{changed: make(chan struct{}), ctx: ctx, cancel: cancel}
}
func (o *Operations) signal() { close(o.changed); o.changed = make(chan struct{}) }
func (o *Operations) Begin(ctx context.Context, media bool) (context.Context, func(), error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sealed || media && o.closing {
		return nil, nil, &sdk.Error{Kind: sdk.ShuttingDown, Reason: "operation admission closed"}
	}
	o.active++
	o.signal()
	combined, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.ctx, cancel)
	var once sync.Once
	done := func() {
		once.Do(func() {
			stop()
			cancel()
			o.mu.Lock()
			o.active--
			o.signal()
			o.mu.Unlock()
		})
	}
	return combined, done, nil
}
func (o *Operations) StartClosing() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if !o.closing {
		o.closing = true
		o.signal()
	}
}
func (o *Operations) IsClosing() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.closing || o.sealed }
func (o *Operations) CheckNetworkMedia() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closing || o.sealed {
		return &sdk.Error{Kind: sdk.ShuttingDown, Reason: "network media admission closed"}
	}
	return nil
}

// Finish is called after all Close callbacks have ended; admission and zero-test are atomic.
func (o *Operations) Finish(ctx context.Context) error {
	for {
		o.mu.Lock()
		o.sealed = true
		if o.active == 0 {
			o.mu.Unlock()
			o.cancel()
			return nil
		}
		changed := o.changed
		o.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			o.cancel()
			return ctx.Err()
		}
	}
}
func (o *Operations) Deadline(d time.Duration) (context.Context, context.CancelFunc) {
	o.StartClosing()
	return context.WithTimeout(context.Background(), d)
}
