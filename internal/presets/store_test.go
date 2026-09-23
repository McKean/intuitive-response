package presets

import (
	"fmt"
	"testing"
)

func items(names ...string) []ModelPreset {
	var out []ModelPreset
	for _, n := range names {
		out = append(out, ModelPreset{ExpectedInput: n, Response: "r " + n, MaxTurns: 2})
	}
	return out
}

func inputs(ps []Preset) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.ExpectedInput)
	}
	return out
}

func TestRingEvictsLowestSeq(t *testing.T) {
	s := NewStore(3, "ring")
	s.AddFromModel("s1", 1, items("a", "b"))
	c := s.AddFromModel("s1", 2, items("c", "d"))
	if c.Evicted != 1 {
		t.Fatalf("evicted = %d", c.Evicted)
	}
	if got := fmt.Sprint(inputs(s.List("s1"))); got != "[b c d]" {
		t.Fatalf("got %s", got)
	}
}

func TestReplaceSameExpectedInput(t *testing.T) {
	s := NewStore(10, "ring")
	s.AddFromModel("s1", 1, items("Asks Why", "b"))
	c := s.AddFromModel("s1", 2, items("asks why"))
	list := s.List("s1")
	if c.Replaced != 1 || len(list) != 2 || list[1].ExpectedInput != "asks why" || list[1].AnchorTurn != 2 {
		t.Fatalf("counts %+v list %+v", c, list)
	}
}

func TestAllModeReplacesSession(t *testing.T) {
	s := NewStore(10, "all")
	s.AddFromModel("s1", 1, items("a", "b"))
	s.AddFromModel("s2", 1, items("x"))
	s.AddFromModel("s1", 2, items("c"))
	if got := fmt.Sprint(inputs(s.List("s1"))); got != "[c]" {
		t.Fatalf("got %s", got)
	}
	if s.Len("s2") != 1 {
		t.Fatal("other session touched")
	}
}

func TestUserTurnExpiry(t *testing.T) {
	s := NewStore(10, "ring")
	s.AddFromModel("s1", 1, []ModelPreset{
		{ExpectedInput: "one", Response: "r", MaxTurns: 1},
		{ExpectedInput: "two", Response: "r", MaxTurns: 2},
	})
	if c := s.UserTurn("s1"); c.Expired != 1 || s.Len("s1") != 1 {
		t.Fatalf("after turn 1: %+v len %d", c, s.Len("s1"))
	}
	if c := s.UserTurn("s1"); c.Expired != 1 || s.Len("s1") != 0 {
		t.Fatalf("after turn 2: %+v len %d", c, s.Len("s1"))
	}
}

func TestTakeAndInvalidate(t *testing.T) {
	s := NewStore(10, "ring")
	s.AddFromModel("s1", 1, items("a", "b"))
	id := s.List("s1")[0].ID
	if p, ok := s.Take(id); !ok || p.ExpectedInput != "a" || s.Len("s1") != 1 {
		t.Fatalf("take: %+v %v", p, ok)
	}
	if c := s.Invalidate("s1"); c.Invalidated != 1 || s.Len("s1") != 0 {
		t.Fatalf("invalidate: %+v", c)
	}
}

func TestParse(t *testing.T) {
	got, err := Parse(` [
		{"expected_input":"a","response":"x","max_turns":9,"min_confidence":0.9},
		{"expected_input":"","response":"dropped"},
		{"expected_input":"b","response":"y","min_confidence":7},
		{"expected_input":"c","response":"z"},{"expected_input":"d","response":"z"},
		{"expected_input":"e","response":"over the limit"}
	]</presets> trailing`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got[0].MaxTurns != 3 || *got[0].MinConfidence != 0.9 ||
		got[1].MaxTurns != 1 || got[1].MinConfidence != nil {
		t.Fatalf("got %+v", got)
	}
	if got, err := Parse(`[{"note":"picked 4"}]`); err != nil || len(got) != 0 {
		t.Fatalf("notes-only block: %v %v", got, err)
	}
	for _, bad := range []string{"", "not json</presets>", "[]", `[{"response":"x"}]`} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("Parse(%q) should fail", bad)
		}
	}
}
