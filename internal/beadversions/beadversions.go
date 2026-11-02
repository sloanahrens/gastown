// Package beadversions records each version of a bead's shaped text (Goal,
// Constraints, Gate, Size and Acceptance as one string) with its hash, actor
// and time, so analysis can compare the text a polecat was given with what the
// bead says later.
//
// The stored text is bead text as filed, never another tenant's text: callers
// must not put any content that is not the bead's own.
//
// The package reads no environment. MemoryStore keeps versions in memory;
// FileStore appends them to one JSONL file and assumes one writer per file,
// taking no lock across processes. The callers are later beads (gt-d7c99).
package beadversions

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"sync"
	"time"
)

var (
	// ErrEmptyBead reports a call with no bead id.
	ErrEmptyBead = errors.New("beadversions: empty bead id")
	// ErrZeroTime reports a Put with a zero time.
	ErrZeroTime = errors.New("beadversions: zero time")
	// ErrOutOfOrder reports a Put whose time is not after the bead's latest
	// version, which the oldest-first ordering and AsOf depend on.
	ErrOutOfOrder = errors.New("beadversions: time not after the latest version")
	// ErrNotFound reports an AsOf time before the bead's first version, or a
	// bead with no versions.
	ErrNotFound = errors.New("beadversions: no version at or before that time")
)

// Version is one recorded state of a bead's text. Text is the shaped bead text
// as filed and Hash is the sha256 hex of Text.
//
// The JSON names are FileStore's on-disk schema, so renaming one changes the
// file for every reader.
type Version struct {
	Bead  string    `json:"bead"`
	Text  string    `json:"text"`
	Hash  string    `json:"hash"`
	Actor string    `json:"actor"`
	At    time.Time `json:"at"`
}

// Store records bead text versions and answers which text was in force at a
// time.
type Store interface {
	// Put records text as the bead's next version when its sha256 differs from
	// the latest recorded hash, and reports whether it recorded. An empty bead
	// id, a zero at, or an at that is not strictly after the bead's latest
	// version are errors, the last as ErrOutOfOrder.
	Put(bead, text, actor string, at time.Time) (recorded bool, err error)
	// Versions returns the bead's versions oldest first as a copy the caller
	// may modify, or an empty slice when the bead has none.
	Versions(bead string) ([]Version, error)
	// AsOf returns the latest version with At at or before t, or ErrNotFound
	// when t is before the first version or the bead has none.
	AsOf(bead string, t time.Time) (Version, error)
}

// MemoryStore is an in-memory Store for callers that need no persistence. Its
// zero value is an empty store, and it is safe for concurrent use.
type MemoryStore struct {
	mu       sync.Mutex
	versions map[string][]Version
}

// NewMemoryStore returns an empty MemoryStore.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{versions: make(map[string][]Version)}
}

// Put implements Store.
func (s *MemoryStore) Put(bead, text, actor string, at time.Time) (bool, error) {
	if bead == "" {
		return false, ErrEmptyBead
	}
	if at.IsZero() {
		return false, ErrZeroTime
	}
	hash := hashText(text)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.versions == nil {
		s.versions = make(map[string][]Version)
	}
	existing := s.versions[bead]
	if len(existing) > 0 {
		latest := existing[len(existing)-1]
		if !at.After(latest.At) {
			return false, ErrOutOfOrder
		}
		if latest.Hash == hash {
			return false, nil
		}
	}
	s.versions[bead] = append(existing, Version{
		Bead:  bead,
		Text:  text,
		Hash:  hash,
		Actor: actor,
		At:    at,
	})
	return true, nil
}

// Versions implements Store.
func (s *MemoryStore) Versions(bead string) ([]Version, error) {
	if bead == "" {
		return nil, ErrEmptyBead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Version(nil), s.versions[bead]...), nil
}

// AsOf implements Store.
func (s *MemoryStore) AsOf(bead string, t time.Time) (Version, error) {
	if bead == "" {
		return Version{}, ErrEmptyBead
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	versions := s.versions[bead]
	i := sort.Search(len(versions), func(i int) bool {
		return versions[i].At.After(t)
	})
	if i == 0 {
		return Version{}, ErrNotFound
	}
	return versions[i-1], nil
}

// hashText returns the sha256 hex of text, the content hash every version
// carries.
func hashText(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
