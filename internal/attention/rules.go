package attention

import (
	"sort"
	"time"
)

// Handled says who deals with a transition, so a reader can tell a line that
// wants a decision from one that is only a record.
type Handled string

// The handled values.
const (
	// HandledSelfHealed: the daemon already repaired the condition itself in
	// the heartbeat that raised it.
	HandledSelfHealed Handled = "self_healed"
	// HandledOperatorAction: only the overseer or the human can resolve it.
	HandledOperatorAction Handled = "operator_action"
	// HandledInfo: nothing to decide; the line is a record. It is also what a
	// line written before these fields existed reads as.
	HandledInfo Handled = "info"
)

// RuleFlags is what a rule raises: who handles it and whether the transition
// wakes the overseer or the human.
type RuleFlags struct {
	Handled Handled
	Wake    bool
}

// Rule ids. They are stable names a reader can filter on, and the keys of the
// flags table; adding a collector means adding its rule here, which
// TestEveryRuleHasFlags enforces.
const (
	RuleRiskPath       = "landing.risk_path"
	RuleRedMain        = "main.red"
	RuleRejectedTwice  = "landing.rejected_twice"
	RuleEscalation     = "escalation.open"
	RulePolecatStall   = "polecat.stall"
	RuleLandingStuck   = "landing.stuck_inflight"
	RuleQueueStuck     = "landing.queue_stuck"
	RuleDirectPush     = "main.direct_push"
	RuleSlotHeld       = "slot.held"
	RuleSlotDeadHolder = "slot.dead_holder"
	RuleBDSlow         = "bd.slow"
	RuleTierSweepRed   = "sweep.tier_red"
	RuleRevertRefused  = "landing.revert_refused"
	RuleBlockedMail    = "polecat.blocked"
	RuleOverrunStopped = "polecat.overrun_stopped"
	RuleScheduledGate  = "schedule.gate_failed"
	RuleHostDiskWarn   = "host.disk_warn"
	RuleHostDiskCrit   = "host.disk_crit"
	RuleHostMemLow     = "host.mem_low"
	RuleHostDockerDown = "host.docker_stack_down"
	RuleHostProbe      = "host.probe_failed"

	// The overseer-question rules (gt-47rea).
	RuleQuestionNew      = "question.new"
	RuleQuestionAnswered = "question.answered"
	RuleQuestionStale    = "question.stale"

	// RuleOverseerSilent is the silent-overseer rule (gt-3a6h6).
	RuleOverseerSilent = "overseer.silent"
)

var (
	flagsWake = RuleFlags{Handled: HandledOperatorAction, Wake: true}
	flagsAsk  = RuleFlags{Handled: HandledOperatorAction}
	flagsInfo = RuleFlags{Handled: HandledInfo}
	flagsSelf = RuleFlags{Handled: HandledSelfHealed}
)

// ruleFlags is the one table of what each rule raises. A raised transition of
// a rule carries its row; a cleared transition is always info and never wakes,
// because the condition has already resolved.
var ruleFlags = map[string]RuleFlags{
	RuleRedMain:        flagsWake,
	RuleRejectedTwice:  flagsWake,
	RuleEscalation:     flagsWake,
	RulePolecatStall:   flagsWake,
	RuleLandingStuck:   flagsWake,
	RuleQueueStuck:     flagsWake,
	RuleDirectPush:     flagsWake,
	RuleSlotHeld:       flagsWake,
	RuleSlotDeadHolder: flagsWake,
	RuleBDSlow:         flagsWake,
	RuleTierSweepRed:   flagsWake,
	RuleBlockedMail:    flagsWake,
	RuleScheduledGate:  flagsWake,
	RuleHostDiskCrit:   flagsWake,
	RuleHostMemLow:     flagsWake,
	RuleHostDockerDown: flagsWake,
	// A question is waiting on Sloan, or Sloan has answered and the overseer
	// has yet to act; both are the overseer's to pick up.
	RuleQuestionNew:      flagsWake,
	RuleQuestionAnswered: flagsWake,
	// A day-old question is a record for the overseer's next look, not a
	// reason to wake it.
	RuleQuestionStale: flagsInfo,
	// A silent overseer cannot be woken by its own queue: the daemon pushes to
	// the person through the configured command instead, so the event itself
	// is a record.
	RuleOverseerSilent: flagsInfo,
	// A refusal is gt done working: the polecat is told and the bead goes
	// back. It needs an operator only if it repeats, which rejected_twice
	// catches.
	RuleRevertRefused: flagsAsk,
	// A post-landing review is queued onto the work bead for the deep review,
	// which writes the review note itself after a green sweep. The rule is
	// info; an item still unreviewed RiskPathPromoteAfter after its landing
	// is promoted to operator_action by RiskPathPromotion.
	RuleRiskPath: flagsInfo,
	// host.disk_warn becomes operator_action after DiskWarnPromoteBeats
	// consecutive heartbeats; the daemon writes that promotion as its own line.
	RuleHostDiskWarn: flagsInfo,
	RuleHostProbe:    flagsInfo,
	// The overrun guard stopped the polecat and handed the bead back itself.
	RuleOverrunStopped: flagsSelf,
}

