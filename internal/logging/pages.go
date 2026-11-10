package logging

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// Pages retain open files and their original lengths, not the log contents.
// Rotation/unlink and new writes therefore cannot shift an existing page.
type pageSnapshot struct {
	mu      sync.Mutex
	files   []*os.File
	sizes   []int64
	filter  Filter
	created time.Time
	timer   *time.Timer
	closed  bool
}

func (p *pageSnapshot) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	if p.timer != nil {
		p.timer.Stop()
	}
	for _, f := range p.files {
		f.Close()
	}
}
func (l *Logger) closePages() {
	l.pagesMu.Lock()
	l.pagesClosed = true
	pages := l.pages
	l.pages = nil
	l.pagesMu.Unlock()
	for _, p := range pages {
		p.close()
	}
}

// ReadPage returns at most one requested page. Up to four snapshots, each with
// at most ten FDs, expire five minutes after creation; no background index/push.
func (l *Logger) ReadPage(filter Filter, id string) ([]string, bool, string, error) {
	if filter.Offset < 0 {
		return nil, false, "", fmt.Errorf("negative offset")
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 100
	}
	var p *pageSnapshot
	if id == "" {
		files, sizes, e := l.snapshot()
		if e != nil {
			return nil, false, "", e
		}
		p = &pageSnapshot{files: files, sizes: sizes, filter: filter, created: time.Now()}
		var random [24]byte
		if _, e = rand.Read(random[:]); e != nil {
			p.close()
			return nil, false, "", e
		}
		id = hex.EncodeToString(random[:])
		l.pagesMu.Lock()
		if l.pagesClosed {
			l.pagesMu.Unlock()
			p.close()
			return nil, false, "", fmt.Errorf("logger closed")
		}
		if l.pages == nil {
			l.pages = map[string]*pageSnapshot{}
		}
		var evicted *pageSnapshot
		if len(l.pages) >= 4 {
			var oldest string
			for k, v := range l.pages {
				if oldest == "" || v.created.Before(l.pages[oldest].created) {
					oldest = k
				}
			}
			evicted = l.pages[oldest]
			delete(l.pages, oldest)
		}
		l.pages[id] = p
		p.timer = time.AfterFunc(5*time.Minute, func() {
			l.pagesMu.Lock()
			if l.pages[id] == p {
				delete(l.pages, id)
			}
			l.pagesMu.Unlock()
			p.close()
		})
		l.pagesMu.Unlock()
		if evicted != nil {
			evicted.close()
		}
	} else {
		l.pagesMu.Lock()
		p = l.pages[id]
		l.pagesMu.Unlock()
		if p == nil {
			return nil, false, "", fmt.Errorf("日志快照已过期或被替换，请刷新获取最新日志")
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, false, "", fmt.Errorf("日志快照已过期，请刷新")
	}
	if filter.Level != p.filter.Level || filter.Plugin != p.filter.Plugin || filter.Keyword != p.filter.Keyword {
		return nil, false, "", fmt.Errorf("筛选条件已变化，请刷新日志")
	}
	rows, more, e := readFiles(p.files, p.sizes, filter)
	return rows, more, id, e
}

func readFiles(files []*os.File, sizes []int64, filter Filter) ([]string, bool, error) {
	var result []string
	matched := 0
	more := false
	for i := len(files) - 1; i >= 0; i-- {
		e := reverseLines(io.NewSectionReader(files[i], 0, sizes[i]), sizes[i], func(line string) bool {
			level, plugin := header(line)
			if (filter.Level != "" && filter.Level != level) || (filter.Plugin != "" && filter.Plugin != plugin) || !strings.Contains(line, filter.Keyword) {
				return true
			}
			matched++
			if matched <= filter.Offset {
				return true
			}
			if len(result) == filter.Limit {
				more = true
				return false
			}
			result = append(result, line)
			return true
		})
		if e != nil {
			return nil, false, e
		}
		if more {
			break
		}
	}
	return result, more, nil
}

// Scan from EOF in bounded blocks and stop once the requested page is full.
// A very long record is assembled whole, preserving the existing no-truncation contract.
func reverseLines(r io.ReaderAt, size int64, emit func(string) bool) error {
	if size == 0 {
		return nil
	}
	var last [1]byte
	if _, e := r.ReadAt(last[:], size-1); e != nil {
		return e
	}
	terminated := last[0] == '\n'
	pos := size
	if terminated {
		pos--
	}
	var parts [][]byte
	line := func(head []byte) bool {
		var b strings.Builder
		length := len(head)
		for _, part := range parts {
			length += len(part)
		}
		if terminated {
			length++
		}
		b.Grow(length)
		b.Write(head)
		for j := len(parts) - 1; j >= 0; j-- {
			b.Write(parts[j])
		}
		if terminated {
			b.WriteByte('\n')
		}
		parts = nil
		terminated = true
		return emit(b.String())
	}
	for pos > 0 {
		n := int64(32 << 10)
		if n > pos {
			n = pos
		}
		pos -= n
		block := make([]byte, n)
		if _, e := r.ReadAt(block, pos); e != nil {
			return e
		}
		right := len(block)
		for {
			j := bytes.LastIndexByte(block[:right], '\n')
			if j < 0 {
				break
			}
			if !line(block[j+1 : right]) {
				return nil
			}
			right = j
		}
		if right > 0 {
			parts = append(parts, block[:right])
		}
	}
	line(nil)
	return nil
}
