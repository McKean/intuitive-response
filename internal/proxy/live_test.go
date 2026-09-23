//go:build live

// Live check of the match questions against the real Jev API:
//
//	go test -tags live -run TestLiveMatch -v ./internal/proxy
package proxy

import (
	"context"
	"os"
	"testing"

	"github.com/McKean/intuitive-response/internal/config"
	"github.com/McKean/intuitive-response/internal/jev"
	"github.com/McKean/intuitive-response/internal/presets"
)

func TestLiveMatch(t *testing.T) {
	os.Chdir("../..") // pick up the repo's .env
	cfg, err := config.Load()
	if err != nil || cfg.TypeSafeAPIKey == "" {
		t.Skip("no TypeSafe key")
	}
	s := &Server{cfg: cfg, jev: jev.New(cfg.JevURL, cfg.TypeSafeAPIKey, cfg.JevModel)}
	s.jev.Warm()

	dbPrev := "I'd use Postgres here: the worker pool has concurrent writers, and SQLite serializes writes. " +
		"Run `make migrate` after pulling to create the tables."
	db := []presets.Preset{
		{ExpectedInput: "asks why not SQLite", Response: "SQLite allows a single writer at a time, so the 8 workers would queue on the write lock and time out under load. Postgres uses MVCC with row-level locks, so concurrent inserts don't block each other."},
		{ExpectedInput: "asks how to run the migrations", Response: "Run `make migrate`. It applies everything in `db/migrations/` in order and is safe to re-run."},
		{ExpectedInput: "says thanks / acknowledges", Response: "You're welcome!"},
	}
	// Terse replies where the reply alone doesn't show that it fits (a guessing game).
	terse := []presets.Preset{
		{ExpectedInput: "guesses 3", Response: "You got it: it was 3, on your third guess."},
		{ExpectedInput: "guesses 4 or 5", Response: "Nope, lower."},
		{ExpectedInput: "guesses 2", Response: "Nope, higher."},
	}
	const (
		mustHit  = iota // a real match: missing it is tolerated only if marked mayMiss
		mayMiss         // a real match that may fall through (cheap: a normal answer)
		mustMiss        // serving any preset here would be wrong
	)
	cases := []struct {
		prev, msg string
		live      []presets.Preset
		want      string // expected_input of the right preset
		kind      int
	}{
		{dbPrev, "why not sqlite?", db, "asks why not SQLite", mustHit},
		{dbPrev, "hmm, wouldn't sqlite be simpler for this?", db, "asks why not SQLite", mayMiss},
		{dbPrev, "how do I run the migrations", db, "asks how to run the migrations", mustHit},
		{dbPrev, "what's the command for the migrations?", db, "asks how to run the migrations", mustHit},
		{dbPrev, "thanks!", db, "says thanks / acknowledges", mustHit},
		{dbPrev, "ty", db, "says thanks / acknowledges", mustHit},
		{dbPrev, "why not sqlite? and also what about mysql?", db, "", mustMiss},
		{dbPrev, "how do I roll back a migration?", db, "", mustMiss},
		{dbPrev, "ok now add an index on users.email", db, "", mustMiss},
		{dbPrev, "why not sqlite in WAL mode with a single writer goroutine?", db, "", mustMiss},
		{dbPrev, "why not sqlite, we only ever have one writer", db, "", mustMiss},
		{dbPrev, "how do I run the migrations on staging?", db, "", mustMiss},
		{dbPrev, "thanks! can you also add tests for it?", db, "", mustMiss},
		{"Nope, lower.", "3", terse, "guesses 3", mustHit},
		{"Nope, lower.", "4", terse, "guesses 4 or 5", mustHit},
		{"Nope, lower.", "is it odd?", terse, "", mustMiss},
		{"Nope, lower.", "3 or 4?", terse, "", mustMiss},
		{"Nope, lower.", "let's stop, can you explain how you picked it?", terse, "", mustMiss},
	}
	for _, c := range cases {
		r := s.match(context.Background(), reqInfo{UserText: c.msg, PrevAssistant: c.prev}, c.live)
		got := ""
		if r.Hit {
			got = r.Preset.ExpectedInput
		}
		bad := got != c.want && !(c.kind == mayMiss && got == "")
		mark := "ok  "
		if bad {
			mark = "FAIL"
		}
		t.Logf("%s %-60q choice=%-9s conf=%.2f full=%.2f reason=%-15s %dms",
			mark, c.msg, r.Choice, r.Confidence, r.FullAnswer, r.Reason, r.Latency.Milliseconds())
		if bad {
			t.Fail()
		}
	}
}
