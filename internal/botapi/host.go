package botapi

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"time"
	"github.com/ZhenForest/xsbot-core/internal/media"
	"github.com/ZhenForest/xsbot-core/internal/onebot"
	run "github.com/ZhenForest/xsbot-core/internal/runtime"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type Shared struct {
	Client     *onebot.Client
	Media      *media.Manager
	Operations *run.Operations
	Log        func(string, string, string)
	references cipher.AEAD
	quoteSlots chan struct{}
}
type resource struct {
	Segment    sdk.Segment
	Key        string
	Generation uint64
}
type Host struct {
	Shared        *Shared
	ID, Directory string
}

func NewShared(c *onebot.Client, m *media.Manager, ops *run.Operations, log func(string, string, string)) *Shared {
	key := make([]byte, 32)
	if _, e := rand.Read(key); e != nil {
		panic(e)
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		panic(e)
	}
	sealed, e := cipher.NewGCM(block)
	if e != nil {
		panic(e)
	}
	return &Shared{Client: c, Media: m, Operations: ops, Log: log, references: sealed, quoteSlots: make(chan struct{}, 4)}
}
func (s *Shared) Resources(m sdk.Message) sdk.Message {
	return s.ResourcesAt(m, s.Client.Generation())
}
func (s *Shared) ResourcesAt(m sdk.Message, generation uint64) sdk.Message {
	m = sdk.CloneMessage(m)
	s.attachResources(&m, generation)
	return m
}
func (s *Shared) attachResources(m *sdk.Message, generation uint64) {
	for i := range m.Segments {
		segment := &m.Segments[i]
		switch segment.Kind {
		case sdk.Image, sdk.Record, sdk.Video, sdk.File:
			key := fmt.Sprintf("receive:%d:%d:%s:%s:%d:%d:%d", generation, m.BotID.Value, segment.Kind, m.Ref.Target.Scene, m.Ref.Target.ID, m.Ref.ID, i)
			value, _ := json.Marshal(resource{Segment: *segment, Key: key, Generation: generation})
			nonce := make([]byte, s.references.NonceSize())
			if _, e := rand.Read(nonce); e != nil {
				panic(e)
			}
			token := base64.RawURLEncoding.EncodeToString(s.references.Seal(nonce, nonce, value, nil))
			segment.Resource = &sdk.Resource{Token: token}
		}
		if segment.Quoted != nil && segment.Unknown == nil {
			s.attachResources(segment.Quoted, generation)
		}
		for j := range segment.Messages {
			s.attachResources(&segment.Messages[j], generation)
		}
	}
}
func (s *Shared) Closing() {
	s.Operations.StartClosing()
	s.Media.StartClosing()
}
func (h *Host) ConnectionState() sdk.ConnectionState {
	if h.Shared.Operations.IsClosing() {
		return sdk.Closing
	}
	return h.Shared.Client.State()
}
func (h *Host) DataDir() string { return h.Directory }
func (h *Host) Log(level, text string) {
	if level != "DEBUG" && level != "INFO" && level != "WARN" && level != "ERROR" {
		level = "INFO"
	}
	h.Shared.Log(level, h.ID, text)
}
func (h *Host) begin(ctx context.Context, networkMedia bool) (context.Context, func(), error) {
	return h.Shared.Operations.Begin(ctx, networkMedia)
}
func (h *Host) Send(ctx context.Context, target sdk.Target, parts []sdk.OutSegment) (sdk.SendResult, error) {
	var zero sdk.SendResult
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return zero, e
	}
	defer done()
	if e = target.Validate(); e != nil {
		return zero, e
	}
	if len(parts) == 0 {
		return zero, &sdk.Error{Kind: sdk.Invalid, Reason: "empty message"}
	}
	// Validate all segments before any source preparation or side effect.
	for _, s := range parts {
		switch s.Kind {
		case sdk.Text:
		case sdk.Mention, sdk.MentionAll:
			if target.Scene != sdk.Group {
				return zero, &sdk.Error{Kind: sdk.Unsupported, Reason: "At only supported in groups"}
			}
			if s.Kind == sdk.Mention && s.UserID <= 0 {
				return zero, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid At target"}
			}
		case sdk.Reply:
			if s.Ref == nil || s.Ref.ID == 0 || s.Ref.Target != target {
				return zero, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid reply locator"}
			}
		case sdk.Image:
			if s.Image == nil {
				return zero, &sdk.Error{Kind: sdk.Invalid, Reason: "missing image source"}
			}
			n := 0
			if s.Image.URL != "" {
				n++
			}
			if s.Image.Path != "" {
				n++
			}
			if s.Image.Bytes != nil {
				n++
			}
			if n != 1 {
				return zero, &sdk.Error{Kind: sdk.Invalid, Reason: "exactly one image source required"}
			}
		default:
			return zero, &sdk.Error{Kind: sdk.Unsupported, Reason: "unplanned outgoing segment"}
		}
	}
	prepared := append([]sdk.OutSegment(nil), parts...)
	var handles []sdk.MediaHandle
	defer func() {
		for _, handle := range handles {
			handle.Release()
		}
	}()
	for i, s := range parts {
		if s.Kind != sdk.Image {
			continue
		}
		var key string
		var open media.Open
		network := s.Image.URL != ""
		if network {
			if e = h.Shared.Operations.CheckNetworkMedia(); e != nil {
				return zero, e
			}
			key, e = media.URLKey(s.Image.URL)
			if e != nil {
				return zero, e
			}
			open = media.HTTPOpen(s.Image.URL)
		} else {
			key = "local:" + media.RandomKey()
			if s.Image.Path != "" {
				open = media.LocalOpen(s.Image.Path)
			} else {
				copy := append([]byte(nil), s.Image.Bytes...)
				open = func(context.Context) (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(copy)), nil }
			}
		}
		handle, e := h.Shared.Media.Acquire(ctx, key, open, network)
		if e != nil {
			return zero, e
		}
		handles = append(handles, handle)
		path, e := handle.Path()
		if e != nil {
			return zero, e
		}
		prepared[i].Image = &sdk.ImageSource{Path: path}
	}
	for _, handle := range handles {
		if _, e = handle.Path(); e != nil {
			return zero, e
		}
	}
	result, e := h.Shared.Client.SendPrepared(ctx, target, prepared)
	h.record("send", e)
	return result, e
}
func (h *Host) record(op string, e error) {
	if e != nil {
		h.Shared.Log("ERROR", h.ID, op+": "+e.Error())
	}
}
func (h *Host) AcquireMedia(ctx context.Context, reference sdk.Resource) (sdk.MediaHandle, error) {
	ctx, done, e := h.begin(ctx, true)
	if e != nil {
		return nil, e
	}
	defer done()
	var source resource
	encoded, decodeErr := base64.RawURLEncoding.DecodeString(reference.Token)
	if decodeErr != nil || len(encoded) < h.Shared.references.NonceSize() {
		return nil, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid resource reference"}
	}
	n := h.Shared.references.NonceSize()
	decoded, decodeErr := h.Shared.references.Open(nil, encoded[:n], encoded[n:], nil)
	if decodeErr != nil || json.Unmarshal(decoded, &source) != nil {
		return nil, &sdk.Error{Kind: sdk.Invalid, Reason: "invalid resource reference"}
	}
	if source.Generation != h.Shared.Client.Generation() {
		return nil, &sdk.Error{Kind: sdk.Expired, Reason: "resource reference expired"}
	}
	handle, e := h.Shared.Media.Acquire(ctx, source.Key, func(ctx context.Context) (io.ReadCloser, error) {
		return h.Shared.Client.OpenResource(ctx, source.Segment)
	}, true)
	h.record("media", e)
	return handle, e
}
func (h *Host) Recall(ctx context.Context, ref sdk.MessageRef) error {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return e
	}
	defer done()
	e = h.Shared.Client.Recall(ctx, ref)
	h.record("recall", e)
	return e
}
func (h *Host) GetMessage(ctx context.Context, ref sdk.MessageRef) (sdk.Message, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return sdk.Message{}, e
	}
	defer done()
	v, e := h.Shared.Client.GetMessage(ctx, ref)
	h.record("get_message", e)
	if e == nil {
		v = h.Shared.Resources(v)
	}
	return v, e
}
func (h *Host) LoginInfo(ctx context.Context) (sdk.User, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return sdk.User{}, e
	}
	defer done()
	v, e := h.Shared.Client.LoginInfo(ctx)
	h.record("login_info", e)
	return v, e
}
func (h *Host) UserInfo(ctx context.Context, id int64) (sdk.User, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return sdk.User{}, e
	}
	defer done()
	v, e := h.Shared.Client.UserInfo(ctx, id)
	h.record("user_info", e)
	return v, e
}
func (h *Host) FriendInfo(ctx context.Context, id int64) (sdk.User, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return sdk.User{}, e
	}
	defer done()
	v, e := h.Shared.Client.FriendInfo(ctx, id)
	h.record("friend_info", e)
	return v, e
}
func (h *Host) Friends(ctx context.Context) ([]sdk.User, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return nil, e
	}
	defer done()
	v, e := h.Shared.Client.Friends(ctx)
	h.record("friends", e)
	return v, e
}
func (h *Host) GroupInfo(ctx context.Context, id int64) (sdk.GroupInfo, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return sdk.GroupInfo{}, e
	}
	defer done()
	v, e := h.Shared.Client.GroupInfo(ctx, id)
	h.record("group_info", e)
	return v, e
}
func (h *Host) Groups(ctx context.Context) ([]sdk.GroupInfo, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return nil, e
	}
	defer done()
	v, e := h.Shared.Client.Groups(ctx)
	h.record("groups", e)
	return v, e
}
func (h *Host) MemberInfo(ctx context.Context, g, u int64) (sdk.Member, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return sdk.Member{}, e
	}
	defer done()
	v, e := h.Shared.Client.MemberInfo(ctx, g, u)
	h.record("member_info", e)
	return v, e
}
func (h *Host) Members(ctx context.Context, id int64) ([]sdk.Member, error) {
	ctx, done, e := h.begin(ctx, false)
	if e != nil {
		return nil, e
	}
	defer done()
	v, e := h.Shared.Client.Members(ctx, id)
	h.record("members", e)
	return v, e
}

var _ sdk.Host = (*Host)(nil)
var _ = time.Second
