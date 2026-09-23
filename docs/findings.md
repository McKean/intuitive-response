# Findings and PRD deviations

Verified against the Claude API docs (2026-09) while building milestones 1–2.

## Resolved open questions

- **Assistant prefill (PRD §7.2, §13).** Returns 400 on every current model: Opus 5 / 5.5,
  Sonnet 5, Fable 5 / 5.1, and the 4.6–4.8 family. `continuation` now defaults to `note`;
  `prefill` only makes sense on older models.
- **Disabling thinking on continuations (PRD §7.2).** Not possible on Opus 5.5 or Fable 5.x:
  `thinking: {type: "disabled"}` is a 400. Use a low `output_config.effort` on the
  continuation request instead, and drop thinking blocks from its output (below).

## Preserved thinking constrains the design

On Opus 5.5 and Fable 5.1, a thinking block's signature records the conversation prefix
before it (system prompt, tools, all earlier messages). Accounts created on or after
2026-08-31 get a 400 when that prefix changes; older accounts can opt in to the check.

- **Stripping presets is safe.** The block is the last content in the assistant turn, so no
  thinking block sits after the stripped text.
- **The injected instruction must never change mid-session.** That includes toggling
  `IRP_INJECT` or restarting the proxy with a different build of `Instruction`. Changing it
  invalidates every earlier thinking block in running sessions (and busts the prompt cache).
- **Hit path (milestone 4).** With `note`, the continuation's thinking blocks are produced
  after a synthetic user message that Claude Code never sees. Forwarding them into the open
  message would put them under a prefix that doesn't match. Drop continuation thinking blocks
  instead of forwarding them. Their removal is at the tail of the chain, which is allowed.
- **Test this explicitly** with `thinking.block_binding.prefix_mismatch_behavior: "error"`
  (beta `thinking-binding-controls-2026-08-01`) on a few captured transcripts before trusting
  milestone 4.

## Implementation choices that differ from the PRD

- **Invalidation (§6.4).** The PRD says to drop presets with an older `AnchorTurn` after a
  file-modifying tool call. Presets are only stored after a final text answer, so every live
  preset predates any later tool call. The proxy therefore drops all of the session's
  presets when it sees a `Write`/`Edit`/`MultiEdit`/`NotebookEdit`/`Bash` `tool_use` in a
  response. This is also robust to `/compact`, which renumbers turns.
- **Tag detection is stricter than "contains `<presets>`".** The tag must start a line and
  be followed (after whitespace) by `[`. That way, discussing this proxy inside Claude Code
  doesn't truncate answers. Whitespace before the tag is dropped.
- **Main-model requests** are those with a non-empty `tools` array on a non-Haiku model.
  Only these get the instruction, decrement `TurnsLeft`, or are stripped.
- **Session ID** comes from `X-Claude-Code-Session-Id`, then from `metadata.user_id`
  (`..._session_<id>` or a JSON `session_id`), then falls back to `default`.
- **Early-close usage** reports `output_tokens` as message_start's count plus about 4 bytes
  per token of visible text.

## Jev (milestone 3)

- **API.** The endpoint is `POST https://api.typesafe.ai/v1/systemone` with `Authorization: Bearer`.
  The request body is `{model, state, questions: {id: {type, instructions, criteria}}}`.
  The PRD's "Boolean" question is called **Noul** and returns `noul` ∈ [0,1]. Choice returns
  `choice`, `probabilities`, and `confidence`, and allows up to 255 options. Model
  `jev-1.13.0` is pinned; `jev-latest` would change under tuned thresholds.
- **One call per candidate.** Each request has a Choice over the presets plus `none`, and
  one speculative Noul per preset. Questions are evaluated in parallel, so this costs tokens,
  not latency, and avoids a second round trip after the Choice.
- **State is kept small**: `user_message` plus `previous_assistant_answer`, each capped at the
  last 4000 characters. Jev's docs note that accuracy drops with irrelevant state, so the
  conversation history isn't sent.
- **Latency from Basel**: about 250–350 ms warm, about 380 ms cold. The PRD's 300 ms timeout
  would miss about half of calls. The default is now 800 ms, which is safe while matching is
  shadow-only. Revisit it before the hit path goes live, because the timeout is added
  directly to time-to-first-token on every candidate.
