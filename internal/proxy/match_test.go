package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/McKean/intuitive-response/internal/config"
	"github.com/McKean/intuitive-response/internal/jev"
	"github.com/McKean/intuitive-response/internal/logx"
	"github.com/McKean/intuitive-response/internal/presets"
)

func testPresets() []presets.Preset {
	return []presets.Preset{
		{ID: "a", ExpectedInput: "asks why not SQLite", Response: "SQLite serializes writes."},
		{ID: "b", ExpectedInput: "asks how to migrate", Response: "Run make migrate.", MinConfidence: 0.95},
	}
}

func jevResp(choice string, conf float64, nouls ...float64) *jev.Response {
	r := &jev.Response{Model: "jev-1.13.0", Answers: map[string]jev.Answer{
		"match": {Type: "choice", Choice: choice, Confidence: conf},
	}}
	for i, n := range nouls {
		r.Answers[fullKey(i)] = jev.Answer{Type: "noul", Noul: n}
	}
	return r
}

func TestDecide(t *testing.T) {
	s := &Server{cfg: config.Default()} // min confidence 0.8, full-answer threshold 0.8
	cases := []struct {
		resp   *jev.Response
		reason string
		preset string
	}{
		{jevResp("preset_1", 0.9, 0.85, 0.1), "hit", "a"},
		{jevResp("none", 0.99, 0.9, 0.9), "none", ""},
		{jevResp("preset_1", 0.7, 0.99, 0.1), "low_confidence", "a"},
		{jevResp("preset_1", 0.9, 0.79, 0.1), "not_full_answer", "a"},
		{jevResp("preset_2", 0.9, 0.1, 0.99), "low_confidence", "b"}, // preset's own 0.95 floor
		{jevResp("preset_2", 0.97, 0.1, 0.99), "hit", "b"},
		{jevResp("preset_7", 0.97), "error", ""},
	}
	for i, c := range cases {
		r := s.decide(matchResult{Response: c.resp}, testPresets())
		got := ""
		if r.Preset != nil {
			got = r.Preset.ID
		}
		if r.Reason != c.reason || got != c.preset || r.Hit != (c.reason == "hit") {
			t.Errorf("case %d: reason %q preset %q hit %v", i, r.Reason, got, r.Hit)
		}
	}
}

func TestBuildMatchRequest(t *testing.T) {
	state, qs := buildMatchRequest(reqInfo{UserText: "why not sqlite?", PrevAssistant: "Use Postgres."}, testPresets())
	st := state.(map[string]string)
	if st["user_message"] != "why not sqlite?" || st["previous_assistant_answer"] != "Use Postgres." {
		t.Fatalf("state %v", st)
	}
	crit := qs["match"].Criteria.(map[string]any)
	if len(crit) != 3 || crit["preset_1"] != "asks why not SQLite" || crit[noneOption] == nil {
		t.Fatalf("criteria %v", crit)
	}
	if qs["full_2"].Type != "noul" || len(qs) != 3 {
		t.Fatalf("questions %v", qs)
	}
}

type jevStub struct {
	mu     sync.Mutex
	bodies [][]byte
	delay  time.Duration
	reply  string
}

func (j *jevStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	j.mu.Lock()
	j.bodies = append(j.bodies, b)
	j.mu.Unlock()
	if r.Header.Get("Authorization") != "Bearer ts-key" || r.URL.Path != "/v1/systemone" {
		w.WriteHeader(401)
		return
	}
	time.Sleep(j.delay)
	w.Header().Set("Content-Type", "application/json")
	io.WriteString(w, j.reply)
}

func setupJev(t *testing.T, js *jevStub, timeout time.Duration) (*httptest.Server, *presets.Store, *logx.Stats, *strings.Builder) {
	return setupJevWith(t, js, http.HandlerFunc(sseStream([]string{"Because."}, "end_turn", nil)), func(c *config.Config) {
		c.JevTimeout, c.MatchMode = timeout, "shadow"
	})
}

func setupJevWith(t *testing.T, js *jevStub, upstream http.Handler, configure func(*config.Config)) (*httptest.Server, *presets.Store, *logx.Stats, *strings.Builder) {
	t.Helper()
	jsrv := httptest.NewServer(js)
	t.Cleanup(jsrv.Close)
	up := httptest.NewServer(upstream)
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Upstream, cfg.JevURL, cfg.TypeSafeAPIKey, cfg.StateDir = up.URL, jsrv.URL, "ts-key", t.TempDir()
	configure(&cfg)
	store := presets.NewStore(10, "ring")
	stats := &logx.Stats{}
	var logBuf strings.Builder
	var mu sync.Mutex
	px, err := New(cfg, store, logx.NewLogger(lockedWriter{&mu, &logBuf}), stats, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(px)
	t.Cleanup(srv.Close)
	return srv, store, stats, &logBuf
}

type lockedWriter struct {
	mu *sync.Mutex
	b  *strings.Builder
}

func (l lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func TestShadowMatchEndToEnd(t *testing.T) {
	js := &jevStub{reply: `{"model":"jev-1.13.0","answers":{"match":{"type":"choice","choice":"preset_1","confidence":0.93,"probabilities":{"preset_1":0.95,"none":0.05}},"full_1":{"type":"noul","noul":0.9}},"usage":{"input_tokens":300,"output_tokens":20}}`}
	srv, store, stats, _ := setupJev(t, js, time.Second)
	store.AddFromModel("sess-1", 1, []presets.ModelPreset{{ExpectedInput: "asks which db", Response: "Postgres.", MaxTurns: 1}})

	_, body := post(t, srv.URL+"/v1/messages", mainReq)
	if text, _ := clientText(t, body); text != "Because." {
		t.Fatalf("shadow mode changed the response: %q", text)
	}
	waitFor(t, func() bool { return stats.Snapshot()["match_hits"] == 1 })

	js.mu.Lock()
	defer js.mu.Unlock()
	var sent struct {
		Model     string                     `json:"model"`
		State     map[string]string          `json:"state"`
		Questions map[string]json.RawMessage `json:"questions"`
	}
	json.Unmarshal(js.bodies[0], &sent)
	if sent.Model != "jev-1.13.0" || sent.State["user_message"] != "which db?" || len(sent.Questions) != 2 {
		t.Fatalf("sent %s", js.bodies[0])
	}
	// The preset had max_turns 1, so this user turn expired it; shadow hits don't consume.
	if store.Len("sess-1") != 0 {
		t.Fatal("preset should have expired after one user turn")
	}
}

func TestShadowMatchTimeoutAndNoCandidates(t *testing.T) {
	js := &jevStub{delay: 200 * time.Millisecond, reply: `{}`}
	srv, store, stats, logBuf := setupJev(t, js, 20*time.Millisecond)

	post(t, srv.URL+"/v1/messages", mainReq) // no live presets: Jev is not called
	store.AddFromModel("sess-1", 1, []presets.ModelPreset{{ExpectedInput: "x", Response: "y", MaxTurns: 2}})
	post(t, srv.URL+"/v1/messages", mainReq)
	waitFor(t, func() bool { return stats.Snapshot()["jev_timeouts"] == 1 })
	if n := stats.Snapshot()["match_candidates"]; n != 1 {
		t.Fatalf("candidates = %d", n)
	}
	_ = logBuf
}
