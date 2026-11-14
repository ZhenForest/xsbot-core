// Package onebot is the private NapCat transport, never exposed as an SDK passthrough.
package onebot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/coder/websocket"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/strictjson"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type Config struct {
	WS             string
	Token          string
	APITimeout     time.Duration
	ConnectTimeout time.Duration
	EventLimit     int64
	ResponseLimit  int64
	Buffer         int
}

func (c Config) Validate() error {
	u, err := url.Parse(c.WS)
	if err != nil || (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("OneBot universal WebSocket root URL required (no credentials/query)")
	}
	if strings.TrimSpace(c.Token) == "" || strings.ContainsAny(c.Token, "\r\n") {
		return fmt.Errorf("Bearer token required")
	}
	if c.APITimeout <= 0 || c.ConnectTimeout <= 0 || c.EventLimit <= 0 || c.ResponseLimit <= 0 || c.Buffer <= 0 {
		return fmt.Errorf("invalid transport limits")
	}
	return nil
}

type Client struct {
	cfg              Config
	state            atomic.Value
	reconnectStopped atomic.Bool
	running          atomic.Bool
	sequence         atomic.Uint64
	generation       atomic.Uint64
	mu               sync.Mutex
	session          *session
	closed           chan struct{}
	closeOnce        sync.Once
	stopReconnect    chan struct{}
	stopOnce         sync.Once
	// Private timing seams for deterministic lifecycle acceptance; production
	// defaults remain the agreed 30s Ping / 10s Pong and real retry timers.
	pingEvery, pongWithin time.Duration
	retryWait             func(context.Context, time.Duration) bool
}

type response struct {
	data *strictjson.Value
	err  error
}

// Pending calls belong to one socket generation and can never move to its replacement.
type session struct {
	conn       *websocket.Conn
	pending    map[string]chan response
	generation uint64
}

func New(cfg Config) (*Client, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	c := &Client{cfg: cfg, closed: make(chan struct{}), stopReconnect: make(chan struct{}), pingEvery: 30 * time.Second, pongWithin: 10 * time.Second}
	c.state.Store(sdk.Disconnected)
	return c, nil
}
func (c *Client) State() sdk.ConnectionState { return c.state.Load().(sdk.ConnectionState) }
func (c *Client) Generation() uint64         { return c.generation.Load() }
func (c *Client) StopReconnect() {
	c.stopOnce.Do(func() { c.reconnectStopped.Store(true); close(c.stopReconnect) })
}
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.StopReconnect()
		c.mu.Lock()
		c.state.Store(sdk.Closing)
		s := c.session
		c.mu.Unlock()
		close(c.closed)
		if s != nil {
			c.detach(s)
		}
	})
}
func (c *Client) detach(s *session) {
	c.mu.Lock()
	if c.session == s {
		c.session = nil
		if c.State() != sdk.Closing {
			c.state.Store(sdk.Disconnected)
		}
	}
	for echo, ch := range s.pending {
		delete(s.pending, echo)
		ch <- response{err: &sdk.Error{Kind: sdk.NetworkFailure, Reason: "connection lost; execution may have occurred"}}
	}
	c.mu.Unlock()
	s.conn.CloseNow()
}
func (c *Client) call(ctx context.Context, action string, payload any) (*strictjson.Value, error) {
	return c.callAt(ctx, action, payload, 0)
}

func (c *Client) callAt(ctx context.Context, action string, payload any, generation uint64) (*strictjson.Value, error) {
	if c.State() != sdk.Connected {
		return nil, &sdk.Error{Kind: sdk.Unavailable, Operation: action, Reason: "event connection unavailable"}
	}
	ctx, cancel := context.WithTimeout(ctx, c.cfg.APITimeout)
	defer cancel()
	if ctx.Err() != nil {
		return nil, requestError(action, ctx, ctx.Err())
	}
	echo := strconv.FormatUint(c.sequence.Add(1), 10)
	body, err := json.Marshal(struct {
		Action string `json:"action"`
		Params any    `json:"params"`
		Echo   string `json:"echo"`
	}{action, payload, echo})
	if err != nil {
		return nil, &sdk.Error{Kind: sdk.Invalid, Operation: action, Cause: err}
	}
	ch := make(chan response, 1)
	c.mu.Lock()
	s := c.session
	if generation != 0 && (s == nil || s.generation != generation) {
		c.mu.Unlock()
		return nil, &sdk.Error{Kind: sdk.Expired, Operation: action, Reason: "quote connection expired"}
	}
	if s == nil || c.State() != sdk.Connected {
		c.mu.Unlock()
		return nil, &sdk.Error{Kind: sdk.Unavailable, Operation: action, Reason: "connection unavailable"}
	}
	s.pending[echo] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(s.pending, echo); c.mu.Unlock() }()
	if err = s.conn.Write(ctx, websocket.MessageText, body); err != nil {
		c.detach(s)
		return nil, requestError(action, ctx, err)
	}
	select {
	case <-ctx.Done():
		return nil, requestError(action, ctx, ctx.Err())
	case r := <-ch:
		if r.err != nil {
			var e *sdk.Error
			if errors.As(r.err, &e) {
				copy := *e
				copy.Operation = action
				return nil, &copy
			}
			return nil, r.err
		}
		return decodeResponse(action, r.data)
	}
}
func decodeResponse(action string, v *strictjson.Value) (*strictjson.Value, error) {
	f, err := v.Fields("status", "retcode", "data", "message", "wording", "echo", "stream")
	if err != nil {
		return nil, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: err.Error()}
	}
	status, err := f["status"].RequireString()
	if err != nil {
		return nil, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: "missing/invalid status"}
	}
	code, err := integer(f["retcode"], false)
	if err != nil {
		return nil, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: "missing/invalid retcode"}
	}
	reason := ""
	for _, k := range []string{"message", "wording"} {
		if x := f[k]; x != nil {
			s, e := x.RequireString()
			if e != nil {
				return nil, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: "invalid " + k}
			}
			if s != "" {
				reason = s
			}
		}
	}
	if status != "ok" || code != 0 {
		return nil, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Code: code, Reason: reason}
	}
	if stream := f["stream"]; stream != nil && (stream.Kind != strictjson.String || stream.Text != "normal-action") {
		return nil, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: "unknown or streaming response mode"}
	}
	if f["data"] == nil {
		return nil, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: "missing data"}
	}
	return f["data"], nil
}
func requestError(op string, ctx context.Context, err error) error {
	k := sdk.NetworkFailure
	if ctx.Err() == context.DeadlineExceeded {
		k = sdk.Timeout
	} else if ctx.Err() != nil {
		k = sdk.Canceled
	}
	return &sdk.Error{Kind: k, Operation: op, Reason: "request failed; execution may have occurred", Cause: err}
}

