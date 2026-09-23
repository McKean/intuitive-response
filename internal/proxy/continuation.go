package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/McKean/intuitive-response/internal/logx"
	"github.com/McKean/intuitive-response/internal/presets"
	"github.com/McKean/intuitive-response/internal/sse"
)

// continuationNote is the synthetic user message for the "note" strategy. The model sees
// its prepared reply as a finished assistant turn and adds to it; the user only sees what
// it adds, appended to the same message.
const continuationNote = "[proxy] The previous assistant message was a reply you prepared in advance; " +
	"it was shown to the user instantly. Continue that reply: add only what is missing, or correct " +
	"anything wrong, written so it reads naturally appended to it. Do not repeat it, do not call " +
	"tools, and do not mention this note or the proxy. If nothing needs adding, reply exactly <none> " +
	"(you may still append a presets block after it)."

const noneReply = "<none>"

// buildContinuation appends the served preset to the conversation. With "prefill" it is a
// partial assistant message the model continues; with "note" it is a complete assistant
// turn followed by continuationNote.
func buildContinuation(body []byte, preset, strategy string) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(req["messages"], &msgs); err != nil {
		return nil, err
	}
	text := preset
	if strategy == "prefill" {
		text = strings.TrimRight(preset, spaceChars) // prefill may not end in whitespace
	}
	asst, err := marshal(map[string]any{
		"role":    "assistant",
		"content": []map[string]string{{"type": "text", "text": text}},
	})
	if err != nil {
		return nil, err
	}
	msgs = append(msgs, asst)
	if strategy != "prefill" {
		note, err := marshal(map[string]any{
			"role":    "user",
			"content": []map[string]string{{"type": "text", "text": continuationNote}},
		})
		if err != nil {
			return nil, err
		}
		msgs = append(msgs, note)
	}
	if req["messages"], err = marshal(msgs); err != nil {
		return nil, err
	}
	req["stream"] = json.RawMessage("true")
	return marshal(req)
}

const spaceChars = " \t\r\n"

// serveHit streams the preset to the client at once, then streams a continuation from the
// model into the same, still-open message.
func (s *Server) serveHit(w http.ResponseWriter, r *http.Request, body []byte, info reqInfo, p presets.Preset, start time.Time) {
	s.stats.Inc("hits_served")
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)

	var cap *captureBuf
	if s.traffic != nil {
		cap = &captureBuf{c: logx.Capture{
			Time: start, Session: info.SessionID, Method: r.Method, Path: r.URL.RequestURI() + " (hit)",
			RequestHeaders: logx.RedactHeaders(r.Header), Status: http.StatusOK,
		}}
	}
	out := newClientWriter(w, cap)

	var id [12]byte
	_, _ = rand.Read(id[:])
	msgStart, _ := marshal(map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": "msg_irp_" + hex.EncodeToString(id[:]), "type": "message", "role": "assistant",
			"model": info.Model, "content": []any{}, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]int{"input_tokens": 0, "output_tokens": 0},
		},
	})
	out.write(sse.Encode("message_start", msgStart))
	out.write(sse.Encode("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)))
	out.write(sse.TextDelta(0, p.Response))
	out.write(sse.BlockStop(0))
	instant := time.Since(start)

	fields := map[string]any{
		"session": info.SessionID, "preset_id": p.ID, "strategy": s.cfg.Continuation,
		"instant_ms": instant.Milliseconds(),
	}

	// The continuation outlives the handler when it ends in a presets block (the drain
	// keeps reading), so it gets its own context: bounded by the timeout, and cancelled
	// by a client disconnect until detached.
	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ContinuationTimeout)
	stopWatch := context.AfterFunc(r.Context(), cancel)
	detached := false
	defer func() {
		if !detached {
			stopWatch()
			cancel()
		}
	}()

	resp, contBody, strategy, err := s.startContinuation(ctx, r, body, p.Response)
	fields["strategy"] = strategy
	if cap != nil {
		cap.c.RequestBody = contBody
	}
	if err != nil {
		s.closeHit(out, len(p.Response), nil)
		fields["outcome"] = outcomeFor(ctx, err)
		fields["error"] = err.Error()
		s.finishHit(fields, cap)
		return
	}
	if cap != nil {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, &cap.up), resp.Body}
	}

	cs := &contState{srv: s, info: info, out: out, presetLen: len(p.Response), start: start, lastVisible: p.Response}
	finish := func() {
		resp.Body.Close()
		stopWatch()
		cancel()
	}
	detached = cs.run(resp.Body, func() { stopWatch() }, func() {
		finish()
		fields["outcome"] = cs.outcome(ctx)
		fields["appended_bytes"] = cs.appended
		fields["continuation_first_text_ms"] = cs.firstTextMS
		s.finishHit(fields, cap)
	})
	if !detached {
		s.closeHit(out, len(p.Response), &cs.usage)
		finish()
		fields["outcome"] = cs.outcome(ctx)
		fields["appended_bytes"] = cs.appended
		fields["continuation_first_text_ms"] = cs.firstTextMS
		s.finishHit(fields, cap)
	}
}

