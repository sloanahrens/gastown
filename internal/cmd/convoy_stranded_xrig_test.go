package cmd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	beadsdk "github.com/steveyegge/beads"
	convoyops "github.com/steveyegge/gastown/internal/convoy"
)

// xrigStrandedStore is one rig's beads store for the stranded scan's blocker
// check: its issues and the raw dependency edges recorded on them.
type xrigStrandedStore struct {
	beadsdk.Storage
	issues map[string]*beadsdk.Issue
	deps   []*beadsdk.Dependency
}

func (s *xrigStrandedStore) GetIssue(_ context.Context, id string) (*beadsdk.Issue, error) {
	return s.issues[id], nil
}

func (s *xrigStrandedStore) GetIssuesByIDs(_ context.Context, ids []string) ([]*beadsdk.Issue, error) {
	var out []*beadsdk.Issue
	for _, id := range ids {
		if iss, ok := s.issues[id]; ok {
			out = append(out, iss)
		}
	}
	return out, nil
}

func (s *xrigStrandedStore) GetDependencyRecords(_ context.Context, issueID string) ([]*beadsdk.Dependency, error) {
	var out []*beadsdk.Dependency
	for _, d := range s.deps {
		if d.IssueID == issueID {
			out = append(out, d)
		}
	}
	return out, nil
}

// strandedXrigTown writes a town whose convoy hq-xr tracks gt-work and gt-sib.
// bd show answers both the way bd does for a bead whose blocker is in another
// rig: open, a dependency_count, and no blockers or dependencies listed
// (gt-db8y's shape), so bd alone reads gt-work as unblocked.
func strandedXrigTown(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	townRoot := t.TempDir()
	beadsDir := filepath.Join(townRoot, ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatalf("mkdir .beads: %v", err)
	}
	routes := `{"prefix":"hq-","path":"."}` + "\n" +
		`{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n" +
		`{"prefix":"oag-","path":"oag/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, "routes.jsonl"), []byte(routes), 0644); err != nil {
		t.Fatalf("write routes: %v", err)
	}
	script := `#!/bin/sh
i=0
for arg in "$@"; do
  case "$arg" in
    --*) ;;
    *) eval "pos$i=\"$arg\""; i=$((i+1)) ;;
  esac
done
case "$pos0" in
  list)
    echo '[{"id":"hq-xr","title":"Cross-rig convoy"}]'
    ;;
  sql)
    case "$*" in
      *"issue_id = 'hq-xr'"*) echo '[{"depends_on_id":"external:gt:gt-work"},{"depends_on_id":"external:gt:gt-sib"}]' ;;
      *) echo '[]' ;;
    esac
    ;;
  show)
    echo '[{"id":"gt-work","title":"Work","status":"open","priority":1,"issue_type":"task","assignee":"","dependency_count":1},{"id":"gt-sib","title":"Sibling","status":"open","priority":2,"issue_type":"task","assignee":""}]'
    ;;
  *)
    echo '[]'
    ;;
esac
exit 0
`
	if runtime.GOOS == "windows" {
		t.Skip("skipping convoy test on Windows")
	}
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write mock bd: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return townRoot
}

// storeBlockCheck opens the stranded scan's blocker check over the given
// stores, the daemon's wiring: every store is held up front.
func storeBlockCheck(stores map[string]beadsdk.Storage) func(string) (blockCheck, func(), error) {
	return func(townRoot string) (blockCheck, func(), error) {
		resolver := convoyops.NewStoreResolver(townRoot, stores)
		return func(id string) string {
			return convoyops.BlockReason(context.Background(), stores["hq"], id, resolver)
		}, func() {}, nil
	}
}

// noBlockers is a blocker check that finds nothing, for scans about something
// other than dependencies.
func noBlockers(string) (blockCheck, func(), error) {
	return func(string) string { return "" }, func() {}, nil
}

