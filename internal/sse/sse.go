// Package sse reads and writes the server-sent events used by the Messages API stream.
package sse

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// Event is one SSE event. Raw holds the exact upstream bytes (including the blank-line
// terminator) so untouched events can be forwarded byte-for-byte.
type Event struct {
	Name string
	Data []byte
	Raw  []byte
}

// Type returns the "type" field of the event's JSON data, falling back to the event name.
func (e Event) Type() string {
	var t struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(e.Data, &t) == nil && t.Type != "" {
		return t.Type
	}
	return e.Name
}

type Reader struct {
	br *bufio.Reader
}

func NewReader(r io.Reader) *Reader {
	return &Reader{br: bufio.NewReaderSize(r, 64*1024)}
}

// Next returns the next event. It returns io.EOF once the stream ends cleanly.
func (r *Reader) Next() (Event, error) {
	var ev Event
	var raw bytes.Buffer
	var data [][]byte
	for {
		line, err := r.br.ReadBytes('\n')
		raw.Write(line)
		trimmed := bytes.TrimRight(line, "\r\n")
		if len(trimmed) == 0 && len(line) > 0 {
			if ev.Name == "" && data == nil {
				// Stray blank line between events.
				raw.Reset()
				if err != nil {
					return Event{}, err
				}
				continue
			}
			ev.Data = bytes.Join(data, []byte("\n"))
			ev.Raw = raw.Bytes()
			return ev, nil
		}
		switch {
		case bytes.HasPrefix(trimmed, []byte("event:")):
			ev.Name = string(bytes.TrimSpace(trimmed[len("event:"):]))
		case bytes.HasPrefix(trimmed, []byte("data:")):
			d := trimmed[len("data:"):]
			if len(d) > 0 && d[0] == ' ' {
				d = d[1:]
			}
			data = append(data, append([]byte(nil), d...))
		}
		if err != nil {
			if err == io.EOF && (ev.Name != "" || data != nil) {
				// Stream ended without a terminating blank line; deliver what we have.
				ev.Data = bytes.Join(data, []byte("\n"))
				raw.WriteString("\n\n")
				ev.Raw = raw.Bytes()
				return ev, nil
			}
			return Event{}, err
		}
	}
}

// Encode renders an event in wire format.
func Encode(name string, data []byte) []byte {
	return fmt.Appendf(nil, "event: %s\ndata: %s\n\n", name, data)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func TextDelta(index int, text string) []byte {
	return Encode("content_block_delta", mustJSON(map[string]any{
		"type":  "content_block_delta",
		"index": index,
		"delta": map[string]any{"type": "text_delta", "text": text},
	}))
}

func BlockStop(index int) []byte {
	return Encode("content_block_stop", mustJSON(map[string]any{
		"type":  "content_block_stop",
		"index": index,
	}))
}

func MessageDelta(stopReason string, outputTokens int) []byte {
	return Encode("message_delta", mustJSON(map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReason, "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": outputTokens},
	}))
}

func MessageStop() []byte {
	return Encode("message_stop", []byte(`{"type":"message_stop"}`))
}
