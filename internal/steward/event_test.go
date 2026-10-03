package steward

import (
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// readyIssue is a bead gt done submitted: the ready label and a READY TO
// LAND block, exactly as done_submit writes them.
func readyIssue(id, head string) *beads.Issue {
	return &beads.Issue{
		ID:       id,
		Status:   "open",
		Assignee: "gastown/polecats/emerald",
		Labels:   []string{land.LabelReadyToLand},
		Notes:    land.FormatReadyNote(land.Work{Branch: "polecat/emerald/" + id, Head: head, Target: "main", Worker: "emerald"}),
	}
}

// rejectedIssue is a bead the landing worker refused: rework swapped in for
// the ready label, and the MERGE REJECTION block Land writes.
func rejectedIssue(id, head string, note land.RejectionNote) *beads.Issue {
	note.Head = head
	return &beads.Issue{
		ID:       id,
		Status:   "open",
		Assignee: "",
		Labels:   []string{land.LabelRework},
		Notes:    land.FormatRejectionNote(note),
	}
}

func never(string) bool { return false }

// needsPlanningIssue is a spec the dispatcher routed to the planner: open,
// labelled, with no proposal on it yet.
func needsPlanningIssue(id string) *beads.Issue {
	return &beads.Issue{
		ID:     id,
		Status: "open",
		Labels: []string{specdispatch.NeedsPlanningLabel},
	}
}

// TestParseKind: the kind set is closed and an unknown name is an error that
// names the key, so the caller can run the default instead (gt-9bioi.7).
func TestParseKind(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		in      string
		want    Kind
		wantErr bool
	}{
		"review":    {"review", KindReview, false},
		"rejection": {"rejection", KindRejection, false},
		"empty":     {"", "", true},
		"typo":      {"reviw", "", true},
		"case":      {"Review", "", true},
		// Planning is not a kinds value: it is its own flag, so a town that
		// scans the landing queue is not asking for planning work
		// (gt-4k3fj.13).
		"plan": {"plan", "", true},
	} {
		got, err := ParseKind(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: ParseKind(%q) error = %v, wantErr %v", name, tc.in, err, tc.wantErr)
		}
		if err != nil && !strings.Contains(err.Error(), "patrols.steward.kinds") {
			t.Errorf("%s: refusal %q does not name the key", name, err)
		}
		if got != tc.want {
			t.Errorf("%s: ParseKind(%q) = %q, want %q", name, tc.in, got, tc.want)
		}
	}
	if got := DefaultKinds(); len(got) != 1 || got[0] != KindRejection {
		t.Errorf("DefaultKinds() = %v, want [rejection]", got)
	}
}

func TestDetectReview(t *testing.T) {
	t.Parallel()
	issue := readyIssue("gt-x", "c0ffee")
	ev, ok := Detect(issue, "gastown", never)
	if !ok {
		t.Fatal("no event for a submitted bead")
	}
	if ev.Kind != KindReview || ev.Bead != "gt-x" || ev.Head != "c0ffee" || ev.Branch != "polecat/emerald/gt-x" ||
		ev.Target != "main" || ev.Rig != "gastown" || ev.Worker != "emerald" {
		t.Fatalf("event = %+v", ev)
	}
	// The same head is one job, however many scans see it.
	if _, ok := Detect(issue, "gastown", func(key string) bool { return key == ev.Key() }); ok {
		t.Error("a head a job already handled raised a second event")
	}
	// A new head on the same bead is a new job.
	resubmitted := readyIssue("gt-x", "feed")
	if ev2, ok := Detect(resubmitted, "gastown", func(key string) bool { return key == ev.Key() }); !ok || ev2.Head != "feed" {
		t.Fatalf("resubmission event = %+v ok=%v", ev2, ok)
	}
}

func TestDetectRejection(t *testing.T) {
	t.Parallel()
	issue := rejectedIssue("gt-x", "c0ffee", land.RejectionNote{
		Attempt: 2, Kind: "review", Reason: "om requested changes",
		Branch: "polecat/emerald/gt-x", Target: "main", MR: "gt-x",
		Findings: []land.Finding{{ID: "abc", Severity: "major", Path: "a.go", Line: 4, Title: "bad"}},
	})
	ev, ok := Detect(issue, "gastown", never)
	if !ok {
		t.Fatal("no event for a rejected bead")
	}
	if ev.Kind != KindRejection || ev.Head != "c0ffee" || ev.Attempt != 2 || ev.Target != "main" {
		t.Fatalf("event = %+v", ev)
	}
	for _, want := range []string{"kind=review", "reason=om requested changes", "findings=abc:a.go"} {
		if !strings.Contains(ev.RejectionDetail, want) {
			t.Errorf("rejection detail %q lacks %q", ev.RejectionDetail, want)
		}
	}
	if _, ok := Detect(issue, "gastown", func(key string) bool { return key == ev.Key() }); ok {
		t.Error("a rejected head a job already handled raised a second event")
	}
}