- **The Choice question alone is too eager.** Messages that add a second question or an extra
  condition still map to the closest preset with 0.9–0.99 confidence. The Noul question
  carries the precision. Its wording matters a lot (`go test -tags live ./internal/proxy`):
  - "Does it fully answer…" scored correct answers only 0.67–0.83. That's too close to the
    0.8 threshold.
  - "Is every question, condition, and detail in `user_message` addressed by
    `prepared_response`?" scored correct answers 0.82–0.93 and bad matches 0.08–0.55.
    It was used until real use showed it fails terse messages: for "6" and "Nope, lower."
    Jev can't tell the reply fits (0.46).
  - Current: the question also shows Jev `expected_input` ("prepared_for") and asks whether
    the message is exactly that kind of message, answering no if it adds anything the reply
    doesn't address. On the 18-case live suite, wrong matches stay at or below 0.63 and real
    matches score 0.84-0.96. One paraphrase ("wouldn't sqlite be simpler") scores 0.75 and
    falls through, which only costs a normal answer.
- **First messages** often carry a `<system-reminder>` in the same text block as the user's
  text. They are now classified as typed user turns (reminders are removed before the check).

## Hit path (milestone 4)

- **Jev runs alongside the upstream request.** On a hit the upstream request is cancelled.
  On a miss its response is used, so a miss costs only the time Jev takes beyond upstream's
  response headers.
- **The preset is sent as a closed text block 0.** The message stays open while the `note`
  continuation streams into it. Continuation text becomes new text blocks from index 1.
  A reply of exactly `<none>` is suppressed. A presets block in the continuation is stripped
  and stored, so a game can go on hitting turn after turn.
- **Deviation from the PRD: continuation thinking blocks and tool calls are dropped.** The
  thinking signatures cover the synthetic note, which Claude Code never sends back. A
  `tool_use` without its thinking block would be rejected on the tool_result turn. The note
  tells the model not to call tools. If it calls one anyway, the call is dropped and the
  turn ends as `end_turn`, logged as outcome `tool_use_dropped`.
- **Prefill** is attempted when configured. On a 400 (every current model) the proxy retries
  once with `note`.
- **Claude Code's prompt-suggestion request** (`[SUGGESTION MODE: …]`) uses the main model
  and tools. It is now classified as background, so it no longer uses up presets' turns or
  stores presets of its own.

## Hidden memory

- **The stripped block is restored into the model's view of history.** On every later request
  in the session it is appended back onto the text block it was stripped from. The model sees
  its earlier presets and any `{"note": …}` items; Claude Code and the user never do. This
  also makes the history the model sees exactly what it generated.
- **Entries are keyed by assistant-message position and a hash of the visible text.** They
  are persisted in `~/.local/state/irp/hidden/<session>.jsonl`, because restoring must be
  identical on every request or later thinking blocks become invalid.
- **Only sessions whose first request the proxy saw are tracked.** A session already running
  when the proxy (or this feature) started keeps its stripped history.
- **Races.** A request waits (up to 30 s) while the previous reply's block is still
  streaming after the early close, so it never goes out without it. The same wait closes a
  race where presets were stored after the user's next message arrived.
- **Instant replies.** The block is restored onto the message's last visible text block.
  When the model's follow-up is a suppressed `<none>`, that is the preset text itself.
- **The instruction no longer has game-specific examples.** Private notes cover that case
  generically. `IRP_HIDDEN_MEMORY=false` turns the feature off; for running sessions this
  has the same caveat as changing the instruction.

## Replacement and confidence (after real use)

- **`replace_mode` now defaults to `all`.** With `ring`, presets from earlier answers stayed
  live next to the new ones. Near-duplicates ("guesses 1 to 7" and "guesses 2 to 7") split
  Jev's vote 53/45, so neither was confident enough. Worse, presets from a finished game
  could still have answered the first guess of the next one.
- **The model no longer sets `min_confidence`.** It picked 0.9 for important presets, but
  Jev's confidence isn't the chosen option's probability. It depends on how many options
  there are (0.89 over four options is 0.85). A correct match was blocked that way. The
  configured threshold now applies; admin presets can still set their own. Match logs
  include the effective `min_confidence`.

## Still open
- How Claude Code renders a message that pauses after the first text block (milestone 4).
- Whether Claude Code is fine with an early-closed stream on long answers. It should be,
  since it's a normal `end_turn`, but check with real sessions.
