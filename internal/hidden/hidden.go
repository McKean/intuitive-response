// Package hidden remembers the text the proxy strips from assistant replies (the presets
// block) so it can be restored into the model's view of the conversation on later requests.
// The user never sees it; the model always does.
//
// Restoring changes the conversation the model sees, so it must happen on every request of
// a session from the first turn on, byte-identically: with preserved thinking, a later
// thinking block is only valid if the history before it is unchanged. That is why entries
// are persisted, and why only sessions observed from their first request are tracked.
package hidden

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"
)

type Store struct {
	dir      string
	mu       sync.Mutex
	sessions map[string]*session
}

type session struct {
	tracked bool
	entries map[string]string // key(turn, visible) -> suffix
	pending int
	idle    chan struct{} // closed when pending drops to 0; nil while idle
}

type entry struct {
	Turn   int    `json:"turn"`
	Hash   string `json:"hash"`
	Suffix string `json:"suffix"`
}

func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir, sessions: map[string]*session{}}, nil
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9_-]`)

func (s *Store) path(sessionID string) string {
	return filepath.Join(s.dir, unsafeChars.ReplaceAllString(sessionID, "_")+".jsonl")
}

func key(turn int, visible string) string {
	h := sha256.Sum256([]byte(visible))
	return keyHash(turn, hex.EncodeToString(h[:]))
}

func keyHash(turn int, hash string) string {
	b, _ := json.Marshal([]any{turn, hash})
	return string(b)
}

// Observe loads the session and reports whether it is tracked. A session becomes tracked
// when it is first seen with no assistant turns (fresh); a session first seen mid-way
// started without restoration and must stay that way.
func (s *Store) Observe(sessionID string, fresh bool) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ss, ok := s.sessions[sessionID]; ok {
		if !ss.tracked && fresh && len(ss.entries) == 0 {
			// e.g. /clear reuses nothing but starts from zero turns: start tracking now.
			return s.startLocked(sessionID, ss)
		}
		return ss.tracked, nil
	}
	ss := &session{entries: map[string]string{}}
	s.sessions[sessionID] = ss
	f, err := os.Open(s.path(sessionID))
	switch {
	case err == nil:
		defer f.Close()
		ss.tracked = true
		sc := bufio.NewScanner(f)
		sc.Buffer(make([]byte, 1<<20), 16<<20)
		for sc.Scan() {
			var e entry
			if json.Unmarshal(sc.Bytes(), &e) == nil && e.Hash != "" {
				ss.entries[keyHash(e.Turn, e.Hash)] = e.Suffix
			}
		}
		return true, sc.Err()
	case errors.Is(err, fs.ErrNotExist):
		if fresh {
			return s.startLocked(sessionID, ss)
		}
		return false, nil
	default:
		return false, err
	}
}

func (s *Store) startLocked(sessionID string, ss *session) (bool, error) {
	f, err := os.OpenFile(s.path(sessionID), os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return false, err
	}
	ss.tracked = true
	return true, f.Close()
}

// Lookup returns the suffix stripped from the assistant message with the given ordinal
// (0-based among assistant messages) whose last text block is visible.
func (s *Store) Lookup(sessionID string, turn int, visible string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[sessionID]
	if ss == nil || !ss.tracked {
		return "", false
	}
	suffix, ok := ss.entries[key(turn, visible)]
	return suffix, ok
}

// Record persists a stripped suffix for a tracked session.
func (s *Store) Record(sessionID string, turn int, visible, suffix string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[sessionID]
	if ss == nil || !ss.tracked {
		return nil
	}
	h := sha256.Sum256([]byte(visible))
	e := entry{Turn: turn, Hash: hex.EncodeToString(h[:]), Suffix: suffix}
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(s.path(sessionID), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	ss.entries[keyHash(e.Turn, e.Hash)] = suffix
	return nil
}

// Begin marks a stripped block whose suffix is still being read from upstream.
func (s *Store) Begin(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[sessionID]
	if ss == nil {
		return
	}
	if ss.pending == 0 {
		ss.idle = make(chan struct{})
	}
	ss.pending++
}

// End is the counterpart of Begin, called after Record (or after giving up).
func (s *Store) End(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ss := s.sessions[sessionID]
	if ss == nil || ss.pending == 0 {
		return
	}
	if ss.pending--; ss.pending == 0 {
		close(ss.idle)
		ss.idle = nil
	}
}

// Wait blocks until no suffix is pending for the session, so the next request restores
// the previous reply's block. It returns false on timeout.
func (s *Store) Wait(sessionID string, timeout time.Duration) bool {
	s.mu.Lock()
	var idle chan struct{}
	if ss := s.sessions[sessionID]; ss != nil {
		idle = ss.idle
	}
	s.mu.Unlock()
	if idle == nil {
		return true
	}
	select {
	case <-idle:
		return true
	case <-time.After(timeout):
		return false
	}
}
