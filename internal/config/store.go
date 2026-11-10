package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"go.yaml.in/yaml/v3"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"github.com/ZhenForest/xsbot-core/internal/plugins"
	"github.com/ZhenForest/xsbot-core/internal/router"
	"github.com/ZhenForest/xsbot-core/sdk"
)

type RuleFile struct {
	Schema int           `yaml:"schema_version"`
	ID     string        `yaml:"id"`
	Rules  []router.Rule `yaml:"rules"`
}
type Pointer struct {
	Schema   int    `yaml:"schema_version"`
	Revision string `yaml:"revision"`
	Previous string `yaml:"previous"`
}
type Draft struct {
	Schema int    `yaml:"schema_version"`
	Base   string `yaml:"base_revision"`
	Text   string `yaml:"text"`
}
type Rules struct {
	mu          sync.Mutex
	dir, drafts string
	pointer     Pointer
	files       map[string]RuleFile
	Problems    map[string]string
	invalid     bool
	write       func(string, []byte) error
	ids         map[string]bool
}

func revision() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	return hex.EncodeToString(b[:])
}
func validRevision(s string) bool {
	if len(s) != 32 {
		return false
	}
	_, e := hex.DecodeString(s)
	return e == nil
}
func ParseRules(data []byte, id string) (RuleFile, error) {
	var f RuleFile
	if e := strictYAML(data, &f); e != nil {
		return f, e
	}
	if f.Schema != 1 || f.ID != id || !plugins.ValidID(f.ID) {
		return f, fmt.Errorf("invalid Rule identity/schema")
	}
	_, e := router.Compile([]router.Entry{{ID: id, Priority: 1, Rules: f.Rules, Deliver: func(sdk.Message) sdk.DeliveryResult { return sdk.DeliveryResult{} }}})
	return f, e
}
func OpenRules(root string, ids []string) (*Rules, error) {
	s := &Rules{dir: filepath.Join(root, "data/rule_config"), drafts: filepath.Join(root, "data/rule_drafts"), files: map[string]RuleFile{}, Problems: map[string]string{}, write: WriteAtomic, ids: map[string]bool{}}
	for _, id := range ids {
		s.ids[id] = true
	}
	for _, dir := range []string{s.dir, s.drafts, filepath.Join(s.dir, "revisions")} {
		if e := os.MkdirAll(dir, 0700); e != nil {
			return nil, e
		}
	}
	data, e := os.ReadFile(filepath.Join(s.dir, "current.yaml"))
	if errors.Is(e, os.ErrNotExist) {
		entries, e := os.ReadDir(filepath.Join(s.dir, "revisions"))
		if e != nil {
			return nil, e
		}
		if len(entries) == 0 {
			for _, id := range ids {
				s.files[id] = RuleFile{Schema: 1, ID: id, Rules: []router.Rule{}}
			}
			if e = s.persist(s.files); e != nil {
				return nil, e
			}
			return s, nil
		}
	}
	if e != nil || strictYAML(data, &s.pointer) != nil || s.pointer.Schema != 1 || !validRevision(s.pointer.Revision) || s.pointer.Previous != "" && !validRevision(s.pointer.Previous) {
		s.invalid = true
		s.pointer = Pointer{Schema: 1, Revision: "unavailable"}
		for _, id := range ids {
			s.Problems[id] = "formal pointer missing or invalid; explicit complete repair required"
		}
		return s, nil
	}
	for _, id := range ids {
		data, e := os.ReadFile(filepath.Join(s.dir, "revisions", s.pointer.Revision, id+".yaml"))
		if e != nil {
			s.Problems[id] = e.Error()
			continue
		}
		f, e := ParseRules(data, id)
		if e != nil {
			s.Problems[id] = e.Error()
			continue
		}
		s.files[id] = f
	}
	return s, nil
}
func (s *Rules) Snapshot() (string, map[string]RuleFile, map[string]string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files := map[string]RuleFile{}
	for id, f := range s.files {
		b, _ := yaml.Marshal(f)
		copy, _ := ParseRules(b, id)
		files[id] = copy
	}
	problems := map[string]string{}
	for id, e := range s.Problems {
		problems[id] = e
	}
	return s.pointer.Revision, files, problems
}
func (s *Rules) Current(id string) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !plugins.ValidID(id) {
		return "", "", fmt.Errorf("invalid ID")
	}
	if !validRevision(s.pointer.Revision) {
		return s.pointer.Revision, "", fmt.Errorf("formal pointer unavailable; explicit complete repair required")
	}
	data, e := os.ReadFile(filepath.Join(s.dir, "revisions", s.pointer.Revision, id+".yaml"))
	return s.pointer.Revision, string(data), e
}
func (s *Rules) Revision() string { s.mu.Lock(); defer s.mu.Unlock(); return s.pointer.Revision }
func (s *Rules) Draft(id string) (Draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !plugins.ValidID(id) {
		return Draft{}, fmt.Errorf("invalid ID")
	}
	data, e := os.ReadFile(filepath.Join(s.drafts, id+".yaml"))
	if e == nil {
		var d Draft
		e = strictYAML(data, &d)
		if e == nil && d.Schema != 1 {
			e = fmt.Errorf("invalid draft schema")
		}
		if e == nil && d.Base != s.pointer.Revision && s.appliedText(id, d.Text) {
			d.Base = s.pointer.Revision
		}
		return d, e
	}
	if !errors.Is(e, os.ErrNotExist) {
		return Draft{}, e
	}
	f, ok := s.files[id]
	if !ok {
		f = RuleFile{Schema: 1, ID: id, Rules: []router.Rule{}}
	}
	b, e := yaml.Marshal(f)
	return Draft{Schema: 1, Base: s.pointer.Revision, Text: string(b)}, e
}
func (s *Rules) SaveDraft(id string, d Draft) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !plugins.ValidID(id) || !s.ids[id] || d.Schema != 1 || d.Base != s.pointer.Revision {
		return fmt.Errorf("draft baseline conflict")
	}
	p := filepath.Join(s.drafts, id+".yaml")
	if old, e := os.ReadFile(p); e == nil {
		var prior Draft
		if e = strictYAML(old, &prior); e != nil || prior.Schema != 1 {
			return fmt.Errorf("existing draft damaged; explicit repair required")
		}
		if prior.Base != d.Base && !s.appliedText(id, prior.Text) {
			return fmt.Errorf("existing draft baseline conflict")
		}
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	b, e := yaml.Marshal(d)
	if e != nil {
		return e
	}
	return WriteAtomic(p, b)
}
func (s *Rules) appliedText(id, text string) bool {
	f, e := ParseRules([]byte(text), id)
	if e != nil {
		return false
	}
	formal, exists := s.files[id]
	if !exists {
		return false
	}
	a, _ := yaml.Marshal(f)
	b, _ := yaml.Marshal(formal)
	return string(a) == string(b)
}

