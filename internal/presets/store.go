package presets

import (
	"crypto/rand"
	"encoding/hex"
	"slices"
	"strings"
	"sync"
	"time"
)

type Preset struct {
	ID            string    `json:"id"`
	SessionID     string    `json:"session_id"`
	Seq           uint64    `json:"seq"`
	ExpectedInput string    `json:"expected_input"`
	Response      string    `json:"response"`
	TurnsLeft     int       `json:"turns_left"`
	MinConfidence float64   `json:"min_confidence"` // 0 = use config default
	AnchorTurn    int       `json:"anchor_turn"`
	Source        string    `json:"source"` // "model" | "admin"
	CreatedAt     time.Time `json:"created_at"`
}

// Counts reports what a store mutation did, for stats.
type Counts struct {
	Created, Replaced, Evicted, Expired, Invalidated int
}

// Store holds live presets per session, in memory.
type Store struct {
	mu       sync.Mutex
	sessions map[string][]*Preset // ordered by Seq ascending
	seq      uint64
	max      int
	mode     string // "ring" | "all"
	now      func() time.Time
}

func NewStore(maxPerSession int, mode string) *Store {
	return &Store{sessions: map[string][]*Preset{}, max: maxPerSession, mode: mode, now: time.Now}
}

// AddFromModel inserts presets parsed from a model-authored block.
func (s *Store) AddFromModel(sessionID string, anchorTurn int, items []ModelPreset) Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Counts
	if s.mode == "all" {
		c.Replaced = len(s.sessions[sessionID])
		delete(s.sessions, sessionID)
	}
	for _, it := range items {
		p := &Preset{
			SessionID:     sessionID,
			ExpectedInput: it.ExpectedInput,
			Response:      it.Response,
			TurnsLeft:     it.MaxTurns,
			AnchorTurn:    anchorTurn,
			Source:        "model",
		}
		if it.MinConfidence != nil {
			p.MinConfidence = *it.MinConfidence
		}
		s.insertLocked(p, &c)
	}
	return c
}

// Add inserts a single preset (admin API). Missing fields get defaults.
func (s *Store) Add(p Preset) (Preset, Counts) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.TurnsLeft < 1 {
		p.TurnsLeft = defaultMaxTurn
	}
	if p.Source == "" {
		p.Source = "admin"
	}
	var c Counts
	cp := p
	s.insertLocked(&cp, &c)
	return cp, c
}

func (s *Store) insertLocked(p *Preset, c *Counts) {
	s.seq++
	p.Seq = s.seq
	p.ID = newID()
	p.CreatedAt = s.now()
	list := s.sessions[p.SessionID]
	if i := slices.IndexFunc(list, func(q *Preset) bool {
		return strings.EqualFold(q.ExpectedInput, p.ExpectedInput)
	}); i >= 0 {
		list = slices.Delete(list, i, i+1)
		c.Replaced++
	}
	for len(list) >= s.max {
		list = list[1:] // lowest Seq
		c.Evicted++
	}
	s.sessions[p.SessionID] = append(list, p)
	c.Created++
}

// List returns copies of the live presets for a session, or all sessions when id is "".
func (s *Store) List(sessionID string) []Preset {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Preset
	for sid, list := range s.sessions {
		if sessionID != "" && sid != sessionID {
			continue
		}
		for _, p := range list {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b Preset) int { return int(a.Seq) - int(b.Seq) })
	return out
}

// Len returns the number of live presets in a session.
func (s *Store) Len(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions[sessionID])
}

// Take removes and returns a preset (used when it fires).
func (s *Store) Take(id string) (Preset, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sid, list := range s.sessions {
		if i := slices.IndexFunc(list, func(p *Preset) bool { return p.ID == id }); i >= 0 {
			p := *list[i]
			s.setLocked(sid, slices.Delete(list, i, i+1))
			return p, true
		}
	}
	return Preset{}, false
}

func (s *Store) Delete(id string) bool {
	_, ok := s.Take(id)
	return ok
}

func (s *Store) DeleteSession(sessionID string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.sessions[sessionID])
	delete(s.sessions, sessionID)
	return n
}

// UserTurn decrements TurnsLeft for every preset in the session and drops expired ones.
// Call it once per real user text turn, after matching.
func (s *Store) UserTurn(sessionID string) Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	var c Counts
	list := s.sessions[sessionID]
	kept := list[:0]
	for _, p := range list {
		p.TurnsLeft--
		if p.TurnsLeft <= 0 {
			c.Expired++
			continue
		}
		kept = append(kept, p)
	}
	s.setLocked(sessionID, kept)
	return c
}

// Invalidate drops every preset in the session. It is called when the model makes a
// file-modifying tool call; since presets are only written at the end of a text answer,
// every live preset predates that call.
func (s *Store) Invalidate(sessionID string) Counts {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := Counts{Invalidated: len(s.sessions[sessionID])}
	delete(s.sessions, sessionID)
	return c
}

func (s *Store) setLocked(sessionID string, list []*Preset) {
	if len(list) == 0 {
		delete(s.sessions, sessionID)
		return
	}
	s.sessions[sessionID] = list
}

func newID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "pre_" + hex.EncodeToString(b[:])
}
