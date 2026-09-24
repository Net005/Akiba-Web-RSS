package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// LogEntry is one line in the live log.
type LogEntry struct {
	ID    int64  `json:"id"`
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// Hub is a log sink: stdout + rotating file + in-memory ring buffer +
// fan-out to live subscribers (SSE).
type Hub struct {
	mu      sync.Mutex
	buf     []LogEntry
	max     int
	nextID  int64
	subs    map[chan LogEntry]struct{}
	file    *os.File
	path    string
	written int64
	out     io.Writer
}

const logRotateBytes = 10 * 1024 * 1024

func NewHub(max int) *Hub {
	return &Hub{max: max, subs: map[chan LogEntry]struct{}{}, out: os.Stdout}
}

// OpenFile attaches the rotating file and preloads its tail into the buffer
// so the UI shows history after a restart.
func (h *Hub) OpenFile(path string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.path = path
	h.preloadTail(path, 300)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if st, err := f.Stat(); err == nil {
		h.written = st.Size()
	}
	h.file = f
	return nil
}

func (h *Hub) preloadTail(path string, n int) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	const window = 96 * 1024
	off := int64(0)
	if st.Size() > window {
		off = st.Size() - window
	}
	_, _ = f.Seek(off, io.SeekStart)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var lines []string
	first := off > 0
	for sc.Scan() {
		if first { // partial line
			first = false
			continue
		}
		lines = append(lines, sc.Text())
	}
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	for _, l := range lines {
		e := parseLogLine(l)
		h.nextID++
		e.ID = h.nextID
		h.buf = append(h.buf, e)
	}
}

func parseLogLine(l string) LogEntry {
	// "2026-09-24 20:03:56 [ERROR] message"
	if len(l) > 26 && l[4] == '-' && l[19] == ' ' && l[20] == '[' {
		if end := strings.Index(l[20:], "]"); end > 0 {
			return LogEntry{Time: l[:19], Level: l[21 : 20+end], Msg: strings.TrimSpace(l[20+end+1:])}
		}
	}
	return LogEntry{Time: "", Level: "RAW", Msg: stripANSI(l)}
}

func stripANSI(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == 0x1b && i+1 < len(s) && s[i+1] == '[' {
			j := i + 2
			for j < len(s) && (s[j] < 0x40 || s[j] > 0x7e) {
				j++
			}
			i = j
			continue
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

func (h *Hub) Log(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now().Format("2006-01-02 15:04:05")
	line := fmt.Sprintf("%s [%s] %s\n", now, level, msg)
	h.mu.Lock()
	h.nextID++
	e := LogEntry{ID: h.nextID, Time: now, Level: level, Msg: msg}
	h.buf = append(h.buf, e)
	if len(h.buf) > h.max {
		h.buf = h.buf[len(h.buf)-h.max:]
	}
	fmt.Fprint(h.out, line)
	if h.file != nil {
		n, _ := h.file.WriteString(line)
		h.written += int64(n)
		if h.written > logRotateBytes {
			h.file.Close()
			_ = os.Rename(h.path, h.path+".1")
			h.file, _ = os.OpenFile(h.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			h.written = 0
		}
	}
	for ch := range h.subs {
		select {
		case ch <- e:
		default: // slow consumer: drop
		}
	}
	h.mu.Unlock()
}

func (h *Hub) Info(f string, a ...any)  { h.Log("INFO", f, a...) }
func (h *Hub) Warn(f string, a ...any)  { h.Log("WARNING", f, a...) }
func (h *Hub) Error(f string, a ...any) { h.Log("ERROR", f, a...) }
func (h *Hub) Debug(f string, a ...any) { h.Log("DEBUG", f, a...) }

// Snapshot returns up to limit entries with ID > since.
func (h *Hub) Snapshot(since int64, limit int) []LogEntry {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]LogEntry, 0, 64)
	for _, e := range h.buf {
		if e.ID > since {
			out = append(out, e)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

func (h *Hub) Subscribe() (chan LogEntry, func()) {
	ch := make(chan LogEntry, 256)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

// Clear empties the in-memory buffer (file is untouched).
func (h *Hub) Clear() {
	h.mu.Lock()
	h.buf = nil
	h.mu.Unlock()
}

func (h *Hub) FilePath() string { return h.path }
