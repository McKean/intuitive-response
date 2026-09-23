package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/McKean/intuitive-response/internal/config"
	"github.com/McKean/intuitive-response/internal/presets"
	"github.com/McKean/intuitive-response/internal/sse"
)

const hitReply = `{"model":"jev-1.13.0","answers":{"match":{"type":"choice","choice":"preset_1","confidence":1,"probabilities":{"preset_1":1,"none":0}},"full_1":{"type":"noul","noul":0.95}},"usage":{"input_tokens":300,"output_tokens":20}}`

// routedUpstream answers continuation requests (recognised by the note) with cont and all
// other requests with orig, recording both bodies.
type routedUpstream struct {
	mu        sync.Mutex
	origCalls int
	origBody  []byte
	contBody  []byte
	orig      http.HandlerFunc
	cont      http.HandlerFunc
}

func (u *routedUpstream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	u.mu.Lock()
	isCont := strings.Contains(string(b), "[proxy] The previous assistant message")
	if isCont {
		u.contBody = b
	} else {
		u.origCalls++
		u.origBody = b
	}
	u.mu.Unlock()
	if isCont {
		u.cont(w, r)
	} else {
		u.orig(w, r)
	}
}

// allText returns text per client block index.
func allText(t *testing.T, body string) (map[int]string, []string) {
	t.Helper()
	rd := sse.NewReader(strings.NewReader(body))
	text := map[int]string{}
	var types []string
	for {
		ev, err := rd.Next()
		if err != nil {
			break
		}
		types = append(types, ev.Type())
		var e streamEvent
		json.Unmarshal(ev.Data, &e)
		if ev.Type() == "content_block_delta" {
			text[e.Index] += e.Delta.Text
		}
	}
	return text, types
}

func liveSetup(t *testing.T, up *routedUpstream, configure ...func(*config.Config)) (string, *presets.Store) {
	url, store, _ := liveSetupLog(t, up, configure...)
	return url, store
}

func liveSetupLog(t *testing.T, up *routedUpstream, configure ...func(*config.Config)) (string, *presets.Store, *strings.Builder) {
	t.Helper()
	srv, store, _, logBuf := setupJevWith(t, &jevStub{reply: hitReply}, up, func(c *config.Config) {
		c.JevTimeout = time.Second
		for _, f := range configure {
			f(c)
		}
	})
	store.AddFromModel("sess-1", 1, []presets.ModelPreset{{ExpectedInput: "asks which db", Response: "Postgres.", MaxTurns: 2}})
	return srv.URL, store, logBuf
}

func TestHitServesPresetThenContinuation(t *testing.T) {
	up := &routedUpstream{
		orig: sseStream([]string{"SLOW ORIGINAL"}, "end_turn", nil),
		// Block 0 is a thinking block, block 1 the text: thinking must be dropped and the
		// text re-indexed to 1, after the preset's block 0.
		cont: sseStream([]string{"Also enable ", "connection pooling."}, "end_turn", nil),
	}
	url, store := liveSetup(t, up)
	_, body := post(t, url+"/v1/messages", mainReq)

	text, types := allText(t, body)
	if text[0] != "Postgres." || text[1] != "Also enable connection pooling." || len(text) != 2 {
		t.Fatalf("text %v", text)
	}
	if strings.Contains(body, "thinking") || strings.Contains(body, "SLOW ORIGINAL") {
		t.Fatalf("leaked thinking or original response:\n%s", body)
	}
	if types[0] != "message_start" || types[len(types)-1] != "message_stop" || strings.Count(body, "event: message_stop") != 1 {
		t.Fatalf("types %v", types)
	}
	if store.Len("sess-1") != 0 {
		t.Fatal("fired preset should be removed")
	}
	up.mu.Lock()
	defer up.mu.Unlock()
	var sent struct {
		Messages []struct {
			Role    string                  `json:"role"`
			Content []struct{ Text string } `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(up.contBody, &sent)
	n := len(sent.Messages)
	if n != 3 || sent.Messages[1].Role != "assistant" || sent.Messages[1].Content[0].Text != "Postgres." ||
		sent.Messages[2].Role != "user" || sent.Messages[2].Content[0].Text != continuationNote {
		t.Fatalf("continuation messages %+v", sent.Messages)
	}
}

func TestHitContinuationNoneWithPresets(t *testing.T) {
	up := &routedUpstream{
		orig: sseStream([]string{"x"}, "end_turn", nil),
		cont: sseStream([]string{"<no", "ne>\n<presets>[{\"expected_input\":\"asks why\",\"response\":\"Because.\"}]</presets>"}, "end_turn", nil),
	}
	url, store := liveSetup(t, up)
	_, body := post(t, url+"/v1/messages", mainReq)
	text, types := allText(t, body)
	if len(text) != 1 || text[0] != "Postgres." || strings.Contains(body, "none") {
		t.Fatalf("text %v\n%s", text, body)
	}
	if types[len(types)-1] != "message_stop" {
		t.Fatalf("types %v", types)
	}
	waitFor(t, func() bool { return store.Len("sess-1") == 1 })
	if got := store.List("sess-1")[0].ExpectedInput; got != "asks why" {
		t.Fatalf("new preset %q", got)
	}
}

func TestHitContinuationDropsToolUse(t *testing.T) {
	up := &routedUpstream{
		orig: sseStream([]string{"x"}, "end_turn", nil),
		cont: sseStream([]string{"Let me check."}, "tool_use", func(w io.Writer) {
			w.Write(sse.Encode("content_block_start", []byte(`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"t1","name":"Bash","input":{}}}`)))
			w.Write(sse.Encode("content_block_delta", []byte(`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{}"}}`)))
			w.Write(sse.Encode("content_block_stop", []byte(`{"type":"content_block_stop","index":2}`)))
		}),
	}
	url, _ := liveSetup(t, up)
	_, body := post(t, url+"/v1/messages", mainReq)
	if strings.Contains(body, "tool_use") || !strings.Contains(body, `"stop_reason":"end_turn"`) {
		t.Fatalf("tool call forwarded:\n%s", body)
	}
}

