package proxy

import (
	"encoding/json"
	"io"
	"strings"

	"github.com/McKean/intuitive-response/internal/presets"
	"github.com/McKean/intuitive-response/internal/sse"
)

// streamState forwards one streaming response, stripping a trailing <presets> block.
type streamState struct {
	srv  *Server
	info reqInfo
	out  *clientWriter
	// detach stops a client disconnect from cancelling the upstream call. It must run
	// before the early close: the client may hang up as soon as it sees message_stop.
	detach func()

	strippers    map[int]*presets.Stripper // text blocks by index
	visible      map[int]*strings.Builder  // text forwarded per block (what the client stores)
	baseOutput   int                       // output_tokens from message_start
	emittedBytes int
}

type streamEvent struct {
	Index   int `json:"index"`
	Message struct {
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type string `json:"type"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
}

// run forwards events until the stream ends. When a presets block is detected it closes
// the client's message immediately and returns true; a goroutine then drains the rest of
// the upstream response and calls finish. Otherwise the caller calls finish.
func (st *streamState) run(body io.Reader, finish func()) (detached bool) {
	st.strippers = map[int]*presets.Stripper{}
	st.visible = map[int]*strings.Builder{}
	rd := sse.NewReader(body)
	for st.out.err == nil {
		ev, err := rd.Next()
		if err != nil {
			return false
		}
		var e streamEvent
		_ = json.Unmarshal(ev.Data, &e)
		switch ev.Type() {
		case "message_start":
			st.baseOutput = e.Message.Usage.OutputTokens
		case "content_block_start":
			switch e.ContentBlock.Type {
			case "text":
				st.strippers[e.Index] = presets.NewStripper()
				st.visible[e.Index] = &strings.Builder{}
			case "tool_use":
				st.srv.onToolUse(st.info, e.ContentBlock.Name)
			}
		case "content_block_delta":
			sp := st.strippers[e.Index]
			if sp == nil || e.Delta.Type != "text_delta" {
				break
			}
			emit, detected := sp.Feed(e.Delta.Text)
			if emit != "" {
				st.out.write(sse.TextDelta(e.Index, emit))
				st.emittedBytes += len(emit)
				st.visible[e.Index].WriteString(emit)
			}
			if detected {
				st.detach()
				// Before the client can see message_stop: its next request must wait
				// for this block to be recorded.
				st.srv.hiddenBegin(st.info)
				st.closeEarly(e.Index)
				go st.drain(rd, sp, e.Index, finish)
				return true
			}
			continue
		case "content_block_stop":
			if sp := st.strippers[e.Index]; sp != nil {
				if rest := sp.Flush(); rest != "" {
					st.out.write(sse.TextDelta(e.Index, rest))
					st.emittedBytes += len(rest)
					st.visible[e.Index].WriteString(rest)
				}
			}
		}
		st.out.write(ev.Raw)
	}
	return false
}

// closeEarly ends the client's message right after the visible answer so the user does
// not wait for the model to finish writing presets.
func (st *streamState) closeEarly(index int) {
	st.out.write(sse.BlockStop(index))
	// Approximate output tokens: ~4 bytes per token for what the client has seen.
	st.out.write(sse.MessageDelta("end_turn", st.baseOutput+st.emittedBytes/4))
	st.out.write(sse.MessageStop())
	st.srv.stats.Inc("early_closes")
}

func (st *streamState) drain(rd *sse.Reader, sp *presets.Stripper, index int, finish func()) {
	defer finish()
	defer st.srv.hiddenEnd(st.info)
	stopReason := ""
	toolUse := false
	complete := false
	for !complete {
		ev, err := rd.Next()
		if err != nil {
			break
		}
		var e streamEvent
		_ = json.Unmarshal(ev.Data, &e)
		switch ev.Type() {
		case "content_block_delta":
			if e.Index == index && e.Delta.Type == "text_delta" {
				sp.Feed(e.Delta.Text)
			}
		case "content_block_start":
			// Never reaches the client, so it cannot modify files: no invalidation.
			toolUse = toolUse || e.ContentBlock.Type == "tool_use"
		case "message_delta":
			stopReason = e.Delta.StopReason
		case "message_stop":
			complete = true
		}
	}
	st.srv.storeBlock(st.info, sp.Captured(), stopReason, toolUse, complete)
	st.srv.rememberHidden(st.info, st.visible[index].String(), sp.Suffix())
}

// storeBlock parses a captured presets block and inserts it, unless the turn ended in a
// tool call (presets are only valid after a final text answer).
func (s *Server) storeBlock(info reqInfo, captured, stopReason string, toolUse, complete bool) {
	fields := map[string]any{"session": info.SessionID, "stop_reason": stopReason, "bytes": len(captured)}
	if toolUse || stopReason == "tool_use" {
		s.stats.Inc("presets_discarded")
		fields["reason"] = "tool_use"
		s.log.Log("presets_discarded", fields)
		return
	}
	if !complete {
		fields["reason"] = "incomplete_stream"
	}
	items, err := presets.Parse(captured)
	if err != nil {
		s.stats.Inc("presets_invalid")
		fields["error"] = err.Error()
		s.log.Log("presets_invalid", fields)
		return
	}
	c := s.store.AddFromModel(info.SessionID, info.AssistantTurns+1, items)
	s.stats.Add("presets_created", c.Created)
	s.stats.Add("presets_evicted", c.Evicted)
	s.stats.Add("presets_replaced", c.Replaced)
	fields["created"] = c.Created
	fields["evicted"] = c.Evicted
	fields["replaced"] = c.Replaced
	fields["live"] = s.store.Len(info.SessionID)
	var expected []string
	for _, it := range items {
		expected = append(expected, it.ExpectedInput)
	}
	fields["expected_inputs"] = expected
	s.log.Log("presets_stored", fields)
}

func (s *Server) onToolUse(info reqInfo, name string) {
	if !modifyingTools[name] {
		return
	}
	if c := s.store.Invalidate(info.SessionID); c.Invalidated > 0 {
		s.stats.Add("presets_invalidated", c.Invalidated)
		s.log.Log("presets_invalidated", map[string]any{
			"session": info.SessionID, "tool": name, "count": c.Invalidated,
		})
	}
}

// handleJSON strips presets from a non-streaming response.
func (s *Server) handleJSON(out *clientWriter, body io.Reader, info reqInfo) {
	raw, err := io.ReadAll(body)
	if err != nil {
		out.write(raw)
		return
	}
	out.write(s.stripJSON(raw, info))
}

func (s *Server) stripJSON(raw []byte, info reqInfo) []byte {
	var msg map[string]json.RawMessage
	var blocks []map[string]json.RawMessage
	if json.Unmarshal(raw, &msg) != nil || json.Unmarshal(msg["content"], &blocks) != nil {
		return raw
	}
	var stopReason string
	_ = json.Unmarshal(msg["stop_reason"], &stopReason)
	toolUse := false
	last := -1
	for i, b := range blocks {
		var typ, name string
		_ = json.Unmarshal(b["type"], &typ)
		switch typ {
		case "text":
			last = i
		case "tool_use":
			toolUse = true
			_ = json.Unmarshal(b["name"], &name)
			s.onToolUse(info, name)
		}
	}
	if last < 0 {
		return raw
	}
	var text string
	_ = json.Unmarshal(blocks[last]["text"], &text)
	visible, captured, suffix, found := presets.StripText(text)
	if !found {
		return raw
	}
	tb, err1 := marshal(visible)
	blocks[last]["text"] = tb
	cb, err2 := marshal(blocks)
	msg["content"] = cb
	nb, err3 := marshal(msg)
	if err1 != nil || err2 != nil || err3 != nil {
		return raw
	}
	s.storeBlock(info, captured, stopReason, toolUse, true)
	s.rememberHidden(info, visible, suffix)
	return nb
}
