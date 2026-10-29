package contract

import (
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
	"github.com/steveyegge/gastown/tracker/store"
)

func init() {
	register(Case{Name: "export streams one rig's issues, wisps included, then their labels, dependencies and comments", Fake: true, Run: exportOneRig})
}

// exportOneRig pins the export's contract: one rig only, durable and ephemeral
// issues, the issues before anything else, and each section in a stable order.
func exportOneRig(t *testing.T, s store.Store) {
	durable := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "durable", Labels: []string{"b", "a"}})
	wisp := mustCreate(t, s, api.CreateParams{Rig: "gt", Title: "wisp", Ephemeral: true})
	other := mustCreate(t, s, api.CreateParams{Rig: "hq", Title: "hq issue", Labels: []string{"x"}})
	mustDep(t, s, wisp.ID, durable.ID, "blocks")
	mustDep(t, s, durable.ID, "external:hq:"+other.ID, "tracks")
	mustDep(t, s, other.ID, durable.ID, "blocks")
	for _, text := range []string{"first", "second"} {
		if err := s.AddComment(ctx(), durable.ID, "dipper", text); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AddComment(ctx(), other.ID, "dipper", "hq note"); err != nil {
		t.Fatal(err)
	}

	var got []api.ExportRecord
	err := s.Export(ctx(), "gt", func(rec api.ExportRecord) error {
		got = append(got, rec)
		return nil
	})
	if err != nil {
		t.Fatalf("Export(gt): %v", err)
	}

	var issues, labels, deps, comments []string
	var issueRecs []api.Issue
	seenPastIssues := false
	for _, rec := range got {
		switch rec.Type {
		case api.RecordIssue:
			if seenPastIssues {
				t.Errorf("issue %s follows a label, dependency or comment; the issues come first", rec.Issue.ID)
			}
			issueRecs = append(issueRecs, *rec.Issue)
			issues = append(issues, rec.Issue.ID)
		case api.RecordLabel:
			seenPastIssues = true
			labels = append(labels, rec.Label.IssueID+"="+rec.Label.Label)
		case api.RecordDependency:
			seenPastIssues = true
			deps = append(deps, rec.Dependency.IssueID+"->"+rec.Dependency.DependsOn+" "+rec.Dependency.Type)
		case api.RecordComment:
			seenPastIssues = true
			comments = append(comments, rec.Comment.IssueID+" "+rec.Comment.Text)
		default:
			t.Errorf("record type %q is not one of the export's", rec.Type)
		}
	}

	wantIssues := []string{durable.ID, wisp.ID}
	slices.Sort(wantIssues)
	if !slices.Equal(issues, wantIssues) {
		t.Errorf("issues = %v, want the rig's durable and wisp issues by id %v; hq's issue is not gt's", issues, wantIssues)
	}
	for _, is := range issueRecs {
		if want := is.ID == wisp.ID; is.Ephemeral != want {
			t.Errorf("issue %s ephemeral = %v, want %v", is.ID, is.Ephemeral, want)
		}
	}

	wantLabels := []string{durable.ID + "=a", durable.ID + "=b"}
	if !slices.Equal(labels, wantLabels) {
		t.Errorf("labels = %v, want %v: sorted within the issue, and hq's label left out", labels, wantLabels)
	}

	wantDeps := []string{
		wisp.ID + "->" + durable.ID + " blocks",
		durable.ID + "->external:hq:" + other.ID + " tracks",
	}
	sameSet(t, "the dependencies of gt's issues", deps, wantDeps...)
	if !slices.IsSortedFunc(deps, func(a, b string) int { return strings.Compare(issueOf(a), issueOf(b)) }) {
		t.Errorf("dependencies = %v, want them in issue id order", deps)
	}

	wantComments := []string{durable.ID + " first", durable.ID + " second"}
	if !slices.Equal(comments, wantComments) {
		t.Errorf("comments = %v, want %v: in the order they were added, and hq's note left out", comments, wantComments)
	}
}

// issueOf is the issue id a "from->to type" line starts with.
func issueOf(line string) string {
	id, _, _ := strings.Cut(line, "->")
	return id
}