// TestDetectPlan: an open spec labelled needs-planning raises the plan event
// once, and the planner's own proposal is what retires it (gt-4k3fj.13).
func TestDetectPlan(t *testing.T) {
	t.Parallel()
	issue := needsPlanningIssue("gt-spec")
	ev, ok := Detect(issue, "gastown", never)
	if !ok {
		t.Fatal("no event for an unplanned spec")
	}
	if ev.Kind != KindPlan || ev.Bead != "gt-spec" || ev.Rig != "gastown" {
		t.Fatalf("event = %+v", ev)
	}
	// A spec is not a submission: the planner reads the bead, so the event
	// carries no head and no branch to work at.
	if ev.Head != "" || ev.Branch != "" || ev.Target != "" {
		t.Errorf("the plan event carries submission facts: %+v", ev)
	}
	// A plan head names no commit, so the bead alone is the key.
	if _, ok := Detect(issue, "gastown", func(key string) bool { return key == ev.Key() }); ok {
		t.Error("a spec a plan job already ran on raised a second event")
	}
	// The proposal is the deliverable, so it retires the event without the
	// ledger: an operator who wants another deletes the block.
	planned := needsPlanningIssue("gt-spec")
	planned.Notes = PlanProposalMarker + "\nSpec: gt-spec\nChildren: 1\n"
	if ev, ok := Detect(planned, "gastown", never); ok {
		t.Errorf("a planned spec raised %+v", ev)
	}
	// A marker quoted inside the spec's own prose is content, not a block, and
	// the block opens with the marker alone on its line — the line the prompt
	// writes.
	quoted := needsPlanningIssue("gt-spec")
	quoted.Notes = "The steward writes a " + PlanProposalMarker + " block into these notes."
	if _, ok := Detect(quoted, "gastown", never); !ok {
		t.Error("a spec quoting the marker raised no event")
	}
	if HasPlanProposal(PlanProposalMarker + ": gt-spec\n") {
		t.Errorf("a line starting %q opened a block", PlanProposalMarker+":")
	}
}

// TestDetectSkips is everything a scan must leave alone.
func TestDetectSkips(t *testing.T) {
	t.Parallel()
	crew := readyIssue("gt-x", "c0ffee")
	crew.Assignee = "gastown/crew/sloan"
	human := rejectedIssue("gt-x", "c0ffee", land.RejectionNote{Attempt: 1, Kind: "policy", Reason: "no_merge"})
	human.Labels = append(human.Labels, land.LabelNeedsHuman)
	noready := &beads.Issue{ID: "gt-x", Status: "open", Labels: []string{land.LabelReadyToLand}, Notes: "no block here"}
	nolabel := &beads.Issue{ID: "gt-x", Status: "open", Notes: readyIssue("gt-x", "c0ffee").Notes}
	nohead := rejectedIssue("gt-x", "", land.RejectionNote{Attempt: 1, Kind: "gate", Reason: "make test exit 2"})
	closed := needsPlanningIssue("gt-x")
	closed.Status = "closed"
	unlabelled := &beads.Issue{ID: "gt-x", Status: "open"}

	for name, issue := range map[string]*beads.Issue{
		"nil": issueNil(), "crew-assigned": crew, "needs-human": human,
		"ready label without a READY TO LAND block": noready,
		"a READY TO LAND block without the label":   nolabel,
		"a rejection without a head":                nohead,
		"a closed spec":                             closed,
		"an open bead with no planning label":       unlabelled,
	} {
		if ev, ok := Detect(issue, "gastown", never); ok {
			t.Errorf("%s raised %+v", name, ev)
		}
	}
}

func issueNil() *beads.Issue { return nil }

func TestCrewAssigned(t *testing.T) {
	t.Parallel()
	for assignee, want := range map[string]bool{
		"gastown/crew/sloan":     true,
		"gastown/crew/":          true,
		"gastown/polecats/emera": false,
		"":                       false,
		"mayor":                  false,
	} {
		if got := CrewAssigned(assignee); got != want {
			t.Errorf("CrewAssigned(%q) = %v, want %v", assignee, got, want)
		}
	}
}
