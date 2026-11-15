package fixrecurrence

import (
	"cmp"
	"fmt"
	"slices"
	"strings"
)

// The Kind values Action carries: KindDepAdd and KindLabelAdd args follow bd,
// and KindEscalate args follow gt, so a runner prefixes the binary the Kind
// names.
const (
	// KindDepAdd adds one dependency edge from a citing bead to the bead it
	// cites.
	KindDepAdd = "dep-add"
	// KindLabelAdd adds one label to one bead.
	KindLabelAdd = "label-add"
	// KindEscalate raises the escalation a chronic fix needs.
	KindEscalate = "escalate"
)

// The labels Plan writes on the two ends of a claim, and the fingerprint a
// chronic escalation carries. A label names the claim and the bead it is about,
// so a reader reaches either end of it from the store alone.
const (
	labelAmbiguous     = "fix-rel:ambiguous"
	labelRecurrenceOf  = "recurrence-of:"
	labelRecurred      = "recurred"
	labelSameClassOf   = "same-class-of:"
	labelClassRecurred = "class-recurred"
	labelChronic       = "chronic"
	fingerprintPrefix  = "fix-recurrence:"
)

// Action is one write Plan proposes: the Kind of write and the Args to pass
// after the bd or gt command it names.
type Action struct {
	Kind string
	Args []string
}

// State is what the bead store already holds for the beads a plan touches: the
// dependency edges between them and each bead's labels. Plan reads it to skip a
// write the store already carries and to count the claims already made against
// a fix.
type State struct {
	// Deps holds the dependency edges the store carries between these beads,
	// each with the relationship its bd dependency type records: "related" is
	// a recurrence or a same-class claim, "caused-by" a regression and
	// "discovered-from" a follow-up. An edge of any other type may be left
	// out: Pairs already keeps a pair an agent joined with an edge from
	// becoming a proposal, so the plan never needs to read one.
	Deps map[Edge]bool
	// Labels maps a bead to the labels it already carries. A bead with no
	// entry carries none.
	Labels map[string]map[string]bool
}

// Plan returns, in a stable order, the writes that turn proposals into the
// store's dependency edges and labels, plus a chronic label and an escalation
// for every cited fix that two or more distinct citing beads now claim did not
// hold. A write the state already carries, and one an earlier proposal already
// asked for, is emitted once. Proposals are ordered by citing bead, cited bead,
// relationship and whether they are ambiguous, so the caller's order never
// decides the plan.
func Plan(proposals []Proposal, state State) []Action {
	claims := claimsByOld(state)
	ordered := slices.Clone(proposals)
	slices.SortStableFunc(ordered, func(a, b Proposal) int {
		if c := cmp.Compare(a.New, b.New); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Old, b.Old); c != 0 {
			return c
		}
		if c := cmp.Compare(a.Rel, b.Rel); c != 0 {
			return c
		}
		return compareAmbiguous(a.Ambiguous, b.Ambiguous)
	})
	var actions []Action
	seen := make(map[string]bool)
	for _, proposal := range ordered {
		actions = planProposal(actions, seen, claims, proposal, state)
	}
	return addChronic(actions, seen, claims, state)
}

// planProposal appends the writes one proposal asks for. A proposal naming no
// second bead earns nothing: an empty id has no bead to write, and a bead is no
// citing claim about itself.
func planProposal(actions []Action, seen map[string]bool, claims map[string]map[string]bool, p Proposal, state State) []Action {
	if p.New == "" || p.Old == "" || p.New == p.Old {
		return actions
	}
	// An ambiguous proposal claims no relationship, whatever its Rel holds, and
	// a relationship no write covers leaves a reader to classify the claim the
	// same way: both earn the ambiguous label rather than disappearing from the
	// plan.
	depType := depTypeOf(p.Rel)
	if p.Ambiguous || depType == "" {
		if !state.hasLabel(p.New, labelAmbiguous) {
			actions = addAction(actions, seen, labelAddAction(p.New, labelAmbiguous))
		}
		return actions
	}
	addClaim(claims, Edge{New: p.New, Old: p.Old, Rel: p.Rel})
	if !state.hasEdge(p.New, p.Old, p.Rel) {
		actions = addAction(actions, seen, Action{
			Kind: KindDepAdd,
			Args: bdWriteArgs([]string{"dep", "add", "--type=" + depType}, p.New, p.Old),
		})
	}
	for _, l := range relationshipLabels(p) {
		if !state.hasLabel(l.bead, l.label) {
			actions = addAction(actions, seen, labelAddAction(l.bead, l.label))
		}
	}
	return actions
}

