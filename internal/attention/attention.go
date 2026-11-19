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
	// KindCacheTrimNoOp: a shared cache trim completed and its claim still
	// holds more than twice the cap, so the Job may have run the wrong
	// helper (gt-gg9hv.46).
	KindCacheTrimNoOp Kind = "cache-trim-noop"
	// KindRejectedTwice: a bead was rejected by landing twice.
	KindRejectedTwice Kind = "rejected-twice"
	// KindReviewRejected: om refused a bead's review, so the bead's owner
	// bird has findings to act on (gt-r3vmgc).
	KindReviewRejected Kind = "review-rejected"
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
	// KindTierSweepStarved: a tier the schedule names has not been judged for
	// several cycles in a row, so its last verdict is stale (gt-r2e0h).
	KindTierSweepStarved Kind = "tier-sweep-starved"
	// KindTierSweepStuck: the tier sweep's single-flight guard has held a cycle
	// past its bound, so every later trigger starts nothing (gt-gg9hv.37).
	KindTierSweepStuck Kind = "tier-sweep-stuck"
	// KindTierSweepGuardUnreadable: a tier sweep's guard read failed, so the
	// cycle deferred without a verdict until a read answers (gt-gg9hv.56).
	KindTierSweepGuardUnreadable Kind = "tier-sweep-guard-unreadable"
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
	// KindHostNodeDisk: the control node's data disk, the image filesystem
	// that holds the container runtime's store and the local-path claims, is
	// filling (gt-x1po96).
	KindHostNodeDisk Kind = "host-node-disk"
	// KindHostMem: the host's free memory is low.
	KindHostMem Kind = "host-mem"
	// KindHostForgejo: the town's Forgejo has not answered its health endpoint
	// for several heartbeats in a row (gt-gg9hv.36).
	KindHostForgejo Kind = "host-forgejo"
	// KindHostProbe: a host probe (disk, memory, docker or Forgejo health)
	// failed, so its rule could not be judged this tick.
	KindHostProbe Kind = "host-probe"
	// KindQuestionNew: an overseer question has been filed and nobody has
	// commented on it yet.
	KindQuestionNew Kind = "question-new"
	// KindQuestionAnswered: the operator answered an overseer question and the
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
	// KindBirdReportStale: an expected reporter's daily report is missing or
	// older than the patrol's maximum age (gt-cbsat).
	KindBirdReportStale Kind = "bird-report-stale"
	// KindPostLandUnknown: a landed commit's post-land status context never
	// reached a verdict before its timeout, or the store that keeps those
	// pending verdicts cannot be read or written, so the post-land safety net
	// is silent (gt-gg9hv.7.1).
	KindPostLandUnknown Kind = "post-land-unknown"
	// KindGreenRef: the sweep could not publish a fully green commit's
	// green/<sha12> ref for the image build, or the remote holds that ref at
	// another commit (gt-gg9hv.61).
	KindGreenRef Kind = "green-ref"
	// KindDoltImageBehind: the town's Dolt container runs a dolt-sql-server
	// image more than one patch release behind the newest stable release on
	// Docker Hub (gt-goda5m).
	KindDoltImageBehind Kind = "dolt-image-behind"
	// KindDeployPairReady: the images workflow published a daemon and polecat
	// image for a green commit newer than the one the running daemon was
	// rendered from, so a roll can start (gt-4nlpw8).
	KindDeployPairReady Kind = "deploy-pair-ready"
	// KindDeployPairUnreadable: the pair check could not read the running
	// commit, a rig's green branches or its images, so it judged nothing
	// (gt-4nlpw8).
	KindDeployPairUnreadable Kind = "deploy-pair-unreadable"
	// KindDeployPairStuck: a green ref newer than the running daemon commit is
	// past the stuck bound with its daemon and polecat images not both
	// published, whether or not an images run exists, so no roll can start
	// from it (gt-afwjb4).
	KindDeployPairStuck Kind = "deploy-pair-stuck"
	// KindDeployPairRefused: the update ledger records a green commit's pair as
	// refused by the pre-swap check, so no roll can start from that commit and
	// nothing retries it (gt-d6ok4r, gt-t8vf15).
	KindDeployPairRefused Kind = "deploy-pair-refused"
	// KindDeployPairSkew: the bd commit a green commit's deploy image pins
	// carries a schema level the live database does not, so the daemon the roll
	// starts would be refused at its startup handshake (gt-qnjuqp).
	KindDeployPairSkew Kind = "deploy-pair-skew"
	// KindImagesFailed: the images workflow's newest run for a green commit
	// finished failed or canceled, so that commit's daemon and polecat images
	// were never published and no roll can start from it (gt-3vafwr).
	KindImagesFailed Kind = "images-failed"
	// KindImagesUnknown: the images workflow's run list for a green commit whose
	// pair is missing could not be read, so whether its build failed is unknown
	// (gt-3vafwr). It is not a clean commit, and the next readable list clears it.
	KindImagesUnknown Kind = "images-unknown"
	// KindTierSweepImagesRetryExhausted: the sweep spent every early re-run on a
	// green commit whose images build kept failing, so it stopped retrying and
	// that commit still has no daemon and polecat images to roll onto
	// (gt-j6cwqr).
	KindTierSweepImagesRetryExhausted Kind = "tier-sweep-images-retry-exhausted"
	// KindConfigEnvShadowed: a settings daemon.env key was not applied at
	// start, because the container's environment already sets it to another
	// value (gt-ih19g6).
	KindConfigEnvShadowed Kind = "config-env-shadowed"
	// KindConfigParseFailed: the patrol config file does not load on a
	// heartbeat reload, so the daemon runs on the config it had before the
	// edit (gt-6frjqf).
	KindConfigParseFailed Kind = "config-parse-failed"
	// KindSandboxSeatKept: the daemon has kept a sandbox seat for a reason other
	// than the jev_rules ask for longer than the window, so the seat holds its
	// pool slot and worktree while the reason stands (gt-l38v7r).
	KindSandboxSeatKept Kind = "sandbox-seat-kept"
	// KindUpdateRolledBack: the update's last run put the daemon back on its
	// previous image after a check failed, so the daemon runs the pair from before
	// the roll (gt-9odoqa). It clears when the next run starts.
	KindUpdateRolledBack Kind = "update-rolled-back"
	// KindHoldStale: a bead wears a hold label (overseer-only, needs-overseer or
	// held) and nobody has released it: over one day with every dependency
	// closed, or over three days whatever its dependencies (gt-j669b4.19).
	KindHoldStale Kind = "hold-stale"
	// KindJobOverdue: a maintenance job is past its cadence or its newest run
	// failed, so the work it does has not happened (gt-toiff6).
	KindJobOverdue Kind = "job-overdue"
	// KindMetricsExtract: the metrics extract a digest or analysis reads is
	// stale, gapped, or could not be read at all, so the tables it publishes
	// no longer describe the town (gt-mi4cem).
	KindMetricsExtract Kind = "metrics-extract"
	// KindSandboxPending: a sandbox pod has stayed Pending and unschedulable
	// past the window, so no agent runs in it and the scheduler's own message
	// names what the pod cannot get (gt-1gtq5g).
	KindSandboxPending Kind = "sandbox-pending"
	// KindSandboxSilent: a running sandbox seat has written no transcript for
	// the window, so its agent may be stuck at a prompt with nothing in the
	// queue saying so until the overrun bound (gt-oq4ele).
	KindSandboxSilent Kind = "sandbox-silent"
	// KindSeatSandboxStale: a seat's gateway sandbox sits in a terminal
	// failure phase, so its seat name fails every sling drawn for it until the
	// sandbox is deleted (gt-zapy97).
	KindSeatSandboxStale Kind = "seat-sandbox-stale"
	// KindSeatUnreachable: a seat's session has not answered the daemon's
	// session check for five minutes, so nobody may be running the bead the
	// seat holds (gt-q6hafz).
	KindSeatUnreachable Kind = "seat-unreachable"
	// KindStoryPointsDrift: the story point estimator's estimate no longer ranks
	// landings the way the measured polecat time does, or the drift measure
	// cannot be read (gt-my0o23).
	KindStoryPointsDrift Kind = "storypoints-drift"
	// KindImagePinsStale: an image pin trails its source's origin/main by the
	// patrol's stale bound, so the refresh the patrol files at the file bound
	// has not landed (gt-01lcq2).
	KindImagePinsStale Kind = "image-pins-stale"
	// KindImagePinsUnreadable: a pin's read failed, or the patrol's whole tick
	// did, so no pin's distance from its tip is known (gt-01lcq2).
	KindImagePinsUnreadable Kind = "image-pins-unreadable"
	// KindImagePinsRefreshStuck: the refresh bead the patrol filed has sat
	// unlanded past its bound, so the pins wait for it (gt-01lcq2).
	KindImagePinsRefreshStuck Kind = "image-pins-refresh-stuck"
	// KindImagePinsDeadLayer: a layer of a digest-pinned image is no longer
	// served by its registry, so the pin names an image the registry can no
	// longer hand out (gt-kyomsc).
	KindImagePinsDeadLayer Kind = "image-pins-dead-layer"
	// KindGateSlow: the newest gate runs of a rig were all over their tier bar,
	// or all pole-bound (gt-l0t2tg).
	KindGateSlow Kind = "gate-slow"
)

