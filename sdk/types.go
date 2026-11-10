// Package sdk defines the public, narrowly scoped contract shared by Core and plugins.
package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"
)

const APIVersion = 1

type FieldState uint8

const (
	Missing FieldState = iota
	Null
	Present
)

type Optional[T any] struct {
	State FieldState
	Value T
}
type Scene string

const (
	Private Scene = "private"
	Group   Scene = "group"
)

type Target struct {
	Scene Scene
	ID    int64
}

func (t Target) Validate() error {
	if (t.Scene != Private && t.Scene != Group) || t.ID <= 0 {
		return &Error{Kind: Invalid, Reason: "invalid conversation target"}
	}
	return nil
}

type MessageRef struct {
	Target Target
	ID     int64
}
type Resource struct{ Token string }
type SegmentKind string

const (
	Text       SegmentKind = "text"
	Mention    SegmentKind = "mention"
	MentionAll SegmentKind = "mention_all"
	Face       SegmentKind = "face"
	Reply      SegmentKind = "reply"
	Image      SegmentKind = "image"
	Record     SegmentKind = "record"
	Video      SegmentKind = "video"
	File       SegmentKind = "file"
	Forward    SegmentKind = "forward"
	MarketFace SegmentKind = "market_face"
	Card       SegmentKind = "json"
	XML        SegmentKind = "xml"
	Markdown   SegmentKind = "markdown"
)

// Metadata contains only explicitly decoded, schema-recognized scalar values.
type Scalar struct {
	State   FieldState
	String  *string
	Integer *int64
	Boolean *bool
	Object  map[string]Scalar
	List    []Scalar
}
type Segment struct {
	Kind     SegmentKind
	Text     string
	UserID   int64
	Ref      *MessageRef
	Quoted   *Message
	Messages []Message
	Unknown  *Unknown
	Resource *Resource
	Metadata map[string]Scalar
	Payload  string
}
type Unknown struct {
	Reason   string
	Location string
}
type Sender struct {
	ID       int64
	Nickname Optional[string]
	Card     Optional[string]
	Role     Optional[string]
}
type Message struct {
	Ref         MessageRef
	Sequence    Optional[int64]
	ReceivedAt  time.Time
	EventTime   Optional[time.Time]
	MessageTime Optional[time.Time]
	Sender      Sender
	BotID       Optional[int64]
	WasAtBot    bool
	Segments    []Segment
	Metadata    map[string]Scalar
}

func CloneMessage(m Message) Message {
	m.Metadata = cloneMetadata(m.Metadata)
	m.Segments = append([]Segment(nil), m.Segments...)
	for i := range m.Segments {
		s := &m.Segments[i]
		if s.Messages != nil {
			messages := make([]Message, len(s.Messages))
			for j, msg := range s.Messages {
				messages[j] = CloneMessage(msg)
			}
			s.Messages = messages
		}
		s.Metadata = cloneMetadata(s.Metadata)
		if s.Ref != nil {
			x := *s.Ref
			s.Ref = &x
		}
		if s.Resource != nil {
			x := *s.Resource
			s.Resource = &x
		}
		if s.Unknown != nil {
			x := *s.Unknown
			s.Unknown = &x
		}
		if s.Quoted != nil {
			x := CloneMessage(*s.Quoted)
			s.Quoted = &x
		}
	}
	return m
}
func cloneMetadata(m map[string]Scalar) map[string]Scalar {
	if m == nil {
		return nil
	}
	n := make(map[string]Scalar, len(m))
	for k, v := range m {
		v.Object = cloneMetadata(v.Object)
		if v.List != nil {
			copy := make([]Scalar, len(v.List))
			for i, x := range v.List {
				copy[i] = cloneMetadata(map[string]Scalar{"v": x})["v"]
			}
			v.List = copy
		}
		if v.String != nil {
			x := *v.String
			v.String = &x
		}
		if v.Integer != nil {
			x := *v.Integer
			v.Integer = &x
		}
		if v.Boolean != nil {
			x := *v.Boolean
			v.Boolean = &x
		}
		n[k] = v
	}
	return n
}

type ImageSource struct {
	URL   string
	Path  string
	Bytes []byte
}
type OutSegment struct {
	Kind   SegmentKind
	Text   string
	UserID int64
	Ref    *MessageRef
	Image  *ImageSource
}
type SendResult struct {
	Ref         MessageRef
	ConfirmedAt time.Time
	SentAt      Optional[time.Time]
}
type ConnectionState string

const (
	Disconnected ConnectionState = "disconnected"
	Connecting   ConnectionState = "connecting"
	Connected    ConnectionState = "connected"
	Closing      ConnectionState = "closing"
)

type ErrorKind string

const (
	Invalid         ErrorKind = "invalid_parameter"
	ShuttingDown    ErrorKind = "closing"
	Unavailable     ErrorKind = "connection_unavailable"
	Unsupported     ErrorKind = "unsupported"
	ProtocolFailure ErrorKind = "protocol_failure"
	NetworkFailure  ErrorKind = "network_failure"
	Timeout         ErrorKind = "timeout"
	Canceled        ErrorKind = "canceled"
	MediaDisabled   ErrorKind = "media_disabled"
	Busy            ErrorKind = "download_busy"
	NoSpace         ErrorKind = "no_space"
	TooLarge        ErrorKind = "too_large"
	Expired         ErrorKind = "resource_expired"
	Released        ErrorKind = "handle_released"
	Revoked         ErrorKind = "handle_revoked"
)

type Error struct {
	Kind      ErrorKind
	Operation string
	Reason    string
	Code      int64
	Cause     error
}

func (e *Error) Error() string            { return fmt.Sprintf("%s: %s: %s", e.Kind, e.Operation, e.Reason) }
func (e *Error) Unwrap() error            { return e.Cause }
func (e *Error) Is(other error) bool      { x, ok := other.(*Error); return ok && x.Kind == e.Kind }
func HasKind(err error, k ErrorKind) bool { return errors.Is(err, &Error{Kind: k}) }

type MediaHandle interface {
	Path() (string, error)
	Reader() (io.ReadCloser, error)
	Release() error
}
type Descriptor struct {
	ID          string
	API         int
	Name        string
	Version     string
	Description string
}
type DeliveryResult struct {
	Accepted bool
	Reason   string
}
type Plugin interface {
	Init(context.Context, Host) error
	Deliver(Message) DeliveryResult
	Close(context.Context) error
}
type User struct {
	ID       int64
	Nickname Optional[string]
	Remark   Optional[string]
	Metadata map[string]Scalar
}
type GroupInfo struct {
	ID       int64
	Name     string
	Metadata map[string]Scalar
}
type Member struct {
	GroupID  int64
	User     User
	Role     Optional[string]
	Metadata map[string]Scalar
}
type Host interface {
	ConnectionState() ConnectionState
	Send(context.Context, Target, []OutSegment) (SendResult, error)
	Recall(context.Context, MessageRef) error
	GetMessage(context.Context, MessageRef) (Message, error)
	LoginInfo(context.Context) (User, error)
	UserInfo(context.Context, int64) (User, error)
	FriendInfo(context.Context, int64) (User, error)
	Friends(context.Context) ([]User, error)
	GroupInfo(context.Context, int64) (GroupInfo, error)
	Groups(context.Context) ([]GroupInfo, error)
	MemberInfo(context.Context, int64, int64) (Member, error)
	Members(context.Context, int64) ([]Member, error)
	AcquireMedia(context.Context, Resource) (MediaHandle, error)
	Log(level, message string)
	DataDir() string
}