func (s *Server) finishHit(fields map[string]any, cap *captureBuf) {
	s.stats.Inc("continuation_" + fields["outcome"].(string))
	s.log.Log("hit", fields)
	cap.write(s.traffic)
}

// startContinuation sends the continuation request. A prefill rejected with 400 (every
// current model) is retried once with the note strategy.
func (s *Server) startContinuation(ctx context.Context, r *http.Request, body []byte, preset string) (*http.Response, []byte, string, error) {
	strategy := s.cfg.Continuation
	for {
		contBody, err := buildContinuation(body, preset, strategy)
		if err != nil {
			return nil, nil, strategy, err
		}
		u := *s.upstream
		u.Path = strings.TrimRight(u.Path, "/") + r.URL.Path
		u.RawQuery = r.URL.RawQuery
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(contBody))
		if err != nil {
			return nil, contBody, strategy, err
		}
		copyHeaders(req.Header, r.Header)
		resp, err := s.client.Do(req)
		if err != nil {
			return nil, contBody, strategy, err
		}
		if resp.StatusCode == http.StatusOK {
			return resp, contBody, strategy, nil
		}
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if resp.StatusCode == http.StatusBadRequest && strategy == "prefill" {
			s.log.Log("prefill_rejected", map[string]any{"body": string(msg)})
			strategy = "note"
			continue
		}
		return nil, contBody, strategy, &upstreamStatusError{resp.StatusCode, string(msg)}
	}
}

type upstreamStatusError struct {
	status int
	body   string
}

func (e *upstreamStatusError) Error() string {
	return "continuation: HTTP " + http.StatusText(e.status) + ": " + e.body
}

func outcomeFor(ctx context.Context, err error) string {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return "timeout"
	}
	return "error"
}

// closeHit ends the client's message. Output tokens are the continuation's plus an
// estimate for the preset (~4 bytes per token).
func (s *Server) closeHit(out *clientWriter, presetLen int, u *contUsage) {
	usage := map[string]int{"output_tokens": presetLen / 4}
	if u != nil {
		usage["output_tokens"] += u.OutputTokens
		usage["input_tokens"] = u.InputTokens
		usage["cache_read_input_tokens"] = u.CacheRead
		usage["cache_creation_input_tokens"] = u.CacheCreation
	}
	delta, _ := marshal(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": usage,
	})
	out.write(sse.Encode("message_delta", delta))
	out.write(sse.MessageStop())
}

type contUsage struct {
	InputTokens   int `json:"input_tokens"`
	OutputTokens  int `json:"output_tokens"`
	CacheRead     int `json:"cache_read_input_tokens"`
	CacheCreation int `json:"cache_creation_input_tokens"`
}

// contBlock tracks one upstream content block of the continuation.
type contBlock struct {
	text      bool // only text blocks are forwarded
	clientIdx int
	started   bool // content_block_start sent to the client
	held      string
	visible   string // text forwarded to the client
	stripper  *presets.Stripper
}

// contState forwards a continuation stream into the client's open message:
//   - only text blocks are forwarded, re-indexed after the preset's block 0. Thinking blocks
//     are dropped (their signatures cover the synthetic note, which the client never sees),
//     and so are tool calls (a tool call without its thinking block is rejected later);
//   - a text block that is exactly <none> is suppressed;
//   - a trailing presets block is stripped and stored, as on a normal turn.
type contState struct {
	srv       *Server
	info      reqInfo
	out       *clientWriter
	presetLen int
	start     time.Time

	blocks      map[int]*contBlock
	nextIdx     int
	appended    int
	firstTextMS int64
	toolUse     bool
	complete    bool
	failed      bool
	usage       contUsage
	lastVisible string // the message's last text block as the client stores it
}

func (cs *contState) outcome(ctx context.Context) string {
	switch {
	case !cs.complete && errors.Is(ctx.Err(), context.DeadlineExceeded):
		return "timeout"
	case !cs.complete && cs.failed:
		return "error"
	case cs.toolUse:
		return "tool_use_dropped"
	case cs.appended > 0:
		return "appended"
	default:
		return "none"
	}
}

