package fixrecurrence

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// The test spells each write's argv itself, so a row pins the contract a runner
// reads rather than echoing the implementation.
func depAdd(new, old, depType string) Action {
	return Action{Kind: KindDepAdd, Args: []string{"dep", "add", "--type=" + depType, "--", new, old}}
}

func labelAdd(bead, label string) Action {
	return Action{Kind: KindLabelAdd, Args: []string{"update", "--add-label=" + label, "--", bead}}
}

func chronicEscalate(old string, citing int) Action {
	return Action{Kind: KindEscalate, Args: []string{
		"escalate",
		fmt.Sprintf("chronic fix %s: %d beads report it did not hold", old, citing),
		"--severity", "medium",
		"--fingerprint", "fix-recurrence:" + old,
	}}
}

// eqActions reports whether got and want carry the same writes in the same
// order.
func eqActions(got, want []Action) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i].Kind != want[i].Kind || !slices.Equal(got[i].Args, want[i].Args) {
			return false
		}
	}
	return true
}

func TestPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		proposals []Proposal
		state     State
		want      []Action
	}{
		{
			name:      "a recurrence adds the related edge and names the claim on both beads",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Recurrence}},
			want: []Action{
				depAdd("gt-new", "gt-old", "related"),
				labelAdd("gt-new", "recurrence-of:gt-old"),
				labelAdd("gt-old", "recurred"),
			},
		},
		{
			name:      "a same-class claim adds the related edge and its own labels",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: SameClass}},
			want: []Action{
				depAdd("gt-new", "gt-old", "related"),
				labelAdd("gt-new", "same-class-of:gt-old"),
				labelAdd("gt-old", "class-recurred"),
			},
		},
		{
			name:      "a regression adds the caused-by edge and no label",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Regression}},
			want: []Action{
				depAdd("gt-new", "gt-old", "caused-by"),
			},
		},
		{
			name:      "a follow-up adds the discovered-from edge and no label",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: FollowUp}},
			want: []Action{
				depAdd("gt-new", "gt-old", "discovered-from"),
			},
		},
		{
			name:      "an ambiguous proposal earns only the ambiguous label",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Reference, Ambiguous: true}},
			want:      []Action{labelAdd("gt-new", "fix-rel:ambiguous")},
		},
		{
			name:      "a reference no classifier claimed is routed to a reader too",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Reference}},
			want:      []Action{labelAdd("gt-new", "fix-rel:ambiguous")},
		},
		{
			name:      "an ambiguous label the state carries is not written again",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Reference, Ambiguous: true}},
			state:     State{Labels: map[string]map[string]bool{"gt-new": {"fix-rel:ambiguous": true}}},
		},
		{
			name:      "an edge the state carries is not added again, but its labels are",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Recurrence}},
			state:     State{Deps: map[Edge]bool{{New: "gt-new", Old: "gt-old", Rel: Recurrence}: true}},
			want: []Action{
				labelAdd("gt-new", "recurrence-of:gt-old"),
				labelAdd("gt-old", "recurred"),
			},
		},
		{
			name:      "the store's one related type covers a same-class spelling of the edge",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Recurrence}},
			state:     State{Deps: map[Edge]bool{{New: "gt-new", Old: "gt-old", Rel: SameClass}: true}},
			want: []Action{
				labelAdd("gt-new", "recurrence-of:gt-old"),
				labelAdd("gt-old", "recurred"),
			},
		},
		{
			name:      "the same edge to a different bead is no reason to skip",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Regression}},
			state:     State{Deps: map[Edge]bool{{New: "gt-new", Old: "gt-other", Rel: Regression}: true}},
			want:      []Action{depAdd("gt-new", "gt-old", "caused-by")},
		},
		{
			name:      "a label the state carries is not written again",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Recurrence}},
			state:     State{Labels: map[string]map[string]bool{"gt-new": {"recurrence-of:gt-old": true}}},
			want: []Action{
				depAdd("gt-new", "gt-old", "related"),
				labelAdd("gt-old", "recurred"),
			},
		},
		{
			name:      "one claim in the state and one in the plan make the cited fix chronic",
			proposals: []Proposal{{New: "gt-b", Old: "gt-old", Rel: SameClass}},
			state:     State{Deps: map[Edge]bool{{New: "gt-a", Old: "gt-old", Rel: Recurrence}: true}},
			want: []Action{
				depAdd("gt-b", "gt-old", "related"),
				labelAdd("gt-b", "same-class-of:gt-old"),
				labelAdd("gt-old", "class-recurred"),
				labelAdd("gt-old", "chronic"),
				chronicEscalate("gt-old", 2),
			},
		},
		{
			name: "two claims already in the state make a chronic pair, sorted by cited bead",
			state: State{Deps: map[Edge]bool{
				{New: "gt-b", Old: "gt-zzz", Rel: Regression}: true,
				{New: "gt-a", Old: "gt-zzz", Rel: Recurrence}: true,
				{New: "gt-c", Old: "gt-aaa", Rel: Recurrence}: true,
				{New: "gt-d", Old: "gt-aaa", Rel: SameClass}:  true,
			}},
			want: []Action{
				labelAdd("gt-aaa", "chronic"),
				chronicEscalate("gt-aaa", 2),
				labelAdd("gt-zzz", "chronic"),
				chronicEscalate("gt-zzz", 2),
			},
		},
		{
			name:      "a cited fix that already carries the chronic label gets neither write",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: Recurrence}},
			state: State{
				Deps: map[Edge]bool{
					{New: "gt-a", Old: "gt-old", Rel: Recurrence}: true,
					{New: "gt-b", Old: "gt-old", Rel: Regression}: true,
				},
				Labels: map[string]map[string]bool{"gt-old": {"chronic": true}},
			},
			want: []Action{
				depAdd("gt-new", "gt-old", "related"),
				labelAdd("gt-new", "recurrence-of:gt-old"),
				labelAdd("gt-old", "recurred"),
			},
		},
		{
			name: "two claims from one citing bead are one incident, ordered by relationship",
			proposals: []Proposal{
				{New: "gt-new", Old: "gt-old", Rel: Recurrence},
				{New: "gt-new", Old: "gt-old", Rel: SameClass},
			},
			want: []Action{
				depAdd("gt-new", "gt-old", "related"),
				labelAdd("gt-new", "recurrence-of:gt-old"),
				labelAdd("gt-old", "recurred"),
				labelAdd("gt-new", "same-class-of:gt-old"),
				labelAdd("gt-old", "class-recurred"),
			},
		},
		{
			name:      "a dash-prefixed id stays an operand past the separator",
			proposals: []Proposal{{New: "-gt-new", Old: "gt-old", Rel: Recurrence}},
			want: []Action{
				depAdd("-gt-new", "gt-old", "related"),
				labelAdd("-gt-new", "recurrence-of:gt-old"),
				labelAdd("gt-old", "recurred"),
			},
		},
		{
			name:      "follow-up edges leave the fix standing",
			proposals: []Proposal{{New: "gt-new", Old: "gt-old", Rel: FollowUp}},
			state: State{Deps: map[Edge]bool{
				{New: "gt-a", Old: "gt-old", Rel: FollowUp}: true,
				{New: "gt-b", Old: "gt-old", Rel: FollowUp}: true,
			}},
			want: []Action{depAdd("gt-new", "gt-old", "discovered-from")},
		},
		{
			name: "the plan is sorted by citing bead then cited bead whatever the input order",
			proposals: []Proposal{
				{New: "gt-new2", Old: "gt-bbb", Rel: Regression},
				{New: "gt-new", Old: "gt-ccc", Rel: Recurrence},
			},
			want: []Action{
				depAdd("gt-new", "gt-ccc", "related"),
				labelAdd("gt-new", "recurrence-of:gt-ccc"),
				labelAdd("gt-ccc", "recurred"),
				depAdd("gt-new2", "gt-bbb", "caused-by"),
			},
		},
		{
			name: "a proposal naming no second bead earns nothing",
			proposals: []Proposal{
				{New: "", Old: "gt-old", Rel: Recurrence},
				{New: "gt-new", Old: "", Rel: Recurrence},
				{New: "gt-same", Old: "gt-same", Rel: Recurrence},
			},
		},
		{
			name: "no proposal and an empty state make no action",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := Plan(tt.proposals, tt.state); !eqActions(got, tt.want) {
				t.Errorf("Plan() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestPlanTiesDoNotFollowInputOrder pins the order of proposals that agree on
// citing and cited bead: the relationship, then the ambiguous flag, decides it,
// so reversing the caller's slice leaves the plan unchanged.
func TestPlanTiesDoNotFollowInputOrder(t *testing.T) {
	t.Parallel()
	proposals := []Proposal{
		{New: "gt-new", Old: "gt-old", Rel: Recurrence},
		{New: "gt-new", Old: "gt-old", Rel: Recurrence, Ambiguous: true},
		{New: "gt-new", Old: "gt-old", Rel: SameClass},
	}
	reversed := slices.Clone(proposals)
	slices.Reverse(reversed)
	want := []Action{
		labelAdd("gt-new", "fix-rel:ambiguous"),
		depAdd("gt-new", "gt-old", "related"),
		labelAdd("gt-new", "recurrence-of:gt-old"),
		labelAdd("gt-old", "recurred"),
		labelAdd("gt-new", "same-class-of:gt-old"),
		labelAdd("gt-old", "class-recurred"),
	}
	if got := Plan(proposals, State{}); !eqActions(got, want) {
		t.Errorf("Plan() = %#v, want %#v", got, want)
	}
	if got := Plan(reversed, State{}); !eqActions(got, want) {
		t.Errorf("Plan(reversed) = %#v, want %#v", got, want)
	}
}

func TestPlanIsIdempotent(t *testing.T) {
	t.Parallel()
	proposals := []Proposal{
		{New: "gt-a", Old: "gt-old", Rel: Recurrence},
		{New: "gt-b", Old: "gt-old", Rel: Regression},
		{New: "gt-c", Old: "gt-old2", Rel: FollowUp},
		{New: "gt-d", Old: "gt-old3", Rel: Reference, Ambiguous: true},
	}
	state := State{
		Deps:   map[Edge]bool{{New: "gt-x", Old: "gt-old", Rel: SameClass}: true},
		Labels: map[string]map[string]bool{"gt-c": {"discovered-from:gt-old2": true}},
	}
	first := Plan(proposals, state)
	if len(first) == 0 {
		t.Fatalf("Plan() made no action for a proposal set that earns writes")
	}
	second := Plan(proposals, applyActions(t, state, first))
	if len(second) != 0 {
		t.Errorf("second Plan() = %#v, want no action: every write the first plan made is in the state", second)
	}
}

// applyActions returns the state a runner leaves behind after running actions:
// one edge per dep-add and one label per label-add. An escalation writes no
// bead, so it changes nothing.
func applyActions(t *testing.T, state State, actions []Action) State {
	t.Helper()
	next := State{Deps: make(map[Edge]bool), Labels: make(map[string]map[string]bool)}
	for edge := range state.Deps {
		next.Deps[edge] = true
	}
	for bead, labels := range state.Labels {
		next.Labels[bead] = make(map[string]bool, len(labels))
		for label := range labels {
			next.Labels[bead][label] = true
		}
	}
	for _, a := range actions {
		switch a.Kind {
		case KindDepAdd:
			if len(a.Args) != 6 || a.Args[3] != "--" {
				t.Fatalf("dep-add args = %v, want dep add --type=<type> -- <new> <old>", a.Args)
			}
			typ := strings.TrimPrefix(a.Args[2], "--type=")
			next.Deps[Edge{New: a.Args[4], Old: a.Args[5], Rel: relationshipOfType(t, typ)}] = true
		case KindLabelAdd:
			if len(a.Args) != 4 || a.Args[2] != "--" {
				t.Fatalf("label-add args = %v, want update --add-label=<label> -- <bead>", a.Args)
			}
			bead, label := a.Args[3], strings.TrimPrefix(a.Args[1], "--add-label=")
			if next.Labels[bead] == nil {
				next.Labels[bead] = make(map[string]bool)
			}
			next.Labels[bead][label] = true
		case KindEscalate:
		default:
			t.Fatalf("action kind %q is not one Plan documents", a.Kind)
		}
	}
	return next
}

// relationshipOfType is a relationship whose edge the bd dependency type
// records; Plan compares the type, so any of them will do.
func relationshipOfType(t *testing.T, typ string) Relationship {
	t.Helper()
	switch typ {
	case "related":
		return Recurrence
	case "caused-by":
		return Regression
	case "discovered-from":
		return FollowUp
	}
	t.Fatalf("dependency type %q is not one Plan writes", typ)
	return ""
}
