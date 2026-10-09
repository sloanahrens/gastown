// Package attention is the deterministic core of the attention queue: the
// item type, its kinds, the pure reconcile that keeps first_seen across ticks,
// and the file store the daemon and gt attention share.
//
// The queue is derived, not event-sourced. Every heartbeat each collector
// returns the items whose condition holds now; Reconcile(prev, observed,
// acks, now) keeps the first_seen of items it already knew, drops the ones no
// collector returned, and emits a new or cleared transition for each change.
// The daemon keeps only first_seen, last_seen and acks, so a condition that
// clears needs no code path to clear it.
package attention

import "time"

// Kind is what an item is about. Each is the class written to
// events.jsonl and the classifier a reader groups by.
type Kind string

// The item kinds. A slice may produce only some of them; the set is fixed
// here so a reader can name any of them.
const (
	// KindRiskPath: a landing touched a path on the risk list
	// (internal/land/riskpaths.txt) and wants a post-landing review.
	KindRiskPath Kind = "risk-path"
	// KindRedMain: the tier sweep found main red.
	KindRedMain Kind = "red-main"
	// KindRejectedTwice: a bead was rejected by landing twice.
	KindRejectedTwice Kind = "rejected-twice"
	// KindEscalation: an open escalation awaits the overseer.
	KindEscalation Kind = "escalation"
	// KindPolecatStall: a polecat has not reported progress for its bound.
	KindPolecatStall Kind = "polecat-stall"
	// KindLandingStuck: a landing has been in flight too long.
	KindLandingStuck Kind = "landing-stuck"
	// KindQueueStuck: the landing queue has been silent too long.
	KindQueueStuck Kind = "queue-stuck"
	// KindDirectPush: a commit reached main outside the landing worker.
	KindDirectPush Kind = "direct-push"
	// KindSlotHeld: a build slot has been held past its bound.
	KindSlotHeld Kind = "slot-held"
	// KindSlotDeadHolder: a build slot is held by a dead process.
	KindSlotDeadHolder Kind = "slot-dead-holder"
	// KindBDSlow: a bd call ran slower than the threshold.
	KindBDSlow Kind = "bd-slow"
	// KindTierSweepRed: the hourly tier sweep failed.
	KindTierSweepRed Kind = "tier-sweep-red"
	// KindRevertRefused: gt done refused a branch as a revert.
	KindRevertRefused Kind = "revert-refused"
	// KindBlockedMail: a polecat reported itself BLOCKED by mail.
	KindBlockedMail Kind = "blocked-mail"
)

// Kinds is every kind an item may carry, in one place for tests and readers
// that must know the full set.
func Kinds() []Kind {
	return []Kind{
		KindRiskPath, KindRedMain, KindRejectedTwice, KindEscalation,
		KindPolecatStall, KindLandingStuck, KindQueueStuck, KindDirectPush,
		KindSlotHeld, KindSlotDeadHolder, KindBDSlow, KindTierSweepRed,
		KindRevertRefused, KindBlockedMail,
	}
}

// Severity is how loud an item is. The values match the alerts.jsonl schema
// gt tail's watch source reads: low or high.
type Severity string

// Severities, lowest first.
const (
	SeverityLow  Severity = "low"
	SeverityHigh Severity = "high"
)

// Item is one condition that holds now. Key is the item's stable identity:
// the same condition re-observed keeps its key, and an ack is found by it.
type Item struct {
	// Key identifies the condition, for example red-main:gastown:internal/cmd,
	// risk:gt-abc:6cf8b456abcd, esc:hq-123 or stall:gastown/opal.
	Key string `json:"key"`
	// Kind is what the item is about.
	Kind Kind `json:"kind"`
	// Severity is how loud it is.
	Severity Severity `json:"severity"`
	// Rig names the rig the condition is in, when it has one.
	Rig string `json:"rig,omitempty"`
	// Bead is the bead the condition names, when it has one.
	Bead string `json:"bead,omitempty"`
	// SHA is the commit the condition names, when it has one.
	SHA string `json:"sha,omitempty"`
	// Summary is the one-line human description shown in the queue.
	Summary string `json:"summary"`
	// FirstSeen is when the condition was first observed. It survives every
	// re-observation and is what AGE counts from.
	FirstSeen time.Time `json:"first_seen"`
	// LastSeen is when the condition was last observed.
	LastSeen time.Time `json:"last_seen"`
	// AckedAt is set when a human acknowledged the item. An ack hides the
	// item until it clears; the item itself stays in the state.
	AckedAt *time.Time `json:"acked_at,omitempty"`
}

// State is state.json: the current set, rewritten atomically each tick.
type State struct {
	// Updated is the tick time the set was written at. A reader treats a
	// state older than StaleAfter as one the daemon stopped writing.
	Updated time.Time `json:"updated"`
	// Items is the current set, ordered by key.
	Items []Item `json:"items"`
}

// Ack records that one item's key was acknowledged at At.
type Ack struct {
	Key string    `json:"key"`
	At  time.Time `json:"at"`
}

// Acks is acks.json: the acknowledged keys. gt attention adds to it under a
// flock; the daemon prunes it back to the keys its current state holds
// (PruneAcks), so an ack dies with its item.
type Acks struct {
	Acks []Ack `json:"acks"`
}

// EventState is whether a transition added or removed an item.
type EventState string

// Transition states.
const (
	// EventNew: the item was observed for the first time.
	EventNew EventState = "new"
	// EventCleared: the item stopped being observed and was dropped.
	EventCleared EventState = "cleared"
)

// Event is one line of events.jsonl. The first four fields (ts, class,
// severity, text) are exactly the gt-z2pdg alerts.jsonl schema, so gt tail's
// watch source reads this file unchanged; key and state are additional.
type Event struct {
	// TS is when the transition happened.
	TS time.Time `json:"ts"`
	// Class is the item's kind.
	Class Kind `json:"class"`
	// Severity is the item's severity.
	Severity Severity `json:"severity"`
	// Text is the item's summary.
	Text string `json:"text"`
	// Key is the item's stable identity.
	Key string `json:"key"`
	// State is new or cleared.
	State EventState `json:"state"`
}

// StaleAfter is how old state.json may be before a reader stops trusting it:
// the daemon writes it every heartbeat, so this much silence means it is not
// writing. It is compiled in, not a config key.
const StaleAfter = 15 * time.Minute

// Stale reports whether s is old enough that the daemon has stopped writing
// it. A state with no tick time (the daemon has never written one) is not
// stale; it is simply empty.
func Stale(s State, now time.Time, staleAfter time.Duration) bool {
	return !s.Updated.IsZero() && now.Sub(s.Updated) >= staleAfter
}

// Find returns the item with key and whether it is in the set.
func Find(s State, key string) (Item, bool) {
	for _, it := range s.Items {
		if it.Key == key {
			return it, true
		}
	}
	return Item{}, false
}

// AckedKey reports whether key has an ack in acks.
func AckedKey(acks Acks, key string) bool {
	for _, a := range acks.Acks {
		if a.Key == key {
			return true
		}
	}
	return false
}