// run forwards the continuation. It returns true if it detected a presets block and
// handed the rest of the stream to a drain goroutine, which then calls done.
func (cs *contState) run(body io.Reader, detach func(), done func()) bool {
	cs.blocks = map[int]*contBlock{}
	cs.nextIdx = 1
	rd := sse.NewReader(body)
	for cs.out.err == nil {
		ev, err := rd.Next()
		if err != nil {
			cs.failed = true // the stream ended without message_stop
			cs.closeOpenBlocks()
			return false
		}
		var e struct {
			streamEvent
			Message struct {
				Usage contUsage `json:"usage"`
			} `json:"message"`
			Usage contUsage `json:"usage"`
		}
		_ = json.Unmarshal(ev.Data, &e)
		switch ev.Type() {
		case "message_start":
			cs.usage = e.Message.Usage
		case "content_block_start":
			b := &contBlock{text: e.ContentBlock.Type == "text"}
			if b.text {
				b.stripper = presets.NewStripper()
			}
			if e.ContentBlock.Type == "tool_use" || e.ContentBlock.Type == "server_tool_use" {
				cs.toolUse = true
			}
			cs.blocks[e.Index] = b
		case "content_block_delta":
			b := cs.blocks[e.Index]
			if b == nil || !b.text || e.Delta.Type != "text_delta" {
				break
			}
			emit, detected := b.stripper.Feed(e.Delta.Text)
			cs.emit(b, emit, false)
			if detected {
				cs.emit(b, "", true)
				cs.closeOpenBlocks()
				detach()
				cs.srv.hiddenBegin(cs.info)
				cs.srv.closeHit(cs.out, cs.presetLen, &cs.usage)
				go cs.drain(rd, b.stripper, e.Index, done)
				return true
			}
		case "content_block_stop":
			if b := cs.blocks[e.Index]; b != nil && b.text {
				cs.emit(b, b.stripper.Flush(), true)
				if b.started {
					cs.out.write(sse.BlockStop(b.clientIdx))
					b.started = false
				}
				delete(cs.blocks, e.Index)
			}
		case "message_delta":
			cs.usage.OutputTokens = e.Usage.OutputTokens
		case "message_stop":
			cs.complete = true
			cs.closeOpenBlocks()
			return false
		}
	}
	return false
}

// emit forwards text for a block, holding it back while it could still be "<none>".
// final is set at the end of the block's visible text.
func (cs *contState) emit(b *contBlock, text string, final bool) {
	if !b.started {
		b.held += text
		trimmed := strings.TrimSpace(b.held)
		if !final && strings.HasPrefix(noneReply, trimmed) {
			return // still possibly <none>
		}
		if trimmed == noneReply || trimmed == "" {
			if final {
				b.held = ""
			}
			return
		}
		b.clientIdx = cs.nextIdx
		cs.nextIdx++
		start, _ := marshal(map[string]any{
			"type": "content_block_start", "index": b.clientIdx,
			"content_block": map[string]string{"type": "text", "text": ""},
		})
		cs.out.write(sse.Encode("content_block_start", start))
		b.started = true
		text, b.held = strings.TrimLeft(b.held, spaceChars), ""
		if cs.firstTextMS == 0 {
			cs.firstTextMS = time.Since(cs.start).Milliseconds()
		}
	}
	if text != "" {
		cs.out.write(sse.TextDelta(b.clientIdx, text))
		cs.appended += len(text)
		b.visible += text
		cs.lastVisible = b.visible
	}
}

func (cs *contState) closeOpenBlocks() {
	for idx, b := range cs.blocks {
		if b.started {
			cs.out.write(sse.BlockStop(b.clientIdx))
		}
		delete(cs.blocks, idx)
	}
}

func (cs *contState) drain(rd *sse.Reader, sp *presets.Stripper, index int, done func()) {
	defer done()
	defer cs.srv.hiddenEnd(cs.info)
	stopReason := ""
	for {
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
			cs.toolUse = cs.toolUse || e.ContentBlock.Type == "tool_use"
		case "message_delta":
			stopReason = e.Delta.StopReason
		case "message_stop":
			cs.complete = true
		}
		if cs.complete {
			break
		}
	}
	cs.srv.storeBlock(cs.info, sp.Captured(), stopReason, cs.toolUse, cs.complete)
	// A suppressed <none> never reaches the client, so the block is restored onto the
	// message's last visible text block (the preset itself if nothing was appended).
	cs.srv.rememberHidden(cs.info, cs.lastVisible, sp.Suffix())
}