// DiskWarnPromoteBeats is how many consecutive heartbeats host.disk_warn must
// hold before it becomes an operator_action that wakes.
const DiskWarnPromoteBeats = 3

// RiskPathPromoteAfter is how long after its landing a risk-path item may stay
// unreviewed before it becomes an operator_action that wakes. A red sweep
// gates the deep review off, so without this an unreviewed landing would stay
// info for good.
const RiskPathPromoteAfter = 3 * time.Hour

// Flags returns the flags of rule, and whether the table has a row for it. An
// unknown rule reads as info and no wake, as a line written before the fields
// existed does.
func Flags(rule string) (RuleFlags, bool) {
	f, ok := ruleFlags[rule]
	if !ok {
		return flagsInfo, false
	}
	return f, true
}

// Rules returns every rule id in the flags table, sorted.
func Rules() []string {
	out := make([]string, 0, len(ruleFlags))
	for r := range ruleFlags {
		out = append(out, r)
	}
	sort.Strings(out)
	return out
}

// kindRules maps a kind to its rule id. The disk kind is keyed by the item
// instead (RuleFor), because it raises two rules.
var kindRules = map[Kind]string{
	KindRiskPath:         RuleRiskPath,
	KindRedMain:          RuleRedMain,
	KindRejectedTwice:    RuleRejectedTwice,
	KindEscalation:       RuleEscalation,
	KindPolecatStall:     RulePolecatStall,
	KindLandingStuck:     RuleLandingStuck,
	KindQueueStuck:       RuleQueueStuck,
	KindDirectPush:       RuleDirectPush,
	KindSlotHeld:         RuleSlotHeld,
	KindSlotDeadHolder:   RuleSlotDeadHolder,
	KindBDSlow:           RuleBDSlow,
	KindTierSweepRed:     RuleTierSweepRed,
	KindRevertRefused:    RuleRevertRefused,
	KindBlockedMail:      RuleBlockedMail,
	KindOverrunStopped:   RuleOverrunStopped,
	KindScheduledGate:    RuleScheduledGate,
	KindHostMem:          RuleHostMemLow,
	KindHostDocker:       RuleHostDockerDown,
	KindHostProbe:        RuleHostProbe,
	KindQuestionNew:      RuleQuestionNew,
	KindQuestionAnswered: RuleQuestionAnswered,
	KindQuestionStale:    RuleQuestionStale,
	KindOverseerSilent:   RuleOverseerSilent,
}

// RuleFor is the rule id of an item. The disk collector's key is its rule
// (host.disk_warn or host.disk_crit); every other kind has one rule. An item
// of a kind with no rule yields "".
func RuleFor(it Item) string {
	if it.Kind == KindHostDisk {
		return it.Key
	}
	return kindRules[it.Kind]
}

// EffectiveHandled returns the handled value of e, reading a line written
// before the field existed as info.
func (e Event) EffectiveHandled() Handled {
	if e.Handled == "" {
		return HandledInfo
	}
	return e.Handled
}
