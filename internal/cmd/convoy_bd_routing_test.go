package cmd

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	convoyops "github.com/steveyegge/gastown/internal/convoy"
)

// convoyCLIFixture is a convoyCLI over a temp town whose database is a
// fake. Nothing it does starts a process or reads the cwd, the environment
// or a package global.
type convoyCLIFixture struct {
	c       convoyCLI
	root    string
	town    *beadsfake.Fake
	out     *bytes.Buffer
	ensured []string // beads dirs the convoy types were registered in
}

// trackFailsStore is a town database whose tracks edge to failDep fails.
type trackFailsStore struct {
	*beadsfake.Fake
	failDep string
}

func (s trackFailsStore) AddTypedDependency(issue, dependsOn, depType string) error {
	if dependsOn == s.failDep {
		return errors.New("simulated tracking failure")
	}
	return s.Fake.AddTypedDependency(issue, dependsOn, depType)
}

// newConvoyCLIFixture builds the fixture; a tracks edge to failDep ("" for
// none) fails.
func newConvoyCLIFixture(t *testing.T, failDep string) *convoyCLIFixture {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".beads"), 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	fx := &convoyCLIFixture{root: root, town: beadsfake.New(beadsfake.WithPrefix("hq")), out: &bytes.Buffer{}}
	fx.c = convoyCLI{
		townRoot: func() (string, error) { return root, nil },
		townDB: func(townBeads string) convoyops.Store {
			if townBeads != root {
				t.Errorf("town database opened at %q, want the town root %q", townBeads, root)
			}
			return trackFailsStore{fx.town, failDep}
		},
		out:     fx.out,
		warn:    io.Discard,
		entropy: strings.NewReader("abcde"),
		sender:  func() string { return "mayor/" },
		ensureTypes: func(beadsDir string) error {
			fx.ensured = append(fx.ensured, beadsDir)
			return nil
		},
	}
	return fx
}

// seedIssues stores open tasks with ids in the town database, as tracks
// edge targets.
func (fx *convoyCLIFixture) seedIssues(ids ...string) {
	for _, id := range ids {
		fx.town.Seed(beads.Issue{ID: id, Title: id, Status: "open"})
	}
}

// tracked is the IDs convoyID tracks, in the order the edges were added.
func (fx *convoyCLIFixture) tracked(t *testing.T, convoyID string) []string {
	t.Helper()
	deps, err := fx.town.DepList(convoyID, "tracks")
	if err != nil {
		t.Fatalf("DepList %s: %v", convoyID, err)
	}
	var ids []string
	for _, d := range deps {
		ids = append(ids, d.ID)
	}
	return ids
}

// seedConvoy stores the convoy hq-cv-test in the town database.
func (fx *convoyCLIFixture) seedConvoy(status string) {
	fx.town.Seed(beads.Issue{ID: "hq-cv-test", Title: "Test Convoy", Status: status, Labels: []string{"gt:convoy"}})
}

// TestRunConvoyList_ReadsTheTownDatabase: gt convoy list --json --all reads
// the convoys, closed ones included, from the town database at the town root.
func TestRunConvoyList_ReadsTheTownDatabase(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, "")
	fx.town.Seed(
		beads.Issue{ID: "hq-cv-town", Title: "Town convoy", CreatedAt: "2026-03-09T00:00:00Z", Labels: []string{"gt:convoy"}},
		beads.Issue{ID: "hq-cv-shut", Title: "Closed convoy", Status: "closed", CreatedAt: "2026-03-08T00:00:00Z", Labels: []string{"gt:convoy"}},
	)

	if err := fx.c.list(convoyListOptions{json: true, all: true}); err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, id := range []string{"hq-cv-town", "hq-cv-shut"} {
		if !strings.Contains(fx.out.String(), `"id": "`+id+`"`) {
			t.Errorf("convoy %s missing from the --all JSON output:\n%s", id, fx.out.String())
		}
	}
}

// TestRunConvoyStatus_ReadsTheTownDatabase: gt convoy status <id> reads the
// convoy from the town database and reports its progress.
func TestRunConvoyStatus_ReadsTheTownDatabase(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, "")
	fx.town.Seed(beads.Issue{ID: "hq-cv-status", Title: "Status convoy", Type: "convoy", CreatedAt: "2026-03-09T00:00:00Z"})

	if err := fx.c.status(false, []string{"hq-cv-status"}); err != nil {
		t.Fatalf("status: %v", err)
	}
	out := fx.out.String()
	if !strings.Contains(out, "hq-cv-status") || !strings.Contains(out, "Progress:  0/0 completed") {
		t.Fatalf("unexpected status output:\n%s", out)
	}
}

