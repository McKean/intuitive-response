// Package logx provides the JSON-lines log, header redaction, counters, and traffic capture.
package logx

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Logger struct {
	mu sync.Mutex
	w  io.Writer
}

func NewLogger(w io.Writer) *Logger { return &Logger{w: w} }

// OpenFile opens (appending) the log file, creating its directory.
func OpenFile(path string) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}

// Log writes one JSON line: {"ts":..., "event":..., ...fields}.
func (l *Logger) Log(event string, fields map[string]any) {
	rec := make(map[string]any, len(fields)+2)
	maps.Copy(rec, fields)
	rec["ts"] = time.Now().UTC().Format(time.RFC3339Nano)
	rec["event"] = event
	b, err := json.Marshal(rec)
	if err != nil {
		b = fmt.Appendf(nil, `{"event":"log_error","error":%q}`, err.Error())
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	_, _ = l.w.Write(append(b, '\n'))
}

var secretHeaders = map[string]bool{
	"authorization":       true,
	"x-api-key":           true,
	"proxy-authorization": true,
	"cookie":              true,
	"set-cookie":          true,
}

// RedactHeaders flattens headers for logging, masking credentials.
func RedactHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, v := range h {
		val := strings.Join(v, ", ")
		if secretHeaders[strings.ToLower(k)] {
			val = redact(val)
		}
		out[k] = val
	}
	return out
}

func redact(v string) string {
	if len(v) <= 12 {
		return "[redacted]"
	}
	return v[:8] + "…[redacted]"
}

// Stats is a set of named monotonic counters.
type Stats struct {
	m sync.Map // string -> *atomic.Int64
}

func (s *Stats) Add(name string, n int) {
	if n == 0 {
		return
	}
	v, _ := s.m.LoadOrStore(name, new(atomic.Int64))
	v.(*atomic.Int64).Add(int64(n))
}

func (s *Stats) Inc(name string) { s.Add(name, 1) }

func (s *Stats) Snapshot() map[string]int64 {
	out := map[string]int64{}
	s.m.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

// Traffic writes full request/response captures for offline evaluation (IRP_TRAFFIC_LOG=1).
type Traffic struct {
	dir string
	n   atomic.Uint64
}

func NewTraffic(dir string) (*Traffic, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Traffic{dir: dir}, nil
}

type Capture struct {
	Time           time.Time         `json:"time"`
	Session        string            `json:"session"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	RequestHeaders map[string]string `json:"request_headers"`
	RequestBody    json.RawMessage   `json:"request_body,omitempty"`
	Status         int               `json:"status"`
	ResponseBody   string            `json:"response_body"`
	ClientResponse string            `json:"client_response,omitempty"` // what Claude Code saw, when it differs
}

func (t *Traffic) Write(c Capture) {
	if t == nil {
		return
	}
	if !json.Valid(c.RequestBody) {
		c.RequestBody = nil
	}
	name := fmt.Sprintf("%s-%06d.json", c.Time.UTC().Format("20060102T150405.000"), t.n.Add(1))
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(t.dir, name), b, 0o600)
}
