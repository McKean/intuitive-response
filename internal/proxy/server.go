// Package proxy forwards Claude Code traffic to the Anthropic API, injecting the preset
// instruction into main-model requests and stripping preset blocks from their responses.
package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/McKean/intuitive-response/internal/config"
	"github.com/McKean/intuitive-response/internal/hidden"
	"github.com/McKean/intuitive-response/internal/jev"
	"github.com/McKean/intuitive-response/internal/logx"
	"github.com/McKean/intuitive-response/internal/presets"
)

type Server struct {
	cfg      config.Config
	upstream *url.URL
	client   *http.Client
	store    *presets.Store
	log      *logx.Logger
	stats    *logx.Stats
	traffic  *logx.Traffic // nil unless IRP_TRAFFIC_LOG=1
	jev      *jev.Client   // nil without a TypeSafe API key: no matching
	hidden   *hidden.Store // nil when hidden memory is off

	jevFailures atomic.Int32 // consecutive Jev timeouts/errors, for the console warning
}

// WarmJev opens a connection to TypeSafe so the first match doesn't pay for TLS setup.
func (s *Server) WarmJev() {
	if s.jev != nil {
		s.jev.Warm()
	}
}

func New(cfg config.Config, store *presets.Store, log *logx.Logger, stats *logx.Stats, traffic *logx.Traffic) (*Server, error) {
	u, err := url.Parse(cfg.Upstream)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("invalid upstream %q", cfg.Upstream)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	var jc *jev.Client
	if cfg.TypeSafeAPIKey != "" {
		jc = jev.New(cfg.JevURL, cfg.TypeSafeAPIKey, cfg.JevModel)
	}
	var hs *hidden.Store
	if cfg.HiddenMemory {
		if hs, err = hidden.New(filepath.Join(cfg.StateDir, "hidden")); err != nil {
			return nil, err
		}
	}
	return &Server{
		jev:      jc,
		hidden:   hs,
		cfg:      cfg,
		upstream: u,
		client:   &http.Client{Transport: tr}, // no overall timeout: streams can run for minutes
		store:    store,
		log:      log,
		stats:    stats,
		traffic:  traffic,
	}, nil
}

// hopHeaders are connection-scoped and never forwarded. Accept-Encoding is dropped too so
// Go's transport negotiates gzip itself and hands us plaintext to inspect.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	"Host", "Content-Length", "Accept-Encoding",
}

func copyHeaders(dst, src http.Header) {
	for k, v := range src {
		dst[k] = append([]string(nil), v...)
	}
	for _, h := range hopHeaders {
		dst.Del(h)
	}
}

