# irp: the Intuitive Response Proxy

<video src="https://github.com/user-attachments/assets/2e9245bc-d5bf-4c1f-beea-c05800fa2f84" controls muted width="100%"></video>

> "I knew you were going to ask that."

People don't wait for you to finish your sentence before they start working on a reply. By the time you get to the question mark, they've already guessed where you were going and have an answer half-loaded. Claude, left to its own devices, waits politely for the whole message and then thinks from scratch. Every time. Even when you just said "thanks".

`irp` is a small Go proxy that sits between Claude Code and the Anthropic API and gives Claude a fast lane, a little System 1 to go with its System 2:

1. At the end of every answer, Claude quietly writes down a few things you're likely to say next, along with the replies it would give. We call these **presets**.
2. The proxy snips them out of the stream before you ever see them and keeps them in a drawer.
3. When your next message arrives, **Jev** (TypeSafe AI's typed-decision model) takes a quick look: does this match one of the presets, and would that prepared reply actually answer what you asked?
4. If yes, the reply shows up in about a quarter of a second. Meanwhile Claude gets a look at what it just "said" and can add something or fix it, in the same message.
5. If no, nothing happens. You get the normal answer, and the proxy was never there.

The deliberate model writes its own intuitions. Jev only decides whether one fits.

## What it looks like

```
you   > let's play a game, you think of a number between 1 and 10 and I'll guess it
claude> Okay, I've got a number locked in. Take your guess!
          (hidden: "guesses 5" -> "Close, go lower", "guesses 4" -> "Yes! It was 4", ...)
you   > 4
claude> 🎉 Yes! It was 4. You got it!              <- served in ~270 ms, no model call
          (Claude checks its own reply in the background, decides it's fine, stays quiet)
```

Games are the cute demo. The everyday wins are the boring ones: "why not SQLite?", "ok, run the tests", "thanks!", "and how do I roll that back?". Anything Claude can answer without reaching for a tool.

## How it works

```
Claude Code ──► irp (127.0.0.1:18766) ──► api.anthropic.com
                  │
                  ├── preset store     (in memory, per session)
                  ├── hidden memory    (on disk, per session)
                  └── Jev              (api.typesafe.ai)
```

**Writing presets.** The proxy adds one fixed paragraph to Claude's system prompt, asking it to end text answers with a `<presets>[...]</presets>` block. Each item has an `expected_input` ("asks why not SQLite"), the full `response`, and how many turns it stays relevant. A new block replaces the old one, so yesterday's predictions don't hang around to answer today's questions. The paragraph never changes, so prompt caching keeps working.

**Hiding them.** The proxy watches the stream, and the moment it sees the tag it ends the message for Claude Code right there. You don't sit around waiting for Claude to finish typing secrets. The rest is read in the background and parsed. The tag only counts when it starts a line and is followed by `[`, so you can talk about `<presets>` inside Claude Code without your answers getting chopped off.

**Matching.** When you type something and there are live presets, the proxy sends Jev one request: a Choice over the presets plus "none of these", and a yes/no question per preset asking whether it addresses everything in your message. Jev answers them all in parallel, so the extra questions cost tokens but not time. A preset fires only if Jev picks it confidently *and* the yes/no check passes. Anything that adds a condition ("why not SQLite, but in WAL mode?") or a second question falls through to Claude, which is exactly what you want.

**Not waiting on Jev.** The real request to Anthropic starts at the same moment as the Jev call. On a miss, that response is already on its way, so a miss costs next to nothing. On a hit, it's cancelled.

**The follow-up.** After a hit, Claude sees the prepared reply as something it already said and gets a note: add what's missing, fix what's wrong, or reply `<none>`. Whatever it adds streams into the same message. `<none>` is swallowed. It can also write fresh presets, so a conversation can keep hitting turn after turn.

**Hidden memory.** Presets are also a scratchpad. The model can add `{"note": "..."}` items for things it has decided but not told you (say, the number it's thinking of). The proxy puts each stripped block back into the model's view of the conversation on every later turn, so the model remembers, and you still never see it.

## Quick start

```sh
go install github.com/McKean/intuitive-response/cmd/irp@latest   # or: go build -o irp ./cmd/irp
irp                                              # reads .env from the current directory
ANTHROPIC_BASE_URL=http://127.0.0.1:18766 claude  # in another terminal
```

Copy `.env.example` to `.env` and put your TypeSafe key in `JEV_SECRET` (or export `TYPESAFE_API_KEY`). `.env` is gitignored. Without a key the proxy still strips and stores presets, it just never serves them.

Then poke at it:

```sh
watch -n1 -t scripts/presets                                     # live table of what Claude is expecting
curl -s localhost:18766/_irp/stats                               # counters
tail -f ~/.local/state/irp/proxy.log                             # everything, as JSON lines
```

A couple of handy log queries:

```sh
# every match decision, and why
jq -c 'select(.event=="match") | {user_message, reason, expected_input, confidence, full_answer, latency_ms}' ~/.local/state/irp/proxy.log

# every served hit, and what the follow-up did
jq -c 'select(.event=="hit") | {outcome, instant_ms, continuation_first_text_ms, appended_bytes}' ~/.local/state/irp/proxy.log
```

## Admin API

Same port, under `/_irp/`. Handy for testing without waiting for Claude to predict something.

| Endpoint | What it does |
|---|---|
| `GET /_irp/presets?session=…` | List live presets (all sessions if you leave out `session`) |
| `POST /_irp/presets` | Add one by hand: `{"session", "expected_input", "response", "max_turns", "min_confidence"}` |
| `DELETE /_irp/presets/{id}` | Delete one |
| `DELETE /_irp/presets?session=…` | Delete a whole session's presets |
| `GET /_irp/stats` | Counters: candidates, hits, misses, follow-up outcomes, created, expired, evicted, invalidated |
| `GET /healthz` | Still alive? |

## Configuration

Environment beats `~/.config/irp/config.json`, which beats the defaults. A `.env` in the working directory fills in anything not already set.

| Env | Default | What it does |
|---|---|---|
| `IRP_PORT` | `18766` | Where the proxy listens (always 127.0.0.1) |
| `IRP_UPSTREAM` | `https://api.anthropic.com` | Where requests go |
| `JEV_SECRET` / `TYPESAFE_API_KEY` | none | TypeSafe key; no key means no matching |
| `IRP_JEV_MODEL` | `jev-1.13.0` | Pinned on purpose, so tuned thresholds don't drift with `jev-latest` |
| `IRP_JEV_TIMEOUT_MS` | `800` | Give up on Jev after this long (it usually answers in 250 to 350 ms) |
| `IRP_MATCH_MODE` | `live` | `live` serves hits, `shadow` only logs what it would have done |
| `IRP_MIN_CONFIDENCE` | `0.8` | Choice confidence needed (presets added through the admin API can set their own) |
| `IRP_FULL_ANSWER_THRESHOLD` | `0.8` | How sure Jev must be that the reply covers the whole message |
| `IRP_MAX_PRESETS` | `10` | Per session; the oldest one goes when it's full |
| `IRP_REPLACE_MODE` | `all` | `all` replaces presets with every new block, `ring` keeps older ones around until they expire or get pushed out |
| `IRP_CONTINUATION` | `note` | How the follow-up is requested (`prefill` is rejected by current models and falls back to `note`) |
| `IRP_CONTINUATION_TIMEOUT_S` | `20` | Close the message after this long, even if Claude is still thinking |
| `IRP_INJECT` | `true` | Set to `false` for a plain passthrough |
| `IRP_HIDDEN_MEMORY` | `true` | Give stripped blocks back to the model on later turns |
| `IRP_TRAFFIC_LOG` | `false` | Save full request and response captures to `~/.local/state/irp/traffic/` |
| `IRP_STATE_DIR` | `~/.local/state/irp` | Logs, captures, hidden memory |

## Things that will bite you

- **Start a new Claude Code session after changing the instruction or `IRP_HIDDEN_MEMORY`.** On Opus 5.5 the model's earlier thinking is only valid if everything before it in the conversation stays exactly the same. Changing what the proxy adds mid-session breaks that (and your prompt cache).
- **Hidden memory only kicks in for sessions the proxy saw from the first message.** Sessions that were already running keep going as they were. That's deliberate, see above.
- **Presets are forgotten on restart; hidden memory isn't.** Presets are cheap and go stale quickly anyway.
- **File-changing tools wipe the slate.** Any `Write`, `Edit`, `MultiEdit`, `NotebookEdit` or `Bash` call drops the session's presets, because "here's what the code does" might not be true anymore.
- **Follow-ups can't use tools.** Thinking blocks and tool calls from the follow-up are dropped, because they were produced in a context Claude Code never sees. If Claude really needs a tool, it says so and you ask again.
- **Check your plan's terms** before routing your own Claude Code traffic through a proxy.

## Development

```sh
go test -race ./...                                           # unit and integration tests
go test -tags live -run TestLiveMatch -v ./internal/proxy     # the match questions against real Jev
```

| Package | What's in it |
|---|---|
| `cmd/irp` | Entry point |
| `internal/proxy` | Passthrough, classification, injection, matching, the hit path, hidden memory restore |
| `internal/presets` | The stream stripper, the parser, the per-session store |
| `internal/hidden` | Persistent hidden memory |
| `internal/jev` | A tiny TypeSafe client (there's no Go SDK) |
| `internal/sse` | Reading and writing server-sent events |
| `internal/admin` | The `/_irp/` endpoints |
| `internal/logx` | JSON-lines log, redaction, counters, traffic capture |
| `internal/config` | Settings |

`docs/findings.md` has everything we learned while building it: what the API allows, how Jev behaved on real messages, and where we deliberately went off-script from the original plan. `docs/STATUS.md` tracks the milestones.

## Literature

None of this is a new idea for brains. Humans have been doing it for a while. Titles link to a readable copy where one exists.

### Turn-taking: replies are planned before the other person is done

Across languages, the typical gap between conversational turns is around 200 ms, while planning even a short utterance takes 600 ms or more. The only way that adds up is if listeners predict where a turn is heading and start building their reply mid-turn.

- Stivers, T., et al. (2009). [Universals and cultural variation in turn-taking in conversation](https://doi.org/10.1073/pnas.0903616106). *PNAS*, 106(26), 10587-10592.
- Levinson, S. C., & Torreira, F. (2015). [Timing in turn-taking and its implications for processing models of language](https://pmc.ncbi.nlm.nih.gov/articles/PMC4464110). *Frontiers in Psychology*, 6, 731.
- Bögels, S., Magyari, L., & Levinson, S. C. (2015). [Neural signatures of response planning occur midway through an incoming question in conversation](https://www.ncbi.nlm.nih.gov/pmc/articles/PMC4525376/). *Scientific Reports*, 5, 12881. EEG evidence that planning starts as soon as the answer is predictable.
- Bögels, S., Casillas, M., & Levinson, S. C. (2018). [Planning versus comprehension in turn-taking: Fast responders show reduced anticipatory processing of the question](https://www.mpi.nl/publications/item2515661/planning-versus-comprehension-turn-taking-fast-responders-show-reduced). *Neuropsychologia*, 109, 295-310. Answering early has a price: fast responders pay less attention to the rest of the question. Which is exactly why our follow-up gets to take a second look.

### Cached vs. deliberate control, arbitrated by uncertainty

The closest match to Jev's confidence gate. Reinforcement-learning neuroscience describes a fast "model-free" system running on cached values (habits) and a slow "model-based" system that plans. Which one gets to act depends on how uncertain each of them is.

- Daw, N. D., Niv, Y., & Dayan, P. (2005). [Uncertainty-based competition between prefrontal and dorsolateral striatal systems for behavioral control](https://www.doi.org/10.1038/NN1560). *Nature Neuroscience*, 8(12), 1704-1711. ([PDF](https://sites.socsci.uci.edu/~lpearl/courses/readings/DawEtAl2005.pdf)) It literally uses the word "cache".
- Keramati, M., Dezfouli, A., & Piray, P. (2011). [Speed/accuracy trade-off between the habitual and the goal-directed processes](https://doi.org/10.1371/journal.pcbi.1002055). *PLoS Computational Biology*, 7(5), e1002055. ([open access](https://openaccess.city.ac.uk/id/eprint/20728/)) Arbitration as a speed/accuracy trade-off, which is more or less what tuning our thresholds is.

### Recognition-primed decisions

Gary Klein studied firefighters and military commanders and found they rarely weigh options against each other. They recognize a situation as a familiar type, pull out the matching action, and run a quick mental simulation to check it fits. Recognize, then check: our Choice plus yes/no question, in a fire helmet.

- Klein, G. (1998). *Sources of Power: How People Make Decisions*. MIT Press. No free copy, but here's [a good review](https://www.jasoncollins.blog/posts/gary-kleins-sources-of-power-how-people-make-decisions).

### Prediction in language comprehension

The brain is constantly guessing the next word, partly by running its own speech production system as a forward model of what the other person is about to say.

- Pickering, M. J., & Garrod, S. (2013). [An integrated theory of language production and comprehension](https://eprints.gla.ac.uk/82858/1/82858.pdf). *Behavioral and Brain Sciences*, 36(4), 329-347.
- Kutas, M., & Federmeier, K. D. (2011). [Thirty years and counting: Finding meaning in the N400 component of the event-related brain potential (ERP)](https://pmc.ncbi.nlm.nih.gov/articles/PMC4052444). *Annual Review of Psychology*, 62, 621-647. The N400 as a signature of a prediction gone wrong.
- Clark, A. (2013). [Whatever next? Predictive brains, situated agents, and the future of cognitive science](https://doi.org/10.1017/S0140525X12000477). *Behavioral and Brain Sciences*, 36(3), 181-204. The bigger picture.

### Prefabricated chunks

A good share of everyday speech isn't assembled word by word at all. It's pulled off the shelf as ready-made multi-word units, the linguistic version of a preset.

- Wray, A. (2002). [*Formulaic Language and the Lexicon*](https://orca.cardiff.ac.uk/id/eprint/3749/). Cambridge University Press.

### Self-monitoring and repair: say it first, fix it after

Speakers keep an ear on their own output and patch it on the fly ("left, uh, I mean right"), often after the words are already out. That's the follow-up path: answer instantly, then correct if needed.

- Levelt, W. J. M. (1983). [Monitoring and self-repair in speech](https://doi.org/10.1016/0010-0277%2883%2990026-4). *Cognition*, 14, 41-104.
- Levelt, W. J. M. (1989). *Speaking: From Intention to Articulation*. MIT Press. The book-length version.
- Clark, H. H., & Fox Tree, J. E. (2002). [Using *uh* and *um* in spontaneous speaking](https://gwern.net/doc/psychology/linguistics/2002-clark-2.pdf). *Cognition*, 84(1), 73-111. Fillers as a signal that more is coming, which might be how an instant reply could hint that Claude isn't quite done yet.

### Related AI work

Other people teaching machines to think fast and slow:

- Christakopoulou, K., Mourad, S., & Matarić, M. (2024). [Agents Thinking Fast and Slow: A Talker-Reasoner Architecture](https://arxiv.org/abs/2410.08328v1). NeurIPS 2024 Workshop. A fast conversational "talker" up front, a slower "reasoner" planning behind it.
- [Speculative Actions: A Lossless Framework for Faster Agentic Systems](https://arxiv.org/pdf/2510.04371) (2025). Guess the next action, run it early, and keep it only if the slow path agrees. Speculative execution for agents, and a cousin of the parallel request trick above.

## License

MIT, see [LICENSE](LICENSE).
