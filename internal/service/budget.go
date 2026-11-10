package service

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"github.com/ZhenForest/xsbot-core/internal/config"
)

type Budget struct {
	mu   sync.Mutex
	path string
}
type ledger struct {
	Schema int  `yaml:"schema_version"`
	Used   int  `yaml:"used"`
	Manual bool `yaml:"manual"`
}

func Open(root string) *Budget {
	return &Budget{path: filepath.Join(root, "data/runtime/retries.yaml")}
}
func (b *Budget) read() (ledger, error) {
	data, e := os.ReadFile(b.path)
	if e != nil {
		return ledger{}, e
	}
	var l ledger
	if e = config.DecodeStrict(data, &l); e != nil {
		return l, e
	}
	if l.Schema != 1 || l.Used < -1 || l.Used > 3 {
		return l, fmt.Errorf("invalid retry ledger")
	}
	return l, nil
}
func (b *Budget) save(l ledger) error {
	data, e := config.Encode(l)
	if e != nil {
		return e
	}
	return config.WriteAtomic(b.path, data)
}
func (b *Budget) Reset() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if e := os.MkdirAll(filepath.Dir(b.path), 0700); e != nil {
		return e
	}
	return b.save(ledger{Schema: 1, Used: -1})
}
func (b *Budget) Condition() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, e := b.read()
	if e != nil {
		return e
	}
	if !l.Manual {
		if l.Used >= 3 {
			return fmt.Errorf("unready retry budget exhausted; manual reset required")
		}
		l.Used++
	}
	l.Manual = false
	return b.save(l)
}
func (b *Budget) Ready() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, e := b.read()
	if e != nil {
		return e
	}
	l.Used = 0
	l.Manual = false
	if e = b.save(l); e != nil {
		return e
	}
	return Notify("READY=1")
}
func (b *Budget) ManualRestart() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, e := b.read()
	if e != nil {
		return e
	}
	l.Manual = true
	return b.save(l)
}
