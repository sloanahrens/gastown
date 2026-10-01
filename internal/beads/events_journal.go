package beads

import (
	"fmt"
	"strconv"
	"strings"
)

// EventsJournalKey is the config.yaml key that turns bd's events journal on.
// The convoy manager polls the journal (gt-7iwy0.2), so every store gastown
// creates or un-parks turns it on and gt doctor fails a store that leaves it
// off (gt-7iwy0.7).
const EventsJournalKey = "events-journal"

// EventsJournalOn reads an events-journal value as bd's config does: a boolean.
func EventsJournalOn(v string) bool {
	on, err := strconv.ParseBool(strings.TrimSpace(v))
	return err == nil && on
}

// EventsJournalProbeEnv is env pinned to beadsDir with any inherited
// BD_EVENTS_JOURNAL removed, so bd answers events-journal from the store's
// config rather than from the caller's environment.
func EventsJournalProbeEnv(env []string, beadsDir string) []string {
	env = StripEnvKey(StripEnvKey(env, "BD_EVENTS_JOURNAL"), "BEADS_DIR")
	return append(env, "BEADS_DIR="+beadsDir)
}

// journalConfig is the bd config surface EnsureEventsJournal needs.
type journalConfig interface {
	ConfigGet(key string) (string, error)
	ConfigSet(key, value string) error
}

// EnsureEventsJournal turns the events journal on in the store bd reaches,
// writing config.yaml only when it is off. It reports whether it wrote.
func EnsureEventsJournal(bd journalConfig) (bool, error) {
	v, err := bd.ConfigGet(EventsJournalKey)
	if err != nil {
		return false, fmt.Errorf("bd config get %s: %w", EventsJournalKey, err)
	}
	if EventsJournalOn(v) {
		return false, nil
	}
	if err := bd.ConfigSet(EventsJournalKey, "true"); err != nil {
		return false, fmt.Errorf("bd config set %s true: %w", EventsJournalKey, err)
	}
	return true, nil
}