// ServeHTTP handles every non-admin request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "reading request body: "+err.Error())
		return
	}
	s.stats.Inc("requests")

	var info reqInfo
	var live []presets.Preset // presets as they were before this turn's decrement
	process := false
	restored := 0 // assistant messages with a stripped block restored
	if r.Method == http.MethodPost && r.URL.Path == "/v1/messages" {
		if info, err = analyze(body, r.Header); err != nil {
			s.log.Log("analyze_error", map[string]any{"error": err.Error()})
		} else if info.Main {
			tracked := false
			if s.hidden != nil {
				if tracked, err = s.hidden.Observe(info.SessionID, info.AssistantTurns == 0); err != nil {
					s.log.Log("hidden_error", map[string]any{"session": info.SessionID, "error": err.Error()})
				}
				// Let the previous reply's stripped block (and its presets) land first.
				if !s.hidden.Wait(info.SessionID, hiddenWait) {
					s.log.Log("hidden_wait_timeout", map[string]any{"session": info.SessionID})
				}
			}
			if info.UserTurn {
				if s.jev != nil {
					live = s.store.List(info.SessionID)
				}
				c := s.store.UserTurn(info.SessionID)
				s.stats.Add("presets_expired", c.Expired)
			}
			if tracked {
				if nb, n, err := s.restoreHidden(body, info.SessionID); err != nil {
					s.log.Log("hidden_error", map[string]any{"session": info.SessionID, "error": err.Error()})
				} else {
					body, restored = nb, n
				}
			}
			if s.cfg.Inject {
				if nb, err := injectInstruction(body); err != nil {
					s.log.Log("inject_error", map[string]any{"error": err.Error()})
				} else {
					body, process = nb, true
				}
			}
		}
	}

	// The upstream call gets its own context: after an early close the handler returns
	// while a goroutine keeps reading the rest of the response. Until then, a client
	// disconnect still cancels it.
	ctx, cancel := context.WithCancel(context.Background())
	stopWatch := context.AfterFunc(r.Context(), cancel)
	detached := false
	defer func() {
		if !detached {
			stopWatch()
			cancel()
		}
	}()

	u := *s.upstream
	u.Path = strings.TrimRight(u.Path, "/") + r.URL.Path
	u.RawQuery = r.URL.RawQuery
	req, err := http.NewRequestWithContext(ctx, r.Method, u.String(), bytes.NewReader(body))
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	copyHeaders(req.Header, r.Header)

	// Matching and the upstream request run in parallel. On a live hit the upstream request
	// is cancelled and the preset is served; on a miss its response is used as-is, so a miss
	// only costs the time Jev takes beyond upstream's response headers.
	var matchCh chan matchResult
	if len(live) > 0 {
		if s.cfg.MatchMode == "live" && info.Stream && process {
			matchCh = make(chan matchResult, 1)
			go func() { matchCh <- s.match(context.Background(), info, live) }()
		} else {
			go func() { s.logMatch(info, live, s.match(context.Background(), info, live), true) }()
		}
	}
	type upResult struct {
		resp *http.Response
		err  error
	}
	upCh := make(chan upResult, 1)
	go func() {
		resp, err := s.client.Do(req)
		upCh <- upResult{resp, err}
	}()
	if matchCh != nil {
		m := <-matchCh
		s.logMatch(info, live, m, false)
		if m.Hit {
			cancel()
			go func() {
				if u := <-upCh; u.resp != nil {
					u.resp.Body.Close()
				}
			}()
			s.store.Take(m.Preset.ID)
			s.serveHit(w, r, body, info, *m.Preset, start)
			return
		}
	}
	up := <-upCh
	resp, err := up.resp, up.err
	if err != nil {
		s.log.Log("upstream_error", map[string]any{"path": r.URL.Path, "error": err.Error()})
		writeError(w, http.StatusBadGateway, "upstream: "+err.Error())
		return
	}

	var cap *captureBuf
	if s.traffic != nil {
		cap = &captureBuf{c: logx.Capture{
			Time: start, Session: info.SessionID, Method: r.Method, Path: r.URL.RequestURI(),
			RequestHeaders: logx.RedactHeaders(r.Header), RequestBody: body, Status: resp.StatusCode,
		}}
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, &cap.up), resp.Body}
	}

	copyHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	out := newClientWriter(w, cap)

	ct := resp.Header.Get("Content-Type")
	fields := map[string]any{
		"method": r.Method, "path": r.URL.Path, "status": resp.StatusCode,
		"model": info.Model, "session": info.SessionID, "main": info.Main,
		"user_turn": info.UserTurn, "injected": process, "restored": restored,
	}
	finish := func() {
		resp.Body.Close()
		stopWatch()
		cancel()
		cap.write(s.traffic)
	}
	switch {
	case process && resp.StatusCode == http.StatusOK && strings.HasPrefix(ct, "text/event-stream"):
		// stopWatch detaches the upstream call from the client before the early close,
		// so the drain goroutine can read the rest of the presets block.
		st := &streamState{srv: s, info: info, out: out, detach: func() { stopWatch() }}
		detached = st.run(resp.Body, finish)
		fields["early_close"] = detached
	case process && resp.StatusCode == http.StatusOK && strings.HasPrefix(ct, "application/json"):
		s.handleJSON(out, resp.Body, info)
	default:
		out.copyFrom(resp.Body)
	}
	if !detached {
		finish()
	}
	fields["ms"] = time.Since(start).Milliseconds()
	s.log.Log("request", fields)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	body, _ := marshal(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": "api_error", "message": "irp: " + msg},
	})
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// clientWriter writes to the client, flushing after each write, and optionally records
// what the client received.
type clientWriter struct {
	w   http.ResponseWriter
	rc  *http.ResponseController
	cap *captureBuf
	err error
}

func newClientWriter(w http.ResponseWriter, cap *captureBuf) *clientWriter {
	return &clientWriter{w: w, rc: http.NewResponseController(w), cap: cap}
}

func (c *clientWriter) write(b []byte) {
	if c.err != nil || len(b) == 0 {
		return
	}
	if c.cap != nil {
		c.cap.client.Write(b)
	}
	if _, c.err = c.w.Write(b); c.err == nil {
		c.err = c.rc.Flush()
	}
}

func (c *clientWriter) copyFrom(r io.Reader) {
	buf := make([]byte, 32*1024)
	for c.err == nil {
		n, err := r.Read(buf)
		c.write(buf[:n])
		if err != nil {
			return
		}
	}
}

type captureBuf struct {
	c      logx.Capture
	up     bytes.Buffer
	client bytes.Buffer
}

func (b *captureBuf) write(t *logx.Traffic) {
	if b == nil {
		return
	}
	b.c.ResponseBody = b.up.String()
	if !bytes.Equal(b.up.Bytes(), b.client.Bytes()) {
		b.c.ClientResponse = b.client.String()
	}
	t.Write(b.c)
}
