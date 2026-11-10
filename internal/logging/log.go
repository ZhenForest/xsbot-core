package logging

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Logger struct {
	mu          sync.Mutex
	dir         string
	file        *os.File
	size        int64
	debug       bool
	pagesMu     sync.Mutex
	pages       map[string]*pageSnapshot
	pagesClosed bool
}

func Fallback(dir, level string) *Logger { return &Logger{dir: dir, debug: level == "DEBUG"} }
func New(dir, level string) (*Logger, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, e
	}
	l := &Logger{dir: dir, debug: level == "DEBUG"}
	f, e := os.OpenFile(filepath.Join(dir, "core.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return nil, e
	}
	l.file = f
	st, e := f.Stat()
	if e != nil {
		f.Close()
		return nil, e
	}
	l.size = st.Size()
	return l, nil
}
func (l *Logger) Write(level, id, message string) {
	if level == "DEBUG" && !l.debug {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	zone, e := time.LoadLocation("Asia/Shanghai")
	if e != nil {
		zone = time.FixedZone("CST", 8*3600)
	}
	line := fmt.Sprintf("%s %-5s plugin=%s %s\n", time.Now().In(zone).Format(time.RFC3339Nano), level, fmt.Sprintf("%q", id), fmt.Sprintf("%q", message))
	fmt.Fprint(os.Stderr, line)
	if l.file == nil {
		return
	}
	n, e := io.WriteString(l.file, line)
	l.size += int64(n)
	if e != nil {
		fmt.Fprintln(os.Stderr, "log write failed:", e)
		return
	}
	if l.size > 10<<20 {
		if e = l.rotate(); e != nil {
			fmt.Fprintln(os.Stderr, "log rotation failed:", e)
		}
	}
}
func (l *Logger) rotate() error {
	if e := l.file.Close(); e != nil {
		return e
	}
	l.file = nil
	os.Remove(filepath.Join(l.dir, "core.log.9"))
	for i := 8; i >= 1; i-- {
		old := filepath.Join(l.dir, fmt.Sprintf("core.log.%d", i))
		if _, e := os.Stat(old); e == nil {
			if e = os.Rename(old, filepath.Join(l.dir, fmt.Sprintf("core.log.%d", i+1))); e != nil {
				return e
			}
		}
	}
	if e := os.Rename(filepath.Join(l.dir, "core.log"), filepath.Join(l.dir, "core.log.1")); e != nil {
		return e
	}
	f, e := os.OpenFile(filepath.Join(l.dir, "core.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	l.file = f
	l.size = 0
	return nil
}
func (l *Logger) Close() error {
	l.closePages()
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file != nil {
		e := l.file.Close()
		l.file = nil
		return e
	}
	return nil
}

type Filter struct {
	Level, Plugin, Keyword string
	Offset, Limit          int
}

// Snapshot pins currently opened files before rotation can rename or unlink them.
func (l *Logger) snapshot() ([]*os.File, []int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var files []*os.File
	var sizes []int64
	for i := 9; i >= 0; i-- {
		name := "core.log"
		if i > 0 {
			name = fmt.Sprintf("core.log.%d", i)
		}
		f, e := openSnapshotFile(filepath.Join(l.dir, name))
		if os.IsNotExist(e) {
			continue
		}
		if e != nil {
			for _, open := range files {
				open.Close()
			}
			return nil, nil, e
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			for _, open := range files {
				open.Close()
			}
			return nil, nil, e
		}
		files = append(files, f)
		sizes = append(sizes, st.Size())
	}
	return files, sizes, nil
}
func (l *Logger) Download(w io.Writer) error {
	files, sizes, e := l.snapshot()
	if e != nil {
		return e
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for i, f := range files {
		if _, e = io.CopyN(w, f, sizes[i]); e != nil {
			return e
		}
	}
	return nil
}
func (l *Logger) Read(filter Filter) ([]string, bool, error) {
	if filter.Offset < 0 {
		return nil, false, fmt.Errorf("negative offset")
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 100
	}
	files, sizes, e := l.snapshot()
	if e != nil {
		return nil, false, e
	}
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	return readFiles(files, sizes, filter)
}
func header(line string) (string, string) {
	parts := strings.SplitN(line, " ", 3)
	if len(parts) != 3 {
		return "", ""
	}
	rest := strings.TrimLeft(parts[2], " ")
	if !strings.HasPrefix(rest, "plugin=") {
		return "", ""
	}
	quoted, e := strconv.QuotedPrefix(strings.TrimPrefix(rest, "plugin="))
	if e != nil {
		return "", ""
	}
	plugin, e := strconv.Unquote(quoted)
	if e != nil {
		return "", ""
	}
	return parts[1], plugin
}
