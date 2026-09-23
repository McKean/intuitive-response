package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/McKean/intuitive-response/internal/jev"
	"github.com/McKean/intuitive-response/internal/presets"
)

const (
	noneOption       = "none"
	maxStateChars    = 4000 // Jev accuracy drops with irrelevant state; keep only the tail
	maxLoggedMessage = 500
)

// matchResult is the outcome of asking Jev whether the user's message matches a preset.
type matchResult struct {
	Hit        bool
	Reason     string // "hit" | "none" | "low_confidence" | "not_full_answer" | "timeout" | "error"
	Preset     *presets.Preset
	Choice     string
	Confidence float64
	MinConf    float64 // confidence required for the chosen preset
	FullAnswer float64 // Noul probability for the chosen preset
	Latency    time.Duration
	Response   *jev.Response
	Err        error
}

func optionKey(i int) string { return "preset_" + strconv.Itoa(i+1) }

func fullKey(i int) string { return "full_" + strconv.Itoa(i+1) }

// buildMatchRequest makes one fan-out request: a Choice over the presets' expected inputs
// plus "none", and a speculative Noul per preset asking whether its response fully answers
// the message. Only the chosen preset's Noul is used; the rest cost tokens, not latency.
func buildMatchRequest(info reqInfo, live []presets.Preset) (any, map[string]jev.Question) {
	state := map[string]string{"user_message": tail(info.UserText, maxStateChars)}
	if info.PrevAssistant != "" {
		state["previous_assistant_answer"] = tail(info.PrevAssistant, maxStateChars)
	}
	criteria := map[string]any{}
	questions := map[string]jev.Question{}
	for i, p := range live {
		criteria[optionKey(i)] = p.ExpectedInput
		questions[fullKey(i)] = fullAnswerQuestion(p)
	}
	criteria[noneOption] = "None of the listed follow-ups: the message asks something else, adds new " +
		"requirements or details, or asks for more than a listed follow-up covers."
	questions["match"] = jev.Choice(
		"Which anticipated follow-up does `user_message` correspond to? The user is replying to "+
			"`previous_assistant_answer`.",
		criteria,
	)
	return state, questions
}

// fullAnswerQuestion asks whether the message is the one the preset was prepared for,
// with nothing added that the reply leaves unanswered. Jev reads questions literally, and
// the wording matters (go test -tags live ./internal/proxy):
//   - Asking only whether the reply "addresses every detail" of the message fails terse
//     messages: for "6" and the reply "Nope, lower." Jev can't tell the reply fits (0.46).
//     Showing it expected_input fixes that (0.80-0.89).
//   - The explicit "answer no if it adds ..." keeps messages that add a condition or a
//     second question low (at most 0.64), well under the threshold. Its misses are the
//     cheap kind: a real match scored too low just gets a normal answer.
func fullAnswerQuestion(p presets.Preset) jev.Question {
	return jev.Noul(map[string]string{
		"prepared_for":      p.ExpectedInput,
		"prepared_response": p.Response,
		"question": "Is `user_message` exactly the kind of message described by `prepared_for`? " +
			"Answer no if it adds any question, condition, constraint, or detail that " +
			"`prepared_response` does not address.",
	})
}

// match evaluates the live presets against the user's message.
func (s *Server) match(ctx context.Context, info reqInfo, live []presets.Preset) matchResult {
	state, questions := buildMatchRequest(info, live)
	ctx, cancel := context.WithTimeout(ctx, s.cfg.JevTimeout)
	defer cancel()
	start := time.Now()
	resp, err := s.jev.Evaluate(ctx, state, questions)
	r := matchResult{Latency: time.Since(start), Response: resp, Err: err}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		r.Reason = "timeout"
		return r
	case err != nil:
		r.Reason = "error"
		return r
	}
	return s.decide(r, live)
}

// decide applies the hit rules: a preset (not "none") was chosen, the Choice confidence
// meets the preset's min_confidence (or the default), and the full-answer Noul meets the
// configured threshold.
func (s *Server) decide(r matchResult, live []presets.Preset) matchResult {
	ans, ok := r.Response.Answers["match"]
	if !ok {
		r.Reason, r.Err = "error", errors.New("jev response has no match answer")
		return r
	}
	r.Choice, r.Confidence = ans.Choice, ans.Confidence
	if r.Choice == noneOption {
		r.Reason = "none"
		return r
	}
	idx := -1
	for i := range live {
		if optionKey(i) == r.Choice {
			idx = i
		}
	}
	if idx < 0 {
		r.Reason, r.Err = "error", fmt.Errorf("jev chose unknown option %q", r.Choice)
		return r
	}
	p := live[idx]
	r.Preset = &p
	r.FullAnswer = r.Response.Answers[fullKey(idx)].Noul
	r.MinConf = p.MinConfidence
	if r.MinConf == 0 {
		r.MinConf = s.cfg.MinConfidence
	}
	switch {
	case r.Confidence < r.MinConf:
		r.Reason = "low_confidence"
	case r.FullAnswer < s.cfg.FullAnswerThreshold:
		r.Reason = "not_full_answer"
	default:
		r.Hit, r.Reason = true, "hit"
	}
	return r
}

// logMatch records a match decision. shadow means the result did not affect the response.
func (s *Server) logMatch(info reqInfo, live []presets.Preset, r matchResult, shadow bool) {
	s.stats.Inc("match_candidates")
	fields := map[string]any{
		"session":      info.SessionID,
		"live_presets": len(live),
		"reason":       r.Reason,
		"hit":          r.Hit,
		"shadow":       shadow,
		"latency_ms":   r.Latency.Milliseconds(),
		"user_message": truncate(info.UserText, maxLoggedMessage),
		"choice":       r.Choice,
		"confidence":   r.Confidence,
	}
	if r.Err != nil {
		fields["error"] = r.Err.Error()
	}
	if r.Response != nil {
		fields["jev_model"] = r.Response.Model
		fields["jev_input_tokens"] = r.Response.Usage.InputTokens
		fields["probabilities"] = r.Response.Answers["match"].Probabilities
		nouls := map[string]float64{}
		for i, p := range live {
			nouls[p.ID] = r.Response.Answers[fullKey(i)].Noul
		}
		fields["full_answer_by_preset"] = nouls
	}
	if r.Preset != nil {
		fields["preset_id"] = r.Preset.ID
		fields["expected_input"] = r.Preset.ExpectedInput
		fields["full_answer"] = r.FullAnswer
		fields["min_confidence"] = r.MinConf
	}
	if r.Reason == "timeout" || r.Reason == "error" {
		// Failures fall back to normal answers silently, so say so on the console.
		if n := s.jevFailures.Add(1); n == 3 {
			fmt.Fprintf(os.Stderr, "irp: Jev failed %d times in a row (last: %v); answers are coming from the model only\n", n, r.Err)
		}
	} else if s.jevFailures.Swap(0) >= 3 {
		fmt.Fprintln(os.Stderr, "irp: Jev is answering again")
	}
	switch r.Reason {
	case "hit":
		s.stats.Inc("match_hits")
	case "timeout":
		s.stats.Inc("jev_timeouts")
		s.stats.Inc("match_misses")
	case "error":
		s.stats.Inc("jev_errors")
		s.stats.Inc("match_misses")
	default:
		s.stats.Inc("match_misses")
	}
	s.log.Log("match", fields)
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return "…" + string(r[len(r)-n:])
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
