package editorial

import (
	"context"
	"fmt"
	"testing"
	"time"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

type priorFindingsStore struct {
	beadsdk.Storage
	issues map[string]*beadsdk.Issue
}

func (s *priorFindingsStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return issue, nil
}

func (s *priorFindingsStore) GetLabels(_ context.Context, id string) ([]string, error) {
	issue, ok := s.issues[id]
	if !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return append([]string(nil), issue.Labels...), nil
}

func (s *priorFindingsStore) GetDependenciesWithMetadata(_ context.Context, id string) ([]*beadsdk.IssueWithDependencyMetadata, error) {
	if _, ok := s.issues[id]; !ok {
		return nil, fmt.Errorf("issue %s not found", id)
	}
	return nil, nil
}

func TestBuildPriorFindings_ParsesIDSevPathLineTitleLines(t *testing.T) {
	notes := `Findings from the last rejection:
- id:abc123456789 sev:major internal/foo.go:42 — leaky abstraction
- id:def987654321 sev:minor internal/bar.go:7 — missing test
not a finding line
`
	store := &priorFindingsStore{issues: map[string]*beadsdk.Issue{
		"gt-source": {ID: "gt-source", Notes: notes, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}}
	bd := beads.NewWithStore(t.TempDir(), store)

	got := BuildPriorFindings(bd, "gt-source", 3)

	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	if got[0].ID != "abc123456789" || got[0].Severity != "major" || got[0].Path != "internal/foo.go" || got[0].Line != 42 || got[0].Title != "leaky abstraction" {
		t.Errorf("finding[0] = %+v, unexpected", got[0])
	}
	if got[1].ID != "def987654321" || got[1].Severity != "minor" {
		t.Errorf("finding[1] = %+v, unexpected", got[1])
	}
	for _, f := range got {
		if f.Attempt != 3 {
			t.Errorf("finding %+v: Attempt = %d, want 3", f, f.Attempt)
		}
	}
}

func TestBuildPriorFindings_CollapsesDuplicateIDs(t *testing.T) {
	// gt-3mp1's notes carried two findings written with the reviewed head sha
	// as the id. Forwarding that to om makes it reject the payload, and the
	// gate fails closed, so the duplicate must be collapsed here (gt-2ok0).
	notes := `Findings from the last rejection:
- id:34d834b sev:major internal/config/config.go:432 — unchecked error
- id:def987654321 sev:minor internal/bar.go:7 — missing test
- id:34d834b sev:major internal/config/config_test.go:898 — unchecked error
`
	store := &priorFindingsStore{issues: map[string]*beadsdk.Issue{
		"gt-source": {ID: "gt-source", Notes: notes, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}}
	bd := beads.NewWithStore(t.TempDir(), store)

	got := BuildPriorFindings(bd, "gt-source", 2)

	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (the repeated id collapsed): %+v", len(got), got)
	}
	if got[0].ID != "34d834b" || got[0].Path != "internal/config/config_test.go" || got[0].Line != 898 {
		t.Errorf("finding[0] = %+v, want the last line naming id 34d834b", got[0])
	}
	if got[1].ID != "def987654321" {
		t.Errorf("finding[1] = %+v, want the unaffected finding, in note order", got[1])
	}
}

func TestBuildPriorFindings_ParsesEmptyTitleAndColonInPath(t *testing.T) {
	// formatMergeRejectionNote's " — %s" with an empty title loses its
	// trailing space to strings.TrimSpace before this line reaches the
	// regex, so the line ends right at the em dash with nothing after it.
	// A path containing its own colon (e.g. a Windows-style path) must still
	// resolve to the line number at the LAST ":<digits>", not the first
	// colon in the line (gt-j6ez).
	notes := "- id:abc123456789 sev:major internal/foo.go:42 —\n" +
		`- id:def987654321 sev:minor C:\repo\bar.go:7 — missing test` + "\n"
	store := &priorFindingsStore{issues: map[string]*beadsdk.Issue{
		"gt-source": {ID: "gt-source", Notes: notes, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}}
	bd := beads.NewWithStore(t.TempDir(), store)

	got := BuildPriorFindings(bd, "gt-source", 1)

	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2: %+v", len(got), got)
	}
	if got[0].ID != "abc123456789" || got[0].Path != "internal/foo.go" || got[0].Line != 42 || got[0].Title != "" {
		t.Errorf("finding[0] = %+v, want empty title parsed cleanly", got[0])
	}
	if got[1].ID != "def987654321" || got[1].Path != `C:\repo\bar.go` || got[1].Line != 7 || got[1].Title != "missing test" {
		t.Errorf("finding[1] = %+v, want colon-containing path parsed as a whole", got[1])
	}
}

func TestBuildPriorFindings_ParsesBulletPrefixedLines(t *testing.T) {
	// The rejection notes are written by a formula an agent executes, and it
	// has emitted these lines under '•' — matching zero times against the
	// '-'-only pattern, so the rejection carried no findings at all
	// (gt-3mp1). Either bullet parses; the prose "FINDING [major] ..." lines
	// the same notes carry are still not findings, because they hold no om
	// finding id to classify on.
	notes := `MERGE REJECTION (attempt 1): om-editorial - findings on MR bead gt-mr-1
• id:cb332644e4cf sev:major internal/hooks/config.go:432 — boot hook override has no self-filtering path
- id:cc825768ed16 sev:minor internal/hooks/config_test.go:898 — test rewritten to agree with the regression
FINDING [major] internal/hooks/config.go boot override (~line 432): removing If is a REGRESSION
`
	store := &priorFindingsStore{issues: map[string]*beadsdk.Issue{
		"gt-source": {ID: "gt-source", Notes: notes, CreatedAt: time.Now(), UpdatedAt: time.Now()},
	}}
	bd := beads.NewWithStore(t.TempDir(), store)

	got := BuildPriorFindings(bd, "gt-source", 1)

	if len(got) != 2 {
		t.Fatalf("got %d findings, want 2 (the bullet line and the dash line): %+v", len(got), got)
	}
	if got[0].ID != "cb332644e4cf" || got[0].Path != "internal/hooks/config.go" || got[0].Line != 432 {
		t.Errorf("finding[0] = %+v, want the bullet-prefixed line parsed", got[0])
	}
	if got[1].ID != "cc825768ed16" {
		t.Errorf("finding[1] = %+v, want the dash-prefixed line", got[1])
	}
}

func TestBuildPriorFindings_NoSourceIssueReturnsNil(t *testing.T) {
	store := &priorFindingsStore{issues: map[string]*beadsdk.Issue{}}
	bd := beads.NewWithStore(t.TempDir(), store)

	if got := BuildPriorFindings(bd, "", 1); got != nil {
		t.Errorf("expected nil for empty sourceIssue, got %+v", got)
	}
	if got := BuildPriorFindings(bd, "gt-missing", 1); got != nil {
		t.Errorf("expected nil for a source issue that can't be shown, got %+v", got)
	}
}
