// Package router compiles immutable rule snapshots and routes serially without running handlers.
package router

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type TextCondition struct {
	Mode  string `yaml:"mode"`
	Value string `yaml:"value"`
	re    *regexp.Regexp
}
type Permissions struct {
	Private []int64           `yaml:"private"`
	Groups  map[int64][]int64 `yaml:"groups"`
}
type Rule struct {
	All         bool              `yaml:"all"`
	Text        []TextCondition   `yaml:"text"`
	At          string            `yaml:"at"`
	Media       string            `yaml:"media"`
	Types       []sdk.SegmentKind `yaml:"types"`
	Permissions Permissions       `yaml:"permissions"`
	Continue    bool              `yaml:"continue"`
}
type Entry struct {
	ID                 string
	Priority           int64
	Rules              []Rule
	Deliver            func(sdk.Message) sdk.DeliveryResult
	NeedsQuotedMessage bool
}
type snapshot struct{ Entries []Entry }
type Result struct {
	Plugin string
	Rule   int
	Event  string
	Reason string
}
type Router struct {
	serial   sync.Mutex
	current  atomic.Pointer[snapshot]
	disabled map[string]bool
	closing  atomic.Bool
}

func (r *Router) Stop() { r.closing.Store(true) }

func New() *Router { r := &Router{disabled: map[string]bool{}}; r.current.Store(&snapshot{}); return r }

