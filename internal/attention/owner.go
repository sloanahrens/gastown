package attention

import "strings"

// Owner is who has to act on an item: the human, or an agent that can resolve
// it without one. The dashboard tags every row with it and the push channel
// reads it, so one rule decides both what a reader sees and what wakes the
// human (gt-71714).
type Owner string

// The owners.
const (
	// OwnerOperator is the human — the operator: the item waits on a decision or
	// an action only they can take.
	OwnerOperator Owner = "operator"
	// OwnerOverseer is an agent: it triages the item itself, and the human
	// hears about it only if it goes unhandled.
	OwnerOverseer Owner = "overseer"
)

// HumanAddress is the address the human's own session escalates from. The mail
// router resolves @overseer to the person (internal/mail/router.go), and an
// escalation filed from the town root, with no GT_ROLE and no GT_POLECAT, is
// sent as "overseer" too (internal/cmd/mail_identity.go).
const HumanAddress = "overseer"

// ruleOwners is the owner table: one row per rule in rules.go, and the one
// place that decides whether an item waits on the human or on the overseer.
// Every rule has a row — TestEveryRuleHasAnOwner fails for a rule added to the
// flags table without one — so the fallback below answers only for a rule this
// binary has never seen, one a newer daemon wrote.
//
// The three rules a row cannot decide alone are noted at their rows:
// escalation.open reads the escalation (EscalationOwner), and the two question
// states belong to whoever owes the next move, which the row states.
var ruleOwners = map[string]Owner{
	// A question is filed for the human to answer. question.answered is the
	// overseer's: the human has answered it, and the overseer owes the next
	// move, so handing it back to the human would be a row nobody can clear.
	RuleQuestionNew:      OwnerOperator,
	RuleQuestionStale:    OwnerOperator,
	RuleQuestionAnswered: OwnerOverseer,
	// The physical machine: no agent frees disk or memory.
	RuleHostDiskWarn:   OwnerOperator,
	RuleHostDiskCrit:   OwnerOperator,
	RuleHostMemLow:     OwnerOperator,
	RuleHostDockerDown: OwnerOperator,
	RuleHostProbe:      OwnerOperator,
	// A silent overseer cannot wake itself; the daemon pushes to the human
	// instead (gt-3a6h6).
	RuleOverseerSilent: OwnerOperator,
	// Rework through the second rejection is routine; the overseer decides what
	// a bead that keeps failing becomes (a rework brief, a waiver, a split). An
	// escalation the third rejection raises that the overseer cannot settle
	// reaches the human through EscalationOwner (gt-few2w).
	RuleRejectedTwice: OwnerOverseer,
	// An escalation is the human's only when the overseer cannot resolve it, and
	// only the escalation says whether that is so. EscalationOwner reads that
	// off it; this row is the family default, for the escalation.open item that
	// carries no raiser, and is the one owner a row corrects per item rather
	// than reads off the table.
	RuleEscalation: OwnerOverseer,
	// Everything below is the town looking after itself: a landing, a polecat,
	// a schedule or the host's own dispatch, which the overseer triages first.
	RuleRiskPath:       OwnerOverseer,
	RuleRedMain:        OwnerOverseer,
	RulePolecatStall:   OwnerOverseer,
	RuleLandingStuck:   OwnerOverseer,
	RuleQueueStuck:     OwnerOverseer,
	RuleDirectPush:     OwnerOverseer,
	RuleSlotHeld:       OwnerOverseer,
	RuleSlotDeadHolder: OwnerOverseer,
	RuleBDSlow:         OwnerOverseer,
	RuleTierSweepRed:   OwnerOverseer,
	RuleRevertRefused:  OwnerOverseer,
	RuleBlockedMail:    OwnerOverseer,
	RuleOverrunStopped: OwnerOverseer,
	RuleScheduledGate:  OwnerOverseer,
	RuleDispatchIdle:   OwnerOverseer,
	// A deploy that did not ship is work, not a decision: the overseer re-runs
	// it or finds what stopped it, and the operator hears about it only if it
	// stays failed (gt-hcbft).
	RuleDeployFailed: OwnerOverseer,
	// A review queue is the overseer's to work down.
	RuleReviewBacklog: OwnerOverseer,
}

// humanFamilies are the rule families the table gives the human as a whole:
// host readings and escalations. They answer for a rule the table has no row
// for, so a host or escalation rule a newer daemon writes reaches the human's
// panel rather than the overseer's. escalation.open is the one row that
// overrides its own family, because only the escalation says who raised it
// (EscalationOwner).
var humanFamilies = []string{"host.", "escalation."}

// RuleOwner returns the owner of rule. A rule the table has no row for reads by
// its family: a host reading or an escalation is the human's, and everything
// else is the overseer's, as a line written before the owner table existed
// reads. A newer daemon's host rule that fell to the overseer would be a
// reading about the machine nothing wakes the person for, which is why the
// families are not all read the same way.
func RuleOwner(rule string) Owner {
	if o, ok := ruleOwners[rule]; ok {
		return o
	}
	for _, family := range humanFamilies {
		if strings.HasPrefix(rule, family) {
			return OwnerOperator
		}
	}
	return OwnerOverseer
}

// EscalationOwner returns the owner of an escalation raised by the address
// escalatedBy: the human's when the human's own session raised it, since only
// the human decides what reaches the human, and the overseer's when a polecat,
// a crew session or the daemon raised it, which the overseer triages first
// (gt-71714). An escalation left open past the unhandled window reaches the
// human anyway.
func EscalationOwner(escalatedBy string) Owner {
	if IsHumanAddress(escalatedBy) {
		return OwnerOperator
	}
	return OwnerOverseer
}

// IsHumanAddress reports whether an agent address names the human's own
// session, the one session that speaks for the person.
func IsHumanAddress(address string) bool {
	address = strings.TrimSpace(address)
	return address == HumanAddress || strings.HasSuffix(address, "/"+HumanAddress)
}