// SendPrepared accepts only Core-prepared local images. Public image preparation lives in botapi.
func (c *Client) SendPrepared(ctx context.Context, target sdk.Target, parts []sdk.OutSegment) (sdk.SendResult, error) {
	var result sdk.SendResult
	if err := target.Validate(); err != nil {
		return result, err
	}
	if len(parts) == 0 {
		return result, &sdk.Error{Kind: sdk.Invalid, Reason: "empty message"}
	}
	segments := make([]map[string]any, 0, len(parts))
	for _, s := range parts {
		var data any
		kind := string(s.Kind)
		switch s.Kind {
		case sdk.Text:
			data = map[string]string{"text": s.Text}
		case sdk.Mention, sdk.MentionAll:
			if target.Scene != sdk.Group {
				return result, &sdk.Error{Kind: sdk.Unsupported, Reason: "At is supported only in groups"}
			}
			q := "all"
			if s.Kind == sdk.Mention {
				if s.UserID <= 0 {
					return result, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid At target"}
				}
				q = fmt.Sprint(s.UserID)
			}
			kind = "at"
			data = map[string]string{"qq": q}
		case sdk.Reply:
			if s.Ref == nil || s.Ref.ID == 0 || s.Ref.Target != target {
				return result, &sdk.Error{Kind: sdk.Invalid, Reason: "reply must reference the same conversation"}
			}
			data = map[string]string{"id": fmt.Sprint(s.Ref.ID)}
		case sdk.Image:
			if s.Image == nil || s.Image.Path == "" || s.Image.URL != "" || len(s.Image.Bytes) > 0 {
				return result, &sdk.Error{Kind: sdk.Invalid, Reason: "image not prepared"}
			}
			data = map[string]string{"file": s.Image.Path}
		default:
			return result, &sdk.Error{Kind: sdk.Unsupported, Reason: "outgoing segment not supported"}
		}
		segments = append(segments, map[string]any{"type": kind, "data": data})
	}
	payload := map[string]any{"message": segments}
	action := "send_private_msg"
	if target.Scene == sdk.Group {
		action = "send_group_msg"
		payload["group_id"] = target.ID
	} else {
		payload["user_id"] = target.ID
	}
	data, err := c.call(ctx, action, payload)
	if err != nil {
		return result, err
	}
	confirmed := time.Now().UTC()
	f, err := data.Fields("message_id")
	if err != nil {
		return result, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: err.Error()}
	}
	id, err := integer(f["message_id"], false)
	if err != nil || id == 0 {
		return result, &sdk.Error{Kind: sdk.ProtocolFailure, Operation: action, Reason: "invalid message ID; send may have occurred"}
	}
	return sdk.SendResult{Ref: sdk.MessageRef{Target: target, ID: id}, ConfirmedAt: confirmed}, nil
}
func (c *Client) Recall(ctx context.Context, ref sdk.MessageRef) error {
	if err := ref.Target.Validate(); err != nil {
		return err
	}
	if ref.ID == 0 {
		return &sdk.Error{Kind: sdk.Invalid, Reason: "missing message ID"}
	}
	data, err := c.call(ctx, "delete_msg", map[string]int64{"message_id": ref.ID})
	if err != nil {
		return err
	}
	if data.Kind != strictjson.Null {
		if _, err := data.Fields(); err != nil {
			return &sdk.Error{Kind: sdk.ProtocolFailure, Reason: "invalid delete response"}
		}
	}
	return nil
}
