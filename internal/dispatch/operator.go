package dispatch

import "strings"

// OperatorLabel marks a bead as the human operator's own work: a hand-run
// audit, a config change, a decision only the person in the loop can make. No
// automatic dispatcher may take one.
const OperatorLabel = "operator"

// OperatorReservation reports why a bead's own record reserves it for the human
// operator, or "" when an agent may take it (gt-21pl0).
//
// Two things reserve a bead. The operator label is the operator's explicit
// mark. The other is the assignee: every Gas Town agent is addressed by a
// slash-qualified address (gastown/polecats/onyx, gastown/crew/sloan, mayor/,
// deacon/dogs/boot), so an assignee that is not one is a person — the bare
// handle left behind when the operator takes a bead by hand (`sloan`,
// `Sloan Ahrens`, `overseer`).
//
// The reservation is a record on the bead, so every dispatcher can read it
// without asking who wrote it: a convoy feeder re-slings any open member of a
// convoy, and on 2026-09-25 one re-slung gt-nj23.9 to a fresh polecat two
// minutes after the mayor had un-slung it and assigned it to the operator,
// undoing the reversal (gt-21pl0).
func OperatorReservation(labels []string, assignee string) string {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), OperatorLabel) {
			return "label " + OperatorLabel
		}
	}
	assignee = strings.TrimSpace(assignee)
	if assignee != "" && !isAgentAddress(assignee) {
		return "assignee " + assignee + " is not an agent address"
	}
	return ""
}

// isAgentAddress reports whether an assignee names a Gas Town agent rather than
// a person.
//
// An agent address is slash-qualified: <rig>/polecats/<name>,
// <rig>/crew/<name>, <rig>/witness, <rig>/refinery, deacon/dogs/<name>, and the
// town singletons mayor/ and deacon/. Every agent path that writes an assignee
// writes one of those forms — the address gt sling resolves a target to, and
// the one agentIDToBeadID splits on "/" to find the agent's bead. A bare handle
// parses to no agent bead at all, which is what makes it a person. The bare
// `mayor` and `deacon` spellings count as agents because an older address form
// wrote them without the trailing slash (GH#3699).
func isAgentAddress(assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	switch assignee {
	case "mayor", "deacon":
		return true
	}
	return strings.Contains(assignee, "/")
}
