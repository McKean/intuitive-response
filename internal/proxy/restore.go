package proxy

import (
	"encoding/json"
	"time"
)

// hiddenWait bounds how long a request waits for the previous reply's stripped block to
// finish streaming. Only replies ended early are pending, and the user is rarely faster.
const hiddenWait = 30 * time.Second

// restoreHidden appends each assistant message's stripped suffix back onto its last text
// block, so the model sees what it wrote (presets, private notes) while the user does not.
// It returns the body unchanged when nothing was restored.
func (s *Server) restoreHidden(body []byte, sessionID string) ([]byte, int, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, 0, err
	}
	var msgs []map[string]json.RawMessage
	if err := json.Unmarshal(req["messages"], &msgs); err != nil {
		return body, 0, err
	}
	restored := 0
	turn := 0
	for _, m := range msgs {
		var role string
		_ = json.Unmarshal(m["role"], &role)
		if role != "assistant" {
			continue
		}
		ordinal := turn
		turn++
		content, changed, err := s.restoreMessage(m["content"], sessionID, ordinal)
		if err != nil {
			return body, 0, err
		}
		if changed {
			m["content"] = content
			restored++
		}
	}
	if restored == 0 {
		return body, 0, nil
	}
	var err error
	if req["messages"], err = marshal(msgs); err != nil {
		return body, 0, err
	}
	out, err := marshal(req)
	if err != nil {
		return body, 0, err
	}
	return out, restored, nil
}

func (s *Server) restoreMessage(content json.RawMessage, sessionID string, ordinal int) (json.RawMessage, bool, error) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		suffix, ok := s.hidden.Lookup(sessionID, ordinal, text)
		if !ok {
			return content, false, nil
		}
		b, err := marshal(text + suffix)
		return b, err == nil, err
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(content, &blocks); err != nil {
		return content, false, nil
	}
	last := -1
	for i, b := range blocks {
		var typ string
		_ = json.Unmarshal(b["type"], &typ)
		if typ == "text" {
			last = i
		}
	}
	if last < 0 {
		return content, false, nil
	}
	_ = json.Unmarshal(blocks[last]["text"], &text)
	suffix, ok := s.hidden.Lookup(sessionID, ordinal, text)
	if !ok {
		return content, false, nil
	}
	tb, err := marshal(text + suffix)
	if err != nil {
		return content, false, err
	}
	blocks[last]["text"] = tb
	b, err := marshal(blocks)
	return b, err == nil, err
}

// rememberHidden records a stripped suffix for the assistant message the current request
// produces (ordinal = number of assistant messages already in history).
func (s *Server) rememberHidden(info reqInfo, visible, suffix string) {
	if s.hidden == nil {
		return
	}
	if err := s.hidden.Record(info.SessionID, info.AssistantTurns, visible, suffix); err != nil {
		s.log.Log("hidden_error", map[string]any{"session": info.SessionID, "error": err.Error()})
	}
}

// hiddenBegin/hiddenEnd bracket a stripped block that is still streaming.
func (s *Server) hiddenBegin(info reqInfo) {
	if s.hidden != nil {
		s.hidden.Begin(info.SessionID)
	}
}

func (s *Server) hiddenEnd(info reqInfo) {
	if s.hidden != nil {
		s.hidden.End(info.SessionID)
	}
}
