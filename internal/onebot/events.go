package onebot

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/coder/websocket"
	"net/http"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type Event struct {
	Data       []byte
	ReceivedAt time.Time
	Generation uint64
}

// Events owns the one universal socket. Only business events enter the bounded
// router buffer; API replies and Ping/Pong always progress independently.
func (c *Client) Events(ctx context.Context, deliver func(Event), report func(error)) {
	if !c.running.CompareAndSwap(false, true) {
		report(&sdk.Error{Kind: sdk.Busy, Operation: "connect", Reason: "connection loop already running"})
		return
	}
	defer c.running.Store(false)
	lifetime, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-c.closed:
			cancel()
		case <-lifetime.Done():
		}
	}()
	queue := make(chan Event, c.cfg.Buffer)
	consumerDone := make(chan struct{})
	go func() {
		defer close(consumerDone)
		for {
			select {
			case <-lifetime.Done():
				return
			case e, ok := <-queue:
				if !ok {
					return
				}
				if lifetime.Err() == nil {
					deliver(e)
				}
			}
		}
	}()
	defer func() { close(queue); <-consumerDone }()
	delay := 10 * time.Second
	for lifetime.Err() == nil && !c.reconnectStopped.Load() {
		c.mu.Lock()
		if c.State() == sdk.Closing {
			c.mu.Unlock()
			return
		}
		c.state.Store(sdk.Connecting)
		c.mu.Unlock()
		dialCtx, dialCancel := context.WithTimeout(lifetime, c.cfg.ConnectTimeout)
		conn, res, err := websocket.Dial(dialCtx, c.cfg.WS, &websocket.DialOptions{
			HTTPClient:      &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
			HTTPHeader:      http.Header{"Authorization": []string{"Bearer " + c.cfg.Token}},
			CompressionMode: websocket.CompressionDisabled,
		})
		dialCancel()
		if err == nil {
			s := &session{conn: conn, pending: make(map[string]chan response)}
			c.mu.Lock()
			stop := c.reconnectStopped.Load() || c.State() == sdk.Closing || lifetime.Err() != nil
			if !stop {
				s.generation = c.generation.Add(1)
				c.session = s
				c.state.Store(sdk.Connected)
			}
			c.mu.Unlock()
			if stop {
				conn.CloseNow()
				return
			}
			delay = 10 * time.Second
			conn.SetReadLimit(max(c.cfg.EventLimit, c.cfg.ResponseLimit))
			err = c.readConnection(lifetime, s, queue, report)
			c.detach(s)
		} else {
			c.mu.Lock()
			if c.State() != sdk.Closing {
				c.state.Store(sdk.Disconnected)
			}
			c.mu.Unlock()
		}
		if lifetime.Err() != nil || c.reconnectStopped.Load() {
			return
		}
		if err != nil {
			reason := "universal WebSocket disconnected"
			if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
				reason = fmt.Sprintf("WebSocket handshake HTTP %d", res.StatusCode)
			}
			// Do not include upstream URL or credentials in routine logs.
			report(&sdk.Error{Kind: sdk.NetworkFailure, Operation: "connect", Reason: reason})
		}
		if !c.waitRetry(lifetime, delay) {
			return
		}
		delay = min(time.Hour, delay+10*time.Second)
	}
}
func (c *Client) waitRetry(ctx context.Context, delay time.Duration) bool {
	if c.retryWait != nil {
		return c.retryWait(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-c.stopReconnect:
		return false
	case <-timer.C:
		return true
	}
}

func (c *Client) readConnection(ctx context.Context, s *session, queue chan<- Event, report func(error)) error {
	readCtx, cancel := context.WithCancel(ctx)
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		tick := time.NewTicker(c.pingEvery)
		defer tick.Stop()
		for {
			select {
			case <-readCtx.Done():
				return
			case <-tick.C:
				ping, done := context.WithTimeout(readCtx, c.pongWithin)
				err := s.conn.Ping(ping)
				done()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	defer func() { cancel(); <-pingDone }()
	for {
		typ, data, err := s.conn.Read(readCtx)
		if err != nil {
			return err
		}
		received := time.Now().UTC()
		if typ != websocket.MessageText {
			report(&sdk.Error{Kind: sdk.ProtocolFailure, Reason: "binary OneBot frame rejected"})
			continue
		}
		// Discrimination is not normalization. Business data retains its strict boundary.
		var envelope map[string]json.RawMessage
		if err = json.Unmarshal(data, &envelope); err != nil || envelope == nil {
			report(&sdk.Error{Kind: sdk.ProtocolFailure, Reason: "invalid OneBot JSON envelope"})
			continue
		}
		_, event := envelope["post_type"]
		_, echo := envelope["echo"]
		_, status := envelope["status"]
		_, retcode := envelope["retcode"]
		if !event && (echo || status || retcode) {
			if int64(len(data)) > c.cfg.ResponseLimit {
				return fmt.Errorf("API response exceeds configured limit")
			}
			c.reply(s, data, report)
			continue
		}
		if int64(len(data)) > c.cfg.EventLimit {
			report(&sdk.Error{Kind: sdk.ProtocolFailure, Operation: "events", Reason: fmt.Sprintf("event exceeds configured byte limit: received=%d limit=%d", len(data), c.cfg.EventLimit)})
			continue
		}
		select {
		case queue <- Event{Data: data, ReceivedAt: received, Generation: s.generation}:
		default:
			report(&sdk.Error{Kind: sdk.Busy, Operation: "events", Reason: "raw event buffer full; newest event dropped"})
		}
	}
}

func (c *Client) reply(s *session, data []byte, report func(error)) {
	v, err := strictjson.Parse(data)
	var echo string
	if err == nil {
		fields, e := v.Fields("status", "retcode", "data", "message", "wording", "echo", "stream")
		err = e
		count := 0
		if v.Kind == strictjson.Object {
			for _, p := range v.Object {
				if p.Key == "echo" {
					count++
					if p.Value.Kind == strictjson.String {
						echo = p.Value.Text
					}
				}
			}
		}
		if count != 1 {
			echo = ""
		}
		if err == nil && (fields["echo"] == nil || fields["echo"].Kind != strictjson.String || echo == "") {
			err = fmt.Errorf("missing/invalid response echo")
		}
	}
	if echo == "" {
		if v != nil {
			if code, e := integer(v.Get("retcode"), false); e == nil && (code == 1401 || code == 1403) {
				report(&sdk.Error{Kind: sdk.ProtocolFailure, Operation: "connect", Code: code, Reason: "OneBot authorization rejected"})
				return
			}
		}
		report(&sdk.Error{Kind: sdk.ProtocolFailure, Operation: "response", Reason: "uncorrelated response rejected"})
		return
	}
	c.mu.Lock()
	ch := s.pending[echo]
	delete(s.pending, echo)
	c.mu.Unlock()
	// Late, duplicate and old-generation replies never become business events.
	if ch == nil {
		return
	}
	r := response{data: v}
	if err != nil {
		r.err = &sdk.Error{Kind: sdk.ProtocolFailure, Reason: "invalid response envelope"}
	}
	ch <- r
}
