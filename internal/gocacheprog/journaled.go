package gocacheprog

import "fmt"

// Journaler records the accesses a Journaled store makes; *Journal satisfies it.
type Journaler interface {
	Put(actionID, outputID []byte, size int64) error
	Use(actionID []byte) error
}

// Journaled is a Store that writes each access's record before the access.
// Eviction moves files aside and then re-reads the journals, so a record
// written before an access is always seen; one written after could be missed.
type Journaled struct {
	store   Store
	journal Journaler
}

// NewJournaled returns a Store that answers from store and records into journal.
func NewJournaled(store Store, journal Journaler) *Journaled {
	return &Journaled{store: store, journal: journal}
}

// Get records a use of actionID, then reads the entry. A use that cannot be
// recorded is a miss carrying that error, and the store is not read.
func (s *Journaled) Get(actionID []byte) (Entry, error) {
	if err := s.journal.Use(actionID); err != nil {
		return Entry{}, fmt.Errorf("%w: journal use: %w", ErrMiss, err)
	}
	return s.store.Get(actionID)
}

// Put records the body's size under actionID and outputID, then writes the
// body. A put record that cannot be written fails the Put and writes no file.
func (s *Journaled) Put(actionID, outputID, body []byte) (string, error) {
	if err := s.journal.Put(actionID, outputID, int64(len(body))); err != nil {
		return "", fmt.Errorf("gocacheprog: journaled put: journal: %w", err)
	}
	return s.store.Put(actionID, outputID, body)
}