// Commit atomically orders the durable pointer switch and new snapshot against routing.
func (r *Router) Commit(entries []Entry, persist func() error) error {
	compiled, e := Compile(entries)
	if e != nil {
		return e
	}
	r.serial.Lock()
	defer r.serial.Unlock()
	if e = persist(); e != nil {
		return e
	}
	r.current.Store(&snapshot{Entries: compiled})
	return nil
}
func Compile(entries []Entry) ([]Entry, error) {
	out := make([]Entry, len(entries))
	ids := map[string]bool{}
	priorities := map[int64]bool{}
	for i, e := range entries {
		if ids[e.ID] || priorities[e.Priority] || e.Priority < 1 || e.Deliver == nil {
			return nil, fmt.Errorf("invalid or conflicting entry %q", e.ID)
		}
		ids[e.ID] = true
		priorities[e.Priority] = true
		out[i] = e
		out[i].Rules = make([]Rule, len(e.Rules))
		for j, rule := range e.Rules {
			c, err := compile(rule)
			if err != nil {
				return nil, fmt.Errorf("%s rule %d: %w", e.ID, j, err)
			}
			out[i].Rules[j] = c
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	return out, nil
}
func compile(r Rule) (Rule, error) {
	if r.At == "" {
		r.At = "any"
	}
	if r.Media == "" {
		r.Media = "any"
	}
	if r.At != "any" && r.At != "required" && r.At != "forbidden" {
		return r, fmt.Errorf("invalid at condition")
	}
	if r.Media != "any" && r.Media != "present" && r.Media != "absent" && r.Media != "types" {
		return r, fmt.Errorf("invalid media condition")
	}
	has := len(r.Text) > 0 || r.At != "any" || r.Media != "any"
	if r.All && has || !r.All && !has {
		return r, fmt.Errorf("all is required for an unconstrained rule and exclusive with conditions")
	}
	if r.Media == "types" && len(r.Types) == 0 || r.Media != "types" && len(r.Types) != 0 {
		return r, fmt.Errorf("invalid media type selection")
	}
	r.Types = append([]sdk.SegmentKind(nil), r.Types...)
	seen := map[sdk.SegmentKind]bool{}
	for _, k := range r.Types {
		if !isMedia(k) || seen[k] {
			return r, fmt.Errorf("invalid media type")
		}
		seen[k] = true
	}
	r.Text = append([]TextCondition(nil), r.Text...)
	for i := range r.Text {
		c := &r.Text[i]
		switch c.Mode {
		case "exact", "prefix", "contains", "suffix":
		case "regex":
			re, err := regexp.Compile(c.Value)
			if err != nil {
				return r, err
			}
			c.re = re
		default:
			return r, fmt.Errorf("invalid text mode")
		}
	}
	r.Permissions.Private = append([]int64(nil), r.Permissions.Private...)
	groups := make(map[int64][]int64, len(r.Permissions.Groups))
	for id, users := range r.Permissions.Groups {
		if id <= 0 {
			return r, fmt.Errorf("invalid group ID")
		}
		for _, u := range users {
			if u <= 0 {
				return r, fmt.Errorf("invalid blacklist ID")
			}
		}
		groups[id] = append([]int64(nil), users...)
	}
	r.Permissions.Groups = groups
	for _, id := range r.Permissions.Private {
		if id <= 0 {
			return r, fmt.Errorf("invalid user ID")
		}
	}
	return r, nil
}
func (r *Router) Update(entries []Entry) error {
	e, err := Compile(entries)
	if err != nil {
		return err
	}
	r.current.Store(&snapshot{Entries: e})
	return nil
}
func (r *Router) Route(m sdk.Message) []Result {
	return r.RouteWith(context.Background(), m, nil)
}

// RouteWith advances one message through one immutable rule snapshot. Preparation
// is lazy, once per message, and runs outside the routing gate. Only short state
// decisions and Deliver callbacks hold that gate; quoted content never matches.
// The caller owns conversation order, admission limits and cancellation.
func (r *Router) RouteWith(ctx context.Context, m sdk.Message, prepare func(context.Context, sdk.Message) sdk.Message) []Result {
	r.serial.Lock()
	s := r.current.Load()
	r.serial.Unlock()
	var results []Result
	var enriched sdk.Message
	prepared := false
	for _, e := range s.Entries {
		r.serial.Lock()
		if r.closing.Load() || ctx.Err() != nil {
			r.serial.Unlock()
			break
		}
		if r.disabled[e.ID] {
			r.serial.Unlock()
			continue
		}
		index := -1
		for i, rule := range e.Rules {
			if matches(rule, m) {
				index = i
				break
			}
		}
		if index < 0 {
			r.serial.Unlock()
			continue
		}
		rule := e.Rules[index]
		if !allowed(rule.Permissions, m) {
			r.serial.Unlock()
			return append(results, Result{e.ID, index, "permission_denied", "whitelist/blacklist"})
		}
		r.serial.Unlock()
		if e.NeedsQuotedMessage && !prepared {
			enriched = m
			if prepare != nil {
				enriched = prepare(ctx, m)
			}
			prepared = true
		}
		view := m
		if e.NeedsQuotedMessage {
			view = enriched
		}
		copy := sdk.CloneMessage(view)
		copy.Segments = deliverySegments(copy)
		r.serial.Lock()
		if r.closing.Load() || ctx.Err() != nil {
			r.serial.Unlock()
			break
		}
		// Another conversation may have disabled this plugin while preparation
		// waited. Skip it, preserving the remaining snapshot candidates.
		if r.disabled[e.ID] {
			r.serial.Unlock()
			continue
		}
		delivery, panicked := deliver(e.Deliver, copy)
		event := "rejected"
		if panicked {
			r.disabled[e.ID] = true
			event = "panic"
		} else if delivery.Accepted {
			event = "accepted"
		}
		r.serial.Unlock()
		results = append(results, Result{e.ID, index, event, delivery.Reason})
		if !rule.Continue {
			break
		}
	}
	return results
}
func deliver(fn func(sdk.Message) sdk.DeliveryResult, m sdk.Message) (result sdk.DeliveryResult, panicked bool) {
	defer func() {
		if x := recover(); x != nil {
			panicked = true
			result.Reason = fmt.Sprint(x)
		}
	}()
	return fn(m), false
}

// deliverySegments prepares only the direct segments of a cloned message;
// rule matching and nested quoted/forwarded messages retain their original text.
func deliverySegments(m sdk.Message) []sdk.Segment {
	out := m.Segments[:0]
	for _, s := range m.Segments {
		if s.Kind == sdk.Mention && m.BotID.State == sdk.Present && s.UserID == m.BotID.Value {
			continue
		}
		if s.Kind == sdk.Text {
			s.Text = strings.TrimSpace(s.Text)
		}
		out = append(out, s)
	}
	return out
}
func isMedia(k sdk.SegmentKind) bool {
	return k == sdk.Image || k == sdk.Record || k == sdk.Video || k == sdk.File
}
func matches(r Rule, m sdk.Message) bool {
	if r.All {
		return true
	}
	if r.At == "required" && !m.WasAtBot || r.At == "forbidden" && m.WasAtBot {
		return false
	}
	media := map[sdk.SegmentKind]bool{}
	for _, s := range m.Segments {
		if isMedia(s.Kind) {
			media[s.Kind] = true
		}
	}
	if r.Media == "present" && len(media) == 0 || r.Media == "absent" && len(media) > 0 {
		return false
	}
	for _, k := range r.Types {
		if !media[k] {
			return false
		}
	}
	if len(r.Text) == 0 {
		return true
	}
	for _, fragment := range fragments(m) {
		ok := true
		for _, c := range r.Text {
			if !textMatch(c, fragment) {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
func fragments(m sdk.Message) []string {
	var result []string
	var current strings.Builder
	exists := false
	flush := func() {
		if exists {
			result = append(result, current.String())
			current.Reset()
			exists = false
		}
	}
	for _, s := range m.Segments {
		switch s.Kind {
		case sdk.Text:
			exists = true
			current.WriteString(s.Text)
		case sdk.Mention:
			exists = true
			if m.BotID.State == sdk.Present && s.UserID == m.BotID.Value {
				current.WriteString("At(Bot)")
			} else {
				fmt.Fprintf(&current, "At(%d)", s.UserID)
			}
		case sdk.MentionAll:
			exists = true
			current.WriteString("At(All)")
		default:
			flush()
		}
	}
	flush()
	return result
}
func textMatch(c TextCondition, s string) bool {
	switch c.Mode {
	case "exact":
		return s == c.Value
	case "prefix":
		return strings.HasPrefix(s, c.Value)
	case "suffix":
		return strings.HasSuffix(s, c.Value)
	case "contains":
		return strings.Contains(s, c.Value)
	case "regex":
		return c.re.MatchString(s)
	}
	return false
}
func contains(a []int64, x int64) bool {
	for _, v := range a {
		if v == x {
			return true
		}
	}
	return false
}
func allowed(p Permissions, m sdk.Message) bool {
	if m.Ref.Target.Scene == sdk.Private {
		return contains(p.Private, m.Sender.ID)
	}
	blocked, exists := p.Groups[m.Ref.Target.ID]
	return exists && !contains(blocked, m.Sender.ID)
}
