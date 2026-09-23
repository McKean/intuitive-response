package hidden

import (
	"testing"
	"time"
)

func TestTrackingAndPersistence(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	if tracked, _ := s.Observe("late", false); tracked {
		t.Fatal("a session first seen mid-way must not be tracked")
	}
	if tracked, _ := s.Observe("s1", true); !tracked {
		t.Fatal("a fresh session must be tracked")
	}
	if err := s.Record("s1", 0, "Hello.", "\n\n<presets>[]</presets>"); err != nil {
		t.Fatal(err)
	}
	s.Record("late", 0, "Hello.", "x") // ignored: untracked
	if _, ok := s.Lookup("late", 0, "Hello."); ok {
		t.Fatal("untracked session restored")
	}

	// A restarted proxy restores from disk, even when it first sees the session mid-way.
	s2, _ := New(dir)
	if tracked, _ := s2.Observe("s1", false); !tracked {
		t.Fatal("persisted session not tracked after restart")
	}
	if got, ok := s2.Lookup("s1", 0, "Hello."); !ok || got != "\n\n<presets>[]</presets>" {
		t.Fatalf("lookup %q %v", got, ok)
	}
	if _, ok := s2.Lookup("s1", 1, "Hello."); ok {
		t.Fatal("same text at another turn must not match")
	}
}

func TestWaitForPending(t *testing.T) {
	s, _ := New(t.TempDir())
	s.Observe("s1", true)
	if !s.Wait("s1", time.Millisecond) {
		t.Fatal("idle session should not block")
	}
	s.Begin("s1")
	if s.Wait("s1", 20*time.Millisecond) {
		t.Fatal("pending session should block")
	}
	go func() {
		time.Sleep(20 * time.Millisecond)
		s.End("s1")
	}()
	if !s.Wait("s1", time.Second) {
		t.Fatal("wait did not return after End")
	}
}
