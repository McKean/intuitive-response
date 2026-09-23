package proxy

import (
	"encoding/json"
	"net/http"
	"regexp"
	"strings"
)

// reqInfo is what the proxy needs to know about a POST /v1/messages request.
type reqInfo struct {
	Model          string
	Stream         bool
	SessionID      string
	Main           bool // main-model request (gets the instruction, may carry presets)
	UserTurn       bool // last message is typed user text, not tool results
	AssistantTurns int  // assistant messages in history; the reply is turn AssistantTurns+1

	UserText      string // typed text of the last user message, reminders removed
	PrevAssistant string // visible text of the assistant message before it
}

type messagesRequest struct {
	Model    string            `json:"model"`
	Stream   bool              `json:"stream"`
	Tools    []json.RawMessage `json:"tools"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
	Metadata struct {
		UserID string `json:"user_id"`
	} `json:"metadata"`
}

func analyze(body []byte, h http.Header) (reqInfo, error) {
	var req messagesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return reqInfo{}, err
	}
	info := reqInfo{
		Model:     req.Model,
		Stream:    req.Stream,
		SessionID: sessionID(h, req.Metadata.UserID),
	}
	// Background requests (titles, summaries, quota probes) run on a small model and/or
	// carry no tools. The main agent loop always sends its tool set.
	info.Main = len(req.Tools) > 0 && !strings.Contains(strings.ToLower(req.Model), "haiku")
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			info.AssistantTurns++
		}
	}
	if n := len(req.Messages); n > 0 && req.Messages[n-1].Role == "user" {
		info.UserTurn = isTypedText(req.Messages[n-1].Content)
		if info.UserTurn {
			info.UserText = typedText(req.Messages[n-1].Content)
			if isBackgroundPrompt(info.UserText) {
				// Same model and tools as the main loop, but a background request that must
				// not touch presets.
				info.Main, info.UserTurn = false, false
				return info, nil
			}
			if n > 1 && req.Messages[n-2].Role == "assistant" {
				info.PrevAssistant = typedText(req.Messages[n-2].Content)
			}
		}
	}
	return info, nil
}

// backgroundPrompts are prompts Claude Code sends as if typed by the user.
var backgroundPrompts = []string{
	"[SUGGESTION MODE",                          // prompt suggestions
	"The user stepped away and is coming back.", // "welcome back" recap
}

func isBackgroundPrompt(text string) bool {
	for _, p := range backgroundPrompts {
		if strings.HasPrefix(text, p) {
			return true
		}
	}
	return false
}

var reminderRE = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>`)

// typedText joins the text blocks of a message, dropping system reminders. Thinking,
// tool_use, and image blocks are ignored.
func typedText(content json.RawMessage) string {
	var parts []string
	var s string
	if json.Unmarshal(content, &s) == nil {
		parts = append(parts, s)
	} else {
		var blocks []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		_ = json.Unmarshal(content, &blocks)
		for _, b := range blocks {
			if b.Type == "text" {
				parts = append(parts, b.Text)
			}
		}
	}
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(reminderRE.ReplaceAllString(p, "")); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, "\n\n")
}

// isTypedText reports whether user content contains text the user typed: no tool_result
// blocks, and some text left once <system-reminder> sections are removed (Claude Code can
// put a reminder and the user's message in the same text block).
func isTypedText(content json.RawMessage) bool {
	var blocks []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		for _, b := range blocks {
			if b.Type == "tool_result" {
				return false
			}
		}
	}
	return typedText(content) != ""
}

// sessionID prefers Claude Code's session header, then the session embedded in
// metadata.user_id (either "..._session_<uuid>" or a JSON object with session_id).
func sessionID(h http.Header, userID string) string {
	if v := h.Get("X-Claude-Code-Session-Id"); v != "" {
		return v
	}
	var obj struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(userID), &obj) == nil && obj.SessionID != "" {
		return obj.SessionID
	}
	if i := strings.LastIndex(userID, "session_"); i >= 0 && i+len("session_") < len(userID) {
		return userID[i+len("session_"):]
	}
	return "default"
}

// modifyingTools invalidate presets when the model calls them.
var modifyingTools = map[string]bool{
	"Write": true, "Edit": true, "MultiEdit": true, "NotebookEdit": true, "Bash": true,
}