// Kinds is every kind an item may carry, in one place for tests and readers
// that must know the full set.
func Kinds() []Kind {
	return []Kind{
		KindRiskPath, KindRedMain, KindCacheTrimNoOp, KindRejectedTwice, KindReviewRejected, KindEscalation,
		KindPolecatStall, KindLandingStuck, KindQueueStuck, KindDirectPush,
		KindSlotHeld, KindSlotDeadHolder, KindBDSlow, KindTierSweepRed,
		KindTierSweepStarved, KindTierSweepStuck, KindTierSweepGuardUnreadable,
		KindRevertRefused, KindBlockedMail, KindOverrunStopped, KindScheduledGate,
		KindHostDisk, KindHostNodeDisk, KindHostMem, KindHostForgejo, KindHostProbe,
		KindQuestionNew, KindQuestionAnswered, KindQuestionStale,
		KindOverseerSilent, KindDispatchIdle, KindDeployFailed, KindReviewBacklog,
		KindBirdReportStale, KindPostLandUnknown, KindGreenRef, KindDoltImageBehind,
		KindDeployPairReady, KindDeployPairUnreadable, KindDeployPairStuck,
		KindDeployPairRefused, KindDeployPairSkew, KindImagesFailed, KindImagesUnknown,
		KindTierSweepImagesRetryExhausted,
		KindConfigEnvShadowed, KindConfigParseFailed, KindSandboxSeatKept,
		KindHoldStale, KindJobOverdue, KindMetricsExtract, KindSandboxPending, KindSandboxSilent, KindSeatSandboxStale, KindSeatUnreachable,
		KindUpdateRolledBack, KindStoryPointsDrift,
		KindImagePinsStale, KindImagePinsUnreadable, KindImagePinsRefreshStuck,
		KindImagePinsDeadLayer, KindGateSlow,
	}
}

// Severity is how loud an item is. The values match the alerts.jsonl schema
// gt tail's watch source reads: low, medium or high.
type Severity string

// Severities, lowest first.
const (
	SeverityLow    Severity = "low"
	SeverityMedium Severity = "medium"
	SeverityHigh   Severity = "high"
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
