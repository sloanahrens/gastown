package attention

import "strings"

// Owner is who has to act on an item: the human, or an agent that can resolve
// it without one. The dashboard tags every row with it and the push channel
// reads it, so one rule decides both what a reader sees and what wakes the
// human (gt-71714).
type Owner string

// The owners.
const (
	// OwnerSloan is the human: the item waits on a decision or an action only
	// they can take.
	OwnerSloan Owner = "sloan"
	// OwnerOverseer is an agent: it triages the item itself, and the human
	// hears about it only if it goes unhandled.
	OwnerOverseer Owner = "overseer"
)

// HumanAddress is the address the human's own session escalates from. The mail
// router resolves @overseer to the person (internal/mail/router.go), and an
// escalation filed from the town root, with no GT_ROLE and no GT_POLECAT, is
// sent as "overseer" too (internal/cmd/mail_identity.go).
const HumanAddress = "overseer"

// ownerFamily is one row of the owner table: the rule ids it covers, by prefix,
// and their owner.
type ownerFamily struct {
	prefix string
	owner  Owner
}

// ownerFamilies is the owner table, most specific rule first. A rule no row
// covers is the overseer's: it triages what a machine raised, and the human is
// woken by the push rules rather than by every open row.
var ownerFamilies = []ownerFamily{
	// A question is filed for the human to answer. question.answered is not
	// here: Sloan has answered it, and the overseer owes the next move.
	{RuleQuestionNew, OwnerSloan},
	{RuleQuestionStale, OwnerSloan},
	// The physical machine: no agent frees disk or memory.
	{"host.", OwnerSloan},
	// A silent overseer cannot wake itself; the daemon pushes to the human
	// instead (gt-3a6h6).
	{RuleOverseerSilent, OwnerSloan},
	// A bead refused twice is a call the landing loop cannot make.
	{RuleRejectedTwice, OwnerSloan},
	// An escalation is the human's only when the overseer cannot resolve it,
	// and only the escalation says whether that is so. EscalationOwner reads
	// that off it; an escalation.open row carries none of it, so the family
	// default is the overseer's and the row is the one owner that has to be
	// corrected per item rather than read off the table.
	{"escalation.", OwnerOverseer},
}

// RuleOwner returns the owner of rule. An unknown rule is the overseer's, as a
// line written before the owner table existed reads.
func RuleOwner(rule string) Owner {
	for _, f := range ownerFamilies {
		if strings.HasPrefix(rule, f.prefix) {
			return f.owner
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
		return OwnerSloan
	}
	return OwnerOverseer
}

// IsHumanAddress reports whether an agent address names the human's own
// session, the one session that speaks for the person.
func IsHumanAddress(address string) bool {
	address = strings.TrimSpace(address)
	return address == HumanAddress || strings.HasSuffix(address, "/"+HumanAddress)
}