// TestConvoyCreate_UsesTrackingHelper: convoy create writes the convoy in the
// town database under an hq-cv-* ID drawn from its entropy, then records one
// tracks edge per issue.
func TestConvoyCreate_UsesTrackingHelper(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, "")
	fx.seedIssues("mo-2sh.1")

	if err := fx.c.create(convoyCreateOptions{}, []string{"test-convoy", "mo-2sh.1"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if c, err := fx.town.Show("hq-cv-pqrst"); err != nil || c.Title != "test-convoy" || strings.Join(c.Labels, ",") != "gt:convoy" {
		t.Errorf("convoy hq-cv-pqrst not created as test-convoy/gt:convoy: %+v, %v", c, err)
	}
	if got := strings.Join(fx.tracked(t, "hq-cv-pqrst"), ","); got != "mo-2sh.1" {
		t.Errorf("tracked = %q, want mo-2sh.1", got)
	}
}

// TestConvoyCreate_RegistersTypesInTownBeadsDir is the hq-dt4 regression:
// the convoy types and statuses are registered in the town's .beads
// directory, not the workspace root, or every convoy reads as empty.
func TestConvoyCreate_RegistersTypesInTownBeadsDir(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, "")

	if err := fx.c.create(convoyCreateOptions{}, []string{"test-convoy", "gt-abc"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	if want := filepath.Join(fx.root, ".beads"); len(fx.ensured) != 1 || fx.ensured[0] != want {
		t.Fatalf("types registered in %v, want [%s]", fx.ensured, want)
	}
}

// TestConvoyAdd_UsesTrackingHelper: convoy add checks the convoy in the town
// database and records a tracks edge for each issue, in order.
func TestConvoyAdd_UsesTrackingHelper(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, "")
	fx.seedConvoy("open")
	fx.seedIssues("ag-95s.1", "ag-95s.2")

	if err := fx.c.add([]string{"hq-cv-test", "ag-95s.1", "ag-95s.2"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	if got := strings.Join(fx.tracked(t, "hq-cv-test"), ","); got != "ag-95s.1,ag-95s.2" {
		t.Errorf("tracked = %q, want ag-95s.1,ag-95s.2", got)
	}
}

// TestConvoyAdd_ReportsOnlyIssuesActuallyAdded: an issue whose tracks edge
// fails is left out of the added count and list.
func TestConvoyAdd_ReportsOnlyIssuesActuallyAdded(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, "ag-95s.2")
	fx.seedConvoy("open")
	fx.seedIssues("ag-95s.1", "ag-95s.2", "ag-95s.3")

	if err := fx.c.add([]string{"hq-cv-test", "ag-95s.1", "ag-95s.2", "ag-95s.3"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	out := fx.out.String()
	if !strings.Contains(out, "Added 2 issue(s)") {
		t.Errorf("output should report 2 added issues, got:\n%s", out)
	}
	if !strings.Contains(out, "Issues: ag-95s.1, ag-95s.3") {
		t.Errorf("output should list the issues actually added (1 and 3), got:\n%s", out)
	}
	if strings.Contains(out, "ag-95s.2") {
		t.Errorf("output must not list the failed issue ag-95s.2, got:\n%s", out)
	}
}

// TestConvoyAdd_ReopensAClosedConvoy: adding to a closed convoy reopens it
// and forgets its completion notice, so the new work is reported when done.
func TestConvoyAdd_ReopensAClosedConvoy(t *testing.T) {
	t.Parallel()
	fx := newConvoyCLIFixture(t, "")
	desc := beads.SetConvoyFields(&beads.Issue{}, &beads.ConvoyFields{Owner: "mayor/", CompletionNotifiedAt: "2026-09-30T00:00:00Z"})
	fx.town.Seed(beads.Issue{ID: "hq-cv-test", Title: "Test Convoy", Status: "closed", Description: desc, Labels: []string{"gt:convoy"}})
	fx.seedIssues("ag-95s.1")

	if err := fx.c.add([]string{"hq-cv-test", "ag-95s.1"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	got, err := fx.town.Show("hq-cv-test")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "open" {
		t.Errorf("status = %q, want reopened", got.Status)
	}
	if f := beads.ParseConvoyFields(got); f == nil || f.CompletionNotifiedAt != "" || f.Owner != "mayor/" {
		t.Errorf("convoy fields after reopen = %+v, want the notice cleared and the owner kept", f)
	}
	if !strings.Contains(fx.out.String(), "Reopened convoy hq-cv-test") {
		t.Errorf("output:\n%s", fx.out.String())
	}
}
