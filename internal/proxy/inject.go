package proxy

import (
	"bytes"
	"encoding/json"
)

// Instruction is appended to the system prompt of every main-model request. It must stay
// byte-identical across requests (and across proxy restarts within a session): changing
// the system prompt breaks prompt caching and, on models with preserved thinking,
// invalidates every earlier thinking block in the conversation.
const Instruction = `# Instant follow-up presets

When you end your turn with a text answer to the user (not a tool call), append a presets block as the very last thing in your answer, starting on its own line:

<presets>[{"expected_input": "...", "response": "...", "max_turns": 1}]</presets>

It holds a JSON array of up to 4 of the user's most likely next messages, each with the reply you would give. A matcher serves a reply instantly when the user's message fits it, so write one whenever you can guess a plausible next message: a "why" or "why not X" question about what you just said, a request for the obvious next step, a yes/no confirmation, or thanks. Each item has: "expected_input" (a short, specific description of the message, e.g. "asks why not SQLite"), "response" (your complete reply to that message, written exactly as you would answer it), and "max_turns" (1-3, how many user turns it stays relevant). Only include messages you can answer fully without tools. The block is hidden from the user, so never refer to it in your answer, but it stays visible to you in later turns. An item may therefore also be {"note": "..."}: a private note to your future self, such as a decision you have made but not told the user. Skip the block only when there is nothing to prepare or note.`

// injectInstruction appends Instruction as a final system text block. A string system
// prompt is converted to a single text block, which renders identically.
func injectInstruction(body []byte) ([]byte, error) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	block, err := marshal(map[string]string{"type": "text", "text": Instruction})
	if err != nil {
		return nil, err
	}
	var blocks []json.RawMessage
	switch sys := bytes.TrimSpace(req["system"]); {
	case len(sys) == 0 || string(sys) == "null":
	case sys[0] == '"':
		var s string
		if err := json.Unmarshal(sys, &s); err != nil {
			return nil, err
		}
		first, err := marshal(map[string]string{"type": "text", "text": s})
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, first)
	default:
		if err := json.Unmarshal(sys, &blocks); err != nil {
			return nil, err
		}
	}
	req["system"], err = marshal(append(blocks, block))
	if err != nil {
		return nil, err
	}
	return marshal(req)
}

// marshal encodes without HTML escaping, so prompt text keeps its literal < and >.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