// Rebase preserves draft text; callers explicitly acknowledge both revisions.
func (s *Rules) Rebase(id, from, current string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !plugins.ValidID(id) || current != s.pointer.Revision {
		return fmt.Errorf("formal baseline changed; review again")
	}
	path := filepath.Join(s.drafts, id+".yaml")
	data, e := os.ReadFile(path)
	if e != nil {
		return e
	}
	var d Draft
	if e = strictYAML(data, &d); e != nil || d.Schema != 1 {
		return fmt.Errorf("draft damaged; explicit repair required")
	}
	if d.Base != from {
		return fmt.Errorf("draft changed; reload before reviewing")
	}
	d.Base = current
	encoded, e := yaml.Marshal(d)
	if e != nil {
		return e
	}
	return WriteAtomic(path, encoded)
}

// Commit stages a complete revision and invokes the routing boundary for the pointer switch.
type RulePreview struct {
	Plugins []string `json:"plugins"`
	Changed []string `json:"changed"`
	Review  string   `json:"review"`
}

func ruleReview(base string, candidate map[string]RuleFile) string {
	data, _ := yaml.Marshal(candidate)
	sum := sha256.Sum256(append([]byte(base+"\n"), data...))
	return hex.EncodeToString(sum[:])
}
func (s *Rules) Preview(base string) (RulePreview, error) {
	p := RulePreview{Plugins: []string{}, Changed: []string{}}
	e := s.Commit(base, func(candidate map[string]RuleFile, _ func() error) error {
		for id, f := range candidate {
			p.Plugins = append(p.Plugins, id)
			left, _ := yaml.Marshal(s.files[id])
			right, _ := yaml.Marshal(f)
			if string(left) != string(right) {
				p.Changed = append(p.Changed, id)
			}
		}
		sort.Strings(p.Plugins)
		sort.Strings(p.Changed)
		p.Review = ruleReview(base, candidate)
		return nil
	})
	return p, e
}
func (s *Rules) CommitReviewed(base, review string, boundary func(map[string]RuleFile, func() error) error) error {
	return s.Commit(base, func(candidate map[string]RuleFile, persist func() error) error {
		if review == "" || review != ruleReview(base, candidate) {
			return fmt.Errorf("规则草稿在预览后已变化，请重新预览并确认")
		}
		return boundary(candidate, persist)
	})
}
func (s *Rules) Commit(base string, boundary func(map[string]RuleFile, func() error) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if base != s.pointer.Revision {
		return fmt.Errorf("formal baseline conflict")
	}
	candidate := map[string]RuleFile{}
	for id, f := range s.files {
		candidate[id] = f
	}
	pending := false
	for id := range s.ids {
		if !plugins.ValidID(id) {
			return fmt.Errorf("invalid ID")
		}
		data, e := os.ReadFile(filepath.Join(s.drafts, id+".yaml"))
		if errors.Is(e, os.ErrNotExist) {
			continue
		}
		if e != nil {
			return e
		}
		var d Draft
		if e = strictYAML(data, &d); e != nil {
			return e
		}
		if d.Schema != 1 {
			return fmt.Errorf("draft baseline conflict")
		}
		f, e := ParseRules([]byte(d.Text), id)
		if e != nil {
			return e
		}
		if d.Base != base {
			old, exists := s.files[id]
			left, _ := yaml.Marshal(old)
			right, _ := yaml.Marshal(f)
			if exists && string(left) == string(right) {
				continue
			} // retained, already-applied text
			return fmt.Errorf("%s draft baseline conflict; explicit review required", id)
		}
		pending = true
		candidate[id] = f
	}
	for id := range s.Problems {
		if _, ok := candidate[id]; !ok {
			return fmt.Errorf("complete repair missing %s", id)
		}
	}
	if !pending {
		return fmt.Errorf("no current Rule drafts to apply")
	}
	return boundary(candidate, func() error { return s.persist(candidate) })
}
func (s *Rules) persist(candidate map[string]RuleFile) error {
	rev := revision()
	path := filepath.Join(s.dir, "revisions", rev)
	if e := os.Mkdir(path, 0700); e != nil {
		return e
	}
	committed := false
	defer func() {
		if !committed {
			os.RemoveAll(path)
		}
	}()
	// Entries removed from Core still retain their historical Rule, including
	// invalid text that must be repaired explicitly if the plugin is reinstalled.
	if validRevision(s.pointer.Revision) {
		old := filepath.Join(s.dir, "revisions", s.pointer.Revision)
		entries, e := os.ReadDir(old)
		if e != nil {
			return e
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".yaml") || !plugins.ValidID(strings.TrimSuffix(name, ".yaml")) {
				continue
			}
			if _, replaced := candidate[strings.TrimSuffix(name, ".yaml")]; replaced {
				continue
			}
			data, e := os.ReadFile(filepath.Join(old, name))
			if e != nil {
				return e
			}
			if e = s.write(filepath.Join(path, name), data); e != nil {
				return e
			}
		}
	}
	for id, f := range candidate {
		data, e := yaml.Marshal(f)
		if e != nil {
			return e
		}
		if e = s.write(filepath.Join(path, id+".yaml"), data); e != nil {
			return e
		}
	}
	parent, e := os.Open(filepath.Dir(path))
	if e != nil {
		return e
	}
	e = parent.Sync()
	parent.Close()
	if e != nil {
		return e
	}
	previous := s.pointer.Revision
	if !validRevision(previous) {
		previous = ""
	}
	pointer := Pointer{Schema: 1, Revision: rev, Previous: previous}
	data, e := yaml.Marshal(pointer)
	if e != nil {
		return e
	}
	e = s.write(filepath.Join(s.dir, "current.yaml"), data)
	if e != nil { // If rename may have committed, keep its revision available and report uncertainty.
		disk, _ := os.ReadFile(filepath.Join(s.dir, "current.yaml"))
		var p Pointer
		if strictYAML(disk, &p) == nil && p.Revision == rev {
			committed = true
			s.pointer = pointer
			s.files = candidate
			s.invalid = true
		}
		return e
	}
	committed = true
	s.pointer = pointer
	s.files = candidate
	s.Problems = map[string]string{}
	s.invalid = false
	entries, _ := os.ReadDir(filepath.Dir(path))
	for _, entry := range entries {
		if entry.IsDir() && validRevision(entry.Name()) && entry.Name() != rev && entry.Name() != previous {
			os.RemoveAll(filepath.Join(s.dir, "revisions", entry.Name()))
		}
	}
	return nil
}
func Encode(value any) ([]byte, error)        { return yaml.Marshal(value) }
func DecodeStrict(data []byte, out any) error { return strictYAML(data, out) }
func (s *Rules) Register(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !plugins.ValidID(id) {
		return fmt.Errorf("invalid ID")
	}
	if _, ok := s.files[id]; ok {
		s.ids[id] = true
		return nil
	}
	if s.invalid {
		return fmt.Errorf("formal pointer unavailable")
	}
	path := filepath.Join(s.dir, "revisions", s.pointer.Revision, id+".yaml")
	if data, e := os.ReadFile(path); e == nil {
		f, e := ParseRules(data, id)
		if e != nil {
			return e
		}
		s.files[id] = f
		s.ids[id] = true
		return nil
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	candidate := map[string]RuleFile{}
	for k, v := range s.files {
		candidate[k] = v
	}
	candidate[id] = RuleFile{Schema: 1, ID: id, Rules: []router.Rule{}}
	if e := s.persist(candidate); e != nil {
		return e
	}
	s.ids[id] = true
	return nil
}
