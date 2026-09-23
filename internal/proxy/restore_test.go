package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// secondTurn is mainReq's conversation after the assistant answered "Use Postgres.".
const secondTurn = `{"model":"claude-opus-5-5","stream":true,"system":"x","tools":[{"name":"Bash","input_schema":{"type":"object"}}],"messages":[` +
	`{"role":"user","content":"which db?"},` +
	`{"role":"assistant","content":[{"type":"thinking","thinking":"","signature":"sig"},{"type":"text","text":"Use Postgres."}]},` +
	`{"role":"user","content":"why?"}]}`

const presetBlock = "\n\n<presets>[{\"note\":\"leaning Postgres for MVCC\"},{\"expected_input\":\"asks why\",\"response\":\"MVCC.\"}]</presets>"

// assistantText returns the text blocks of the first assistant message sent upstream.
func assistantText(t *testing.T, body []byte) string {
	t.Helper()
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	json.Unmarshal(body, &req)
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			return typedText(m.Content)
		}
	}
	return ""
}

func TestHiddenBlockRestoredOnNextTurn(t *testing.T) {
	release := make(chan struct{})
	stub, srv, _ := setup(t, sseStream([]string{"Use Postgres.", presetBlock[:12], waitDelta, presetBlock[12:]}, "end_turn", nil, release))

	_, body := post(t, srv.URL+"/v1/messages", mainReq)
	if text, _ := clientText(t, body); text != "Use Postgres." {
		t.Fatalf("client text %q", text)
	}
	// The next request arrives while the block is still streaming: it must wait for it.
	done := make(chan struct{})
	go func() {
		post(t, srv.URL+"/v1/messages", secondTurn)
		close(done)
	}()
	time.Sleep(50 * time.Millisecond)
	close(release)
	<-done

	stub.mu.Lock()
	defer stub.mu.Unlock()
	if got := assistantText(t, stub.lastBody); got != "Use Postgres."+presetBlock {
		t.Fatalf("upstream saw assistant text %q", got)
	}
	// The thinking block before the text is untouched.
	if !strings.Contains(string(stub.lastBody), `"signature":"sig"`) {
		t.Fatal("thinking block changed")
	}
}

func TestHiddenNotRestoredForUntrackedSession(t *testing.T) {
	stub, srv, _ := setup(t, sseStream([]string{"Use Postgres.", presetBlock}, "end_turn", nil))
	// First request the proxy sees already has history: the session is not tracked.
	post(t, srv.URL+"/v1/messages", secondTurn)
	third := strings.Replace(secondTurn, `{"role":"user","content":"why?"}]`,
		`{"role":"user","content":"why?"},{"role":"assistant","content":"Use Postgres."},{"role":"user","content":"ok"}]`, 1)
	post(t, srv.URL+"/v1/messages", third)
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if strings.Contains(string(stub.lastBody), "leaning Postgres") {
		t.Fatal("restored into an untracked session")
	}
}

func TestRestoreSkipsOtherSessions(t *testing.T) {
	stub, srv, _ := setup(t, sseStream([]string{"Use Postgres.", presetBlock}, "end_turn", nil))
	post(t, srv.URL+"/v1/messages", mainReq)
	time.Sleep(50 * time.Millisecond)
	req, _ := http.NewRequest("POST", srv.URL+"/v1/messages", strings.NewReader(secondTurn))
	req.Header.Set("X-Claude-Code-Session-Id", "other")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	stub.mu.Lock()
	defer stub.mu.Unlock()
	if strings.Contains(string(stub.lastBody), "leaning Postgres") {
		t.Fatal("restored across sessions")
	}
}