// TestFindStrandedConvoys_CrossRigBlockerNotReady is gt-j02xy on the daemon's
// stranded path (review minor 7): gt-work is blocked by oag-x, open in a third
// rig, and bd show drops that blocker. The scan must not offer gt-work as
// ready; when oag-x closes it must.
func TestFindStrandedConvoys_CrossRigBlockerNotReady(t *testing.T) {
	for _, tc := range []struct {
		blocker   beadsdk.Status
		wantReady []string
	}{
		{beadsdk.StatusOpen, []string{"gt-sib"}},
		{beadsdk.StatusClosed, []string{"gt-work", "gt-sib"}},
	} {
		t.Run(string(tc.blocker), func(t *testing.T) {
			townRoot := strandedXrigTown(t)
			gastown := &xrigStrandedStore{issues: map[string]*beadsdk.Issue{
				"gt-work": {ID: "gt-work", Status: beadsdk.StatusOpen},
				"gt-sib":  {ID: "gt-sib", Status: beadsdk.StatusOpen},
			}, deps: []*beadsdk.Dependency{
				{IssueID: "gt-work", DependsOnID: "external:oag:oag-x", Type: "blocks"},
			}}
			oag := &xrigStrandedStore{issues: map[string]*beadsdk.Issue{
				"oag-x": {ID: "oag-x", Status: tc.blocker},
			}}
			hq := &xrigStrandedStore{issues: map[string]*beadsdk.Issue{}}
			check := storeBlockCheck(map[string]beadsdk.Storage{"hq": hq, "gastown": gastown, "oag": oag})

			stranded, err := findStrandedConvoysWith(townRoot, check)
			if err != nil {
				t.Fatalf("findStrandedConvoysWith: %v", err)
			}
			if len(stranded) != 1 {
				t.Fatalf("want 1 stranded convoy, got %+v", stranded)
			}
			if got := strings.Join(stranded[0].ReadyIssues, ","); got != strings.Join(tc.wantReady, ",") {
				t.Errorf("ReadyIssues = %q, want %q", got, strings.Join(tc.wantReady, ","))
			}
		})
	}
}

// TestFindStrandedConvoys_BlockCheckOpensOnlyWithCandidates: a scan with no
// otherwise-ready bead opens no store.
func TestFindStrandedConvoys_BlockCheckOpensOnlyWithCandidates(t *testing.T) {
	_, townBeads, _ := mockBdForConvoyTest(t, "hq-empty-open", "Empty convoy")
	opened := 0
	open := func(string) (blockCheck, func(), error) {
		opened++
		return func(string) string { return "" }, func() {}, nil
	}
	if _, err := findStrandedConvoysWith(townBeads, open); err != nil {
		t.Fatalf("findStrandedConvoysWith: %v", err)
	}
	if opened != 0 {
		t.Errorf("blocker check opened %d times for a scan with no candidates, want 0", opened)
	}
}

// TestFindStrandedConvoys_TownStoreDownFailsTheScan (review round 2): a town
// store that will not open must fail the scan, not hold every candidate in
// silence. The daemon drops a successful run's stderr, so a silent hold read
// as "N tracked, 0 ready" every scan with no cause; an error makes gt exit
// non-zero and the daemon log "stranded scan failed".
func TestFindStrandedConvoys_TownStoreDownFailsTheScan(t *testing.T) {
	townRoot := strandedXrigTown(t)
	down := func(townRoot string) (blockCheck, func(), error) {
		return openStrandedBlockCheckWith(townRoot, func(string) (beadsdk.Storage, error) {
			return nil, errors.New("dial tcp 127.0.0.1:3307: connection refused")
		})
	}

	stranded, err := findStrandedConvoysWith(townRoot, down)
	if err == nil {
		t.Fatalf("want an error when the town store will not open, got stranded %+v", stranded)
	}
	for _, want := range []string{"blocker check", "town beads store unavailable", "connection refused"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, s := range stranded {
		if len(s.ReadyIssues) != 0 {
			t.Errorf("convoy %s lists ready issues %v on a failed scan", s.ID, s.ReadyIssues)
		}
	}
}