// addChronic appends the chronic label and its escalation for every cited fix
// whose distinct citing beads reach chronicStrikes and that carries no chronic
// label yet, sorted by cited bead so the plan does not depend on map order. A
// fix that carries the label needs neither write: its escalation is open, and
// the label is what a later run and a reader look for.
func addChronic(actions []Action, seen map[string]bool, claims map[string]map[string]bool, state State) []Action {
	var cited []string
	for old, citing := range claims {
		if len(citing) >= chronicStrikes && !state.hasLabel(old, labelChronic) {
			cited = append(cited, old)
		}
	}
	slices.Sort(cited)
	for _, old := range cited {
		actions = addAction(actions, seen, labelAddAction(old, labelChronic))
		actions = addAction(actions, seen, escalateAction(old, len(claims[old])))
	}
	return actions
}

// claimsByOld is the distinct citing beads each cited bead already has a
// failing claim from, read from the edges the store carries.
func claimsByOld(state State) map[string]map[string]bool {
	claims := make(map[string]map[string]bool)
	for edge := range state.Deps {
		addClaim(claims, edge)
	}
	return claims
}

// addClaim records edge's citing bead among the beads whose claim says edge's
// cited fix did not hold. An empty id, a bead citing itself and a relationship
// that leaves the fix standing are no claim.
func addClaim(claims map[string]map[string]bool, edge Edge) {
	if edge.New == "" || edge.Old == "" || edge.New == edge.Old || !claimsFix(edge.Rel) {
		return
	}
	if claims[edge.Old] == nil {
		claims[edge.Old] = make(map[string]bool)
	}
	claims[edge.Old][edge.New] = true
}

// depTypeOf is the bd dependency type the edge a relationship claims is
// recorded as: the store has one "related" type for both a recurrence and a
// same-class claim, and the other two are bd's own names. A relationship the
// package does not define writes no edge.
func depTypeOf(rel Relationship) string {
	switch rel {
	case Recurrence, SameClass:
		return "related"
	case Regression:
		return "caused-by"
	case FollowUp:
		return "discovered-from"
	}
	return ""
}

// claimsFix reports whether rel says the cited fix did not hold. Follow-up,
// which says the work continues, and reference, which claims nothing, leave it
// standing, and a relationship the package does not define is not one of the
// three Plan escalates, so it counts for nothing.
func claimsFix(rel Relationship) bool {
	switch rel {
	case Recurrence, SameClass, Regression:
		return true
	}
	return false
}

// beadLabel is one label one bead's write carries.
type beadLabel struct {
	bead  string
	label string
}

// relationshipLabels are the labels a relationship's claim carries, in the
// order the plan writes them: the citing bead names the claim and the cited
// bead records that its fix failed, so either end leads a reader to the other.
func relationshipLabels(p Proposal) []beadLabel {
	switch p.Rel {
	case Recurrence:
		return []beadLabel{
			{p.New, labelRecurrenceOf + p.Old},
			{p.Old, labelRecurred},
		}
	case SameClass:
		return []beadLabel{
			{p.New, labelSameClassOf + p.Old},
			{p.Old, labelClassRecurred},
		}
	}
	return nil
}

// labelAddAction is the bd write that adds one label to one bead.
func labelAddAction(bead, label string) Action {
	return Action{Kind: KindLabelAdd, Args: bdWriteArgs([]string{"update", "--add-label=" + label}, bead)}
}

// bdWriteArgs is the argv of a bd write: the command and its flags, then the
// bead ids past a "--" so a bead id that starts with a hyphen stays an operand
// and is never read as a flag.
func bdWriteArgs(flags []string, ids ...string) []string {
	args := append(slices.Clone(flags), "--")
	return append(args, ids...)
}

// compareAmbiguous orders an ambiguous proposal before one that claims a
// relationship.
func compareAmbiguous(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return -1
	}
	return 1
}

// escalateAction is the escalation a chronic fix raises: its fingerprint names
// the cited bead, so a repeat firing records on the open escalation instead of
// minting another, and medium is the severity the town asks for a chronic fix
// (gt-p7isw0).
func escalateAction(old string, citing int) Action {
	return Action{
		Kind: KindEscalate,
		Args: []string{
			"escalate",
			fmt.Sprintf("chronic fix %s: %d beads report it did not hold", old, citing),
			"--severity", "medium",
			"--fingerprint", fingerprintPrefix + old,
		},
	}
}

// addAction appends a unless an earlier action in this plan already asked for
// it. The caller has already dropped a write the state carries.
func addAction(actions []Action, seen map[string]bool, a Action) []Action {
	key := a.Kind + "\x00" + strings.Join(a.Args, "\x00")
	if seen[key] {
		return actions
	}
	seen[key] = true
	return append(actions, a)
}

// hasEdge reports whether the state already carries the dependency edge an
// action of rel would add. The bd dependency type decides, not the Relationship
// value, because the store spells a recurrence and a same-class claim with the
// same type.
func (s State) hasEdge(new, old string, rel Relationship) bool {
	want := depTypeOf(rel)
	for edge := range s.Deps {
		if edge.New == new && edge.Old == old && depTypeOf(edge.Rel) == want {
			return true
		}
	}
	return false
}

// hasLabel reports whether the state already carries label on bead.
func (s State) hasLabel(bead, label string) bool {
	return s.Labels[bead][label]
}
