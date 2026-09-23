package presets

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const (
	closeTag       = "</presets>"
	maxPerBlock    = 4
	maxTurnsLimit  = 3
	defaultMaxTurn = 1
)

// ModelPreset is one item of the JSON array the model writes inside <presets>.
type ModelPreset struct {
	ExpectedInput string   `json:"expected_input"`
	Response      string   `json:"response"`
	MaxTurns      int      `json:"max_turns"`
	MinConfidence *float64 `json:"min_confidence,omitempty"`
	Note          string   `json:"note,omitempty"` // private note: kept in history, never matched
}

// Parse decodes the captured text that followed "<presets>". Notes and invalid items are
// dropped; an error is returned only when the block holds neither presets nor notes.
func Parse(captured string) ([]ModelPreset, error) {
	body, _, _ := strings.Cut(captured, closeTag)
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("empty presets block")
	}
	var items []ModelPreset
	if err := json.Unmarshal([]byte(body), &items); err != nil {
		return nil, fmt.Errorf("presets json: %w", err)
	}
	var out []ModelPreset
	notes := 0
	for _, it := range items {
		if it.Note != "" && it.ExpectedInput == "" {
			notes++
			continue
		}
		it.ExpectedInput = strings.TrimSpace(it.ExpectedInput)
		if it.ExpectedInput == "" || strings.TrimSpace(it.Response) == "" {
			continue
		}
		if it.MaxTurns < 1 {
			it.MaxTurns = defaultMaxTurn
		}
		it.MaxTurns = min(it.MaxTurns, maxTurnsLimit)
		if it.MinConfidence != nil && (*it.MinConfidence < 0 || *it.MinConfidence > 1) {
			it.MinConfidence = nil
		}
		out = append(out, it)
		if len(out) == maxPerBlock {
			break
		}
	}
	if len(out) == 0 && notes == 0 {
		return nil, errors.New("no valid presets in block")
	}
	return out, nil
}
