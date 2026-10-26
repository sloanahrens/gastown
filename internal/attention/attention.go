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
	// KindOverrunStopped: the overrun guard stopped a polecat at the
	// 20-minute rule and handed its bead back to be split.
	KindOverrunStopped Kind = "overrun-stopped"
	// KindScheduledGate: a scheduled sling's gate_command failed or timed out
	// three ticks in a row, so the schedule is not running.
	KindScheduledGate Kind = "scheduled-gate"
	// KindHostDisk: the volume holding the town root is filling.
	KindHostDisk Kind = "host-disk"
	// KindHostMem: the host's free memory is low.
	KindHostMem Kind = "host-mem"
	// KindHostDocker: a container of the Forgejo or hm stack is not running.
	KindHostDocker Kind = "host-docker"
	// KindHostProbe: a host probe (disk, memory or docker) failed, so its rule
	// could not be judged this tick.
	KindHostProbe Kind = "host-probe"
	// KindQuestionNew: an overseer question has been filed and nobody has
	// commented on it yet.
	KindQuestionNew Kind = "question-new"
	// KindQuestionAnswered: Sloan answered an overseer question and the
	// overseer has not yet commented on the answer.
	KindQuestionAnswered Kind = "question-answered"
	// KindQuestionStale: an overseer question has been open more than a day.
	KindQuestionStale Kind = "question-stale"
	// KindOverseerSilent: the overseer's heartbeat is older than its window
	// while dispatchable work or an unanswered question waits.
	KindOverseerSilent Kind = "overseer-silent"
	// KindDispatchIdle: the dispatcher has a free seat and dispatchable work
	// and has started nothing for several ticks.
	KindDispatchIdle Kind = "dispatch-idle"
	// KindDeployFailed: a deploy workflow's newest run on an app rig's main or
	// a v* tag failed, so a landing or a release did not ship (gt-hcbft).
	KindDeployFailed Kind = "deploy-failed"
	// KindReviewBacklog: the open risk-path queue passed its threshold or rose
	// fast, so unreviewed landings are piling up faster than they clear.
	KindReviewBacklog Kind = "review-backlog"
)

// Kinds is every kind an item may carry, in one place for tests and readers
// that must know the full set.
func Kinds() []Kind {
	return []Kind{
		KindRiskPath, KindRedMain, KindRejectedTwice, KindEscalation,
		KindPolecatStall, KindLandingStuck, KindQueueStuck, KindDirectPush,
		KindSlotHeld, KindSlotDeadHolder, KindBDSlow, KindTierSweepRed,
		KindRevertRefused, KindBlockedMail, KindOverrunStopped, KindScheduledGate,
		KindHostDisk, KindHostMem, KindHostDocker, KindHostProbe,
		KindQuestionNew, KindQuestionAnswered, KindQuestionStale,
		KindOverseerSilent, KindDispatchIdle, KindDeployFailed, KindReviewBacklog,
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
	// Owner names who holds the condition now, when a person or seat does.
	Owner string `json:"owner,omitempty"`
	// WaitingOn says what the condition is waiting on, in words.
	WaitingOn string `json:"waiting_on,omitempty"`
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
	// LandedAt is when the landing the item is about landed, for the items
	// that have one. The risk-path promotion ages the item from it, so a
	// daemon restart does not restart the clock.
	LandedAt time.Time `json:"landed_at,omitzero"`
	// PromotedAt is when the item was promoted to operator_action. It is in
	// the state so a restart does not promote the same item twice.
	PromotedAt *time.Time `json:"promoted_at,omitempty"`
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
	// Rule is the stable id of the rule that raised the item, such as
	// landing.stuck_inflight. A line written before the rule existed has none.
	Rule string `json:"rule,omitempty"`
	// Handled says who handles the transition (self_healed, operator_action or
	// info). Empty reads as info.
	Handled Handled `json:"handled,omitempty"`
	// Wake is whether the transition should wake the overseer or the human.
	// Absent reads as false.
	Wake bool `json:"wake,omitempty"`
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

// Class says whether an item needs the overseer to act, or is only work in the
// pipeline that clears by itself when someone else's landing happens.
type Class string

const (
	// ClassAct is an item that needs the overseer or a person.
	ClassAct Class = "act"
	// ClassPipeline is an item that is only waiting on work already in flight.
	ClassPipeline Class = "pipeline"
)

// Classify derives an item's class from its kind and its owner and waiting-on
// fields. Rejected-twice, tier-sweep-red and risk-path items are pipeline when
// someone owns them or they wait on something; review-backlog is always
// pipeline. Every other kind, an unknown kind included, is act, so an unknown
// condition is seen rather than hidden (gt-x3qra).
func Classify(it Item) Class {
	switch it.Kind {
	case KindRejectedTwice, KindTierSweepRed, KindRiskPath:
		if it.Owner != "" || it.WaitingOn != "" {
			return ClassPipeline
		}
		return ClassAct
	case KindReviewBacklog:
		return ClassPipeline
	}
	return ClassAct
}
