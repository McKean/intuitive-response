package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/McKean/intuitive-response/internal/config"
	"github.com/McKean/intuitive-response/internal/logx"
	"github.com/McKean/intuitive-response/internal/presets"
	"github.com/McKean/intuitive-response/internal/sse"
)

type upstreamStub struct {
	mu       sync.Mutex
	lastBody []byte
	lastURL  string
	lastHdr  http.Header
	respond  func(w http.ResponseWriter, r *http.Request)
}

func (u *upstreamStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	u.lastBody, u.lastURL, u.lastHdr = b, r.URL.String(), r.Header.Clone()
	u.mu.Unlock()
	u.respond(w, r)
}

func setup(t *testing.T, respond func(w http.ResponseWriter, r *http.Request)) (*upstreamStub, *httptest.Server, *presets.Store) {
	t.Helper()
	stub := &upstreamStub{respond: respond}
	up := httptest.NewServer(stub)
	t.Cleanup(up.Close)
	cfg := config.Default()
	cfg.Upstream, cfg.StateDir = up.URL, t.TempDir()
	store := presets.NewStore(cfg.MaxPresets, cfg.ReplaceMode)
	px, err := New(cfg, store, logx.NewLogger(io.Discard), &logx.Stats{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(px)
	t.Cleanup(srv.Close)
	return stub, srv, store
}

// waitDelta in a delta list makes sseStream block until the test's release channel closes.
const waitDelta = "\x00wait"

// sseStream writes a Messages stream whose single text block is split into the given deltas.
func sseStream(deltas []string, stopReason string, extra func(w io.Writer), release ...chan struct{}) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		write := func(name, data string) {
			w.Write(sse.Encode(name, []byte(data)))
			fl.Flush()
		}
		write("message_start", `{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"claude-opus-5-5","usage":{"input_tokens":10,"output_tokens":1}}}`)
		write("content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`)
		write("content_block_stop", `{"type":"content_block_stop","index":0}`)
		write("content_block_start", `{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`)
		write("ping", `{"type": "ping"}`)
		for _, d := range deltas {
			if d == waitDelta {
				<-release[0]
				continue
			}
			b, _ := json.Marshal(d)
			write("content_block_delta", fmt.Sprintf(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":%s}}`, b))
		}
		write("content_block_stop", `{"type":"content_block_stop","index":1}`)
		if extra != nil {
			extra(w)
		}
		write("message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":99}}`, stopReason))
		write("message_stop", `{"type":"message_stop"}`)
	}
}

const mainReq = `{"model":"claude-opus-5-5","stream":true,"system":[{"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"which db?"}]}]}`

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req.Header.Set("x-api-key", "sk-test")
	req.Header.Set("X-Claude-Code-Session-Id", "sess-1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return resp, string(b)
}

// clientText reassembles the text of block 1 and returns the event types seen.
func clientText(t *testing.T, body string) (string, []string) {
	t.Helper()
	rd := sse.NewReader(strings.NewReader(body))
	var text strings.Builder
	var types []string
	for {
		ev, err := rd.Next()
		if err != nil {
			break
		}
		types = append(types, ev.Type())
		var e streamEvent
		json.Unmarshal(ev.Data, &e)
		if ev.Type() == "content_block_delta" && e.Index == 1 {
			text.WriteString(e.Delta.Text)
		}
	}
	return text.String(), types
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for range 200 {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met")
}

func TestStreamStripsPresetsAndClosesEarly(t *testing.T) {
	// The rest of the block arrives only after the client has its message_stop and has
	// hung up, so the drain must survive the client's request context ending.
	release := make(chan struct{})
	deltas := []string{"Use Post", "gres.\n", "\n<pre", "sets>", `[{"expected_input":"asks why not SQLite",`, waitDelta, `"response":"Because SQLite locks.","max_turns":2}]</presets>`}
	stub, srv, store := setup(t, sseStream(deltas, "end_turn", nil, release))

	done := make(chan struct{})
	var body string
	var resp *http.Response
	go func() {
		resp, body = post(t, srv.URL+"/v1/messages?beta=true", mainReq)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client response did not complete before upstream finished")
	}
	time.Sleep(50 * time.Millisecond) // let the server see the handler return
	close(release)

	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	text, types := clientText(t, body)
	if text != "Use Postgres." {
		t.Fatalf("client text %q", text)
	}
	if strings.Contains(body, "presets") {
		t.Fatalf("presets leaked to client:\n%s", body)
	}
	if got := types[len(types)-3:]; fmt.Sprint(got) != "[content_block_stop message_delta message_stop]" {
		t.Fatalf("tail events %v", got)
	}

	waitFor(t, func() bool { return store.Len("sess-1") == 1 })
	p := store.List("sess-1")[0]
	if p.ExpectedInput != "asks why not SQLite" || p.TurnsLeft != 2 || p.AnchorTurn != 1 || p.Source != "model" {
		t.Fatalf("preset %+v", p)
	}

	// Upstream saw the query string, auth, and the injected instruction as the last system block.
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if stub.lastURL != "/v1/messages?beta=true" || stub.lastHdr.Get("x-api-key") != "sk-test" {
		t.Fatalf("url %q hdr %v", stub.lastURL, stub.lastHdr)
	}
	var sent struct {
		System []struct {
			Text         string          `json:"text"`
			CacheControl json.RawMessage `json:"cache_control"`
		} `json:"system"`
	}
	json.Unmarshal(stub.lastBody, &sent)
	if len(sent.System) != 2 || sent.System[1].Text != Instruction || sent.System[0].CacheControl == nil {
		t.Fatalf("system %+v", sent.System)
	}
}

func TestStreamWithoutPresetsIsUnchanged(t *testing.T) {
	_, srv, store := setup(t, sseStream([]string{"Hello ", "world.\n\nBye"}, "end_turn", nil))
	_, body := post(t, srv.URL+"/v1/messages", mainReq)
	text, types := clientText(t, body)
	if text != "Hello world.\n\nBye" || types[len(types)-1] != "message_stop" || store.Len("sess-1") != 0 {
		t.Fatalf("text %q types %v", text, types)
	}
	if !strings.Contains(body, `"output_tokens":99`) {
		t.Fatal("upstream message_delta not forwarded")
	}
}

func TestPresetsDiscardedWhenTurnEndsInToolUse(t *testing.T) {
	deltas := []string{"Checking.\n<presets>[{\"expected_input\":\"a\",\"response\":\"b\"}]</presets>"}
	_, srv, store := setup(t, sseStream(deltas, "tool_use", func(w io.Writer) {
		w.Write(sse.Encode("content_block_start", []byte(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`)))
	}))
	_, body := post(t, srv.URL+"/v1/messages", mainReq)
	if text, _ := clientText(t, body); text != "Checking." {
		t.Fatalf("text %q", text)
	}
	time.Sleep(50 * time.Millisecond)
	if store.Len("sess-1") != 0 {
		t.Fatal("preset stored despite tool_use")
	}
}

func TestModifyingToolInvalidatesAndUserTurnExpires(t *testing.T) {
	_, srv, store := setup(t, sseStream([]string{"Editing."}, "tool_use", func(w io.Writer) {
		w.Write(sse.Encode("content_block_start", []byte(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"Edit","input":{}}}`)))
	}))
	store.AddFromModel("sess-1", 1, []presets.ModelPreset{{ExpectedInput: "a", Response: "b", MaxTurns: 3}})
	// A tool_result turn is not a user turn: no decrement. The Edit call invalidates.
	toolTurn := strings.Replace(mainReq, `[{"type":"text","text":"<system-reminder>x</system-reminder>"},{"type":"text","text":"which db?"}]`, `[{"type":"tool_result","tool_use_id":"t0","content":"ok"}]`, 1)
	store.AddFromModel("sess-1", 1, []presets.ModelPreset{{ExpectedInput: "c", Response: "d", MaxTurns: 3}})
	post(t, srv.URL+"/v1/messages", toolTurn)
	if store.Len("sess-1") != 0 {
		t.Fatalf("not invalidated: %+v", store.List("sess-1"))
	}

	store.AddFromModel("sess-1", 1, []presets.ModelPreset{{ExpectedInput: "a", Response: "b", MaxTurns: 1}})
	bg := strings.Replace(mainReq, `"model":"claude-opus-5-5"`, `"model":"claude-haiku-4-5"`, 1)
	post(t, srv.URL+"/v1/messages", bg) // background request: no decrement
	if store.Len("sess-1") != 1 {
		t.Fatal("background request decremented presets")
	}
}

func TestNonStreamingStrip(t *testing.T) {
	_, srv, store := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"Answer <b>.\n\n<presets>[{\"expected_input\":\"a\",\"response\":\"b\"}]</presets>"}],"stop_reason":"end_turn","usage":{"output_tokens":5}}`)
	})
	req := strings.Replace(mainReq, `"stream":true`, `"stream":false`, 1)
	_, body := post(t, srv.URL+"/v1/messages", req)
	var msg struct {
		Content []struct{ Text string } `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil || msg.Content[0].Text != "Answer <b>." {
		t.Fatalf("body %s", body)
	}
	if store.Len("sess-1") != 1 {
		t.Fatal("preset not stored")
	}
}

func TestPassthroughUntouched(t *testing.T) {
	const reply = `{"input_tokens":42}`
	stub, srv, _ := setup(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("request-id", "req_1")
		io.WriteString(w, reply)
	})
	resp, body := post(t, srv.URL+"/v1/messages/count_tokens?beta=true", mainReq)
	if body != reply || resp.Header.Get("request-id") != "req_1" {
		t.Fatalf("body %q hdr %v", body, resp.Header)
	}
	if !bytes.Equal(stub.lastBody, []byte(mainReq)) {
		t.Fatal("count_tokens body was modified")
	}
	// Upstream errors pass through with their status.
	stub.respond = func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error"}}`)
	}
	if resp, _ := post(t, srv.URL+"/v1/messages", mainReq); resp.StatusCode != 529 {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestInjectStringSystem(t *testing.T) {
	out, err := injectInstruction([]byte(`{"system":"Be brief & <nice>","messages":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		System []struct{ Type, Text string } `json:"system"`
	}
	json.Unmarshal(out, &m)
	if len(m.System) != 2 || m.System[0].Text != "Be brief & <nice>" || !bytes.Contains(out, []byte("<nice>")) {
		t.Fatalf("%s", out)
	}
	again, _ := injectInstruction([]byte(`{"system":"Be brief & <nice>","messages":[]}`))
	if !bytes.Equal(out, again) {
		t.Fatal("injection is not deterministic")
	}
}