func TestHitContinuationTimeout(t *testing.T) {
	hang := make(chan struct{})
	up := &routedUpstream{
		orig: sseStream([]string{"x"}, "end_turn", nil),
		cont: sseStream([]string{"partial ", waitDelta, "never"}, "end_turn", nil, hang),
	}
	url, _ := liveSetup(t, up, func(c *config.Config) { c.ContinuationTimeout = 200 * time.Millisecond })
	t.Cleanup(func() { close(hang) }) // registered after setup so it runs before the servers close
	start := time.Now()
	_, body := post(t, url+"/v1/messages", mainReq)
	text, types := allText(t, body)
	if time.Since(start) > 2*time.Second || text[0] != "Postgres." || text[1] != "partial" {
		t.Fatalf("text %v after %v", text, time.Since(start))
	}
	// Every started block is stopped before the message closes.
	if fmt.Sprint(types[len(types)-3:]) != "[content_block_stop message_delta message_stop]" {
		t.Fatalf("types %v", types)
	}
}

func TestPrefillFallsBackToNote(t *testing.T) {
	var prefillSeen bool
	up := &routedUpstream{
		orig: func(w http.ResponseWriter, r *http.Request) {
			// Only the prefill continuation reaches here (no note): reject it like current models.
			prefillSeen = true
			w.WriteHeader(400)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"prefill not supported"}}`)
		},
		cont: sseStream([]string{"Added."}, "end_turn", nil),
	}
	url, _ := liveSetup(t, up, func(c *config.Config) { c.Continuation = "prefill" })
	_, body := post(t, url+"/v1/messages", mainReq)
	text, _ := allText(t, body)
	if !prefillSeen || text[1] != "Added." {
		t.Fatalf("prefill seen %v, text %v", prefillSeen, text)
	}
}

func TestLiveMissUsesOriginal(t *testing.T) {
	up := &routedUpstream{
		orig: sseStream([]string{"Fresh answer."}, "end_turn", nil),
		cont: sseStream([]string{"unused"}, "end_turn", nil),
	}
	srv, store, _, _ := setupJevWith(t, &jevStub{reply: strings.Replace(hitReply, `"choice":"preset_1"`, `"choice":"none"`, 1)}, up, func(c *config.Config) {})
	store.AddFromModel("sess-1", 1, []presets.ModelPreset{{ExpectedInput: "asks which db", Response: "Postgres.", MaxTurns: 2}})
	_, body := post(t, srv.URL+"/v1/messages", mainReq)
	if text, _ := clientText(t, body); text != "Fresh answer." {
		t.Fatalf("text %v", text)
	}
	if store.Len("sess-1") != 1 {
		t.Fatal("miss should keep the preset (turns left 1)")
	}
}

func TestSuggestionModeIsBackground(t *testing.T) {
	for _, prompt := range []string{
		"[SUGGESTION MODE: Suggest what the user might naturally type next]",
		"The user stepped away and is coming back. Recap in under 40 words.",
	} {
		body := strings.Replace(mainReq, `"which db?"`, fmt.Sprintf("%q", prompt), 1)
		info, err := analyze([]byte(body), http.Header{})
		if err != nil || info.Main || info.UserTurn {
			t.Fatalf("%q: info %+v err %v", prompt, info, err)
		}
	}
}

func TestHitNoneBlockRestoredOntoPreset(t *testing.T) {
	up := &routedUpstream{
		orig: sseStream([]string{"ok"}, "end_turn", nil),
		cont: sseStream([]string{"<none>\n<presets>[{\"note\":\"n1\"}]</presets>"}, "end_turn", nil),
	}
	url, _ := liveSetup(t, up)
	post(t, url+"/v1/messages", mainReq) // hit: preset "Postgres." served, continuation <none>

	// Claude Code's history holds only the preset text; the model gets the block back on it.
	post(t, url+"/v1/messages", strings.Replace(secondTurn, `"Use Postgres."`, `"Postgres."`, 1))
	up.mu.Lock()
	defer up.mu.Unlock()
	if got := assistantText(t, up.origBody); got != "Postgres.\n<presets>[{\"note\":\"n1\"}]</presets>" {
		t.Fatalf("restored %q", got)
	}
}

func TestReminderInSameBlockIsTypedText(t *testing.T) {
	c := json.RawMessage(`[{"type":"text","text":"<system-reminder>ctx</system-reminder>\nthink of a number"}]`)
	if !isTypedText(c) || typedText(c) != "think of a number" {
		t.Fatalf("typed %v text %q", isTypedText(c), typedText(c))
	}
	if isTypedText(json.RawMessage(`[{"type":"text","text":"<system-reminder>only</system-reminder>"}]`)) {
		t.Fatal("reminder-only message counted as typed")
	}
}
