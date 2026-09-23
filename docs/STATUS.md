# PRD: Intuitive Response Proxy

Status: experiment · Language: Go · Owner: Chris

See `docs/findings.md` for deviations discovered while building. The original PRD text was supplied in conversation; the milestones below track implementation state.

## Milestones

1. **Passthrough** — implemented (`internal/proxy`).
2. **Preset authoring** — implemented (instruction injection, stream stripping with early close, store, admin endpoints).
3. **Matching (shadow mode)** — implemented (`internal/jev`, `internal/proxy/match.go`). Runs
   off the request path and logs `match` events; never changes a response. Needs a day of
   real use to be "done".
4. **Hit path** — implemented (`internal/proxy/continuation.go`), on by default
   (`IRP_MATCH_MODE=live`; `shadow` logs only). Needs real-use verification of how Claude Code
   renders the paused message.
5. **Evaluation** — not started. `IRP_TRAFFIC_LOG=1` captures the inputs it needs.
