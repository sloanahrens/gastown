package beads

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
)

// snapshotIssuesJSON is what the stub returns for the issues-table read: an
// agent bead, an open MR, a closed MR, an ephemeral MR, a hooked work bead, an
// ordinary open bead, and a closed agent bead.
const snapshotIssuesJSON = `[
{"id":"gt-gastown-polecat-a","title":"agent a","description":"role_type: polecat\nhook_bead: gt-work1\n","status":"open","priority":2,"issue_type":"task","assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T01:00:00Z","created_by":"mayor","ephemeral":0,"labels_csv":"gt:agent"},
{"id":"gt-mr-open","title":"Merge: open","description":"branch: b1\n","status":"open","priority":1,"issue_type":"task","assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"t","ephemeral":0,"labels_csv":"gt:merge-request"},
{"id":"gt-mr-closed","title":"Merge: closed","description":"branch: b2\n","status":"closed","priority":1,"issue_type":"task","assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"t","ephemeral":0,"labels_csv":"gt:merge-request"},
{"id":"gt-mr-eph","title":"Merge: ephemeral","description":"branch: b3\n","status":"open","priority":1,"issue_type":"task","assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"t","ephemeral":1,"labels_csv":"gt:merge-request"},
{"id":"gt-work1","title":"work","description":"","status":"hooked","priority":2,"issue_type":"bug","assignee":"gastown/polecats/a","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"t","ephemeral":0,"labels_csv":"ready-to-land,other"},
{"id":"gt-plain","title":"plain","description":"","status":"open","priority":3,"issue_type":"task","assignee":null,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"t","ephemeral":0,"labels_csv":""},
{"id":"gt-gastown-polecat-gone","title":"agent gone","description":"","status":"closed","priority":2,"issue_type":"task","assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"mayor","ephemeral":0,"labels_csv":"gt:agent"}
]`

// installIssueSnapshotBDStub answers the issues-table bd sql with
// snapshotIssuesJSON, the wisps-table one with no rows, and fails any other
// read the preloaded snapshots are supposed to make unnecessary — bd list,
// bd query, bd mol. Every invocation is logged.
func installIssueSnapshotBDStub(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
LOG_FILE='` + logPath + `'
printf '%s\n' "$*" >> "$LOG_FILE"
if [ "${1:-}" = "--allow-stale" ]; then
  if [ "${2:-}" = "version" ]; then
    echo "Error: unknown flag: --allow-stale" >&2
    exit 0
  fi
  shift
fi
case "${1:-}" in
  sql)
    case "$*" in
      *"FROM issues"*)
        cat <<'EOF'
` + snapshotIssuesJSON + `
EOF
        ;;
      *)
        printf '%s\n' '[]'
        ;;
    esac
    exit 0
    ;;
  show)
    printf '%s\n' '[]'
    exit 0
    ;;
  *)
    echo "unexpected bd read the preloaded snapshot should have answered: $*" >&2
    exit 7
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func preloadedIssueSnapshotBeads(t *testing.T) (*Beads, string) {
	t.Helper()
	logPath := installIssueSnapshotBDStub(t)
	b := New(t.TempDir())
	err := b.PreloadIssues(
		[]string{"gt:agent", "gt:merge-request"},
		[]IssueStatus{IssueStatusHooked, StatusInProgress, StatusOpen},
	)
	if err != nil {
		t.Fatalf("PreloadIssues() error = %v", err)
	}
	return b, logPath
}

func sortedIDs(issues []*Issue) []string {
	ids := make([]string, 0, len(issues))
	for _, issue := range issues {
		ids = append(ids, issue.ID)
	}
	sort.Strings(ids)
	return ids
}

// TestPreloadIssuesSingleQuery: warming the snapshot is one bd sql round trip
// over the issues table, and the query names both the labels and the statuses
// it must cover — a snapshot narrower than what the readers ask would answer
// them wrongly rather than fall through.
func TestPreloadIssuesSingleQuery(t *testing.T) {
	_, logPath := preloadedIssueSnapshotBeads(t)

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	log := string(logData)
	if count := strings.Count(log, "sql --json"); count != 1 {
		t.Fatalf("sql --json count = %d, want 1\nlog:\n%s", count, log)
	}
	for _, want := range []string{"FROM issues i", "'gt:agent'", "'gt:merge-request'", "i.status IN ('hooked', 'in_progress', 'open')"} {
		if !strings.Contains(log, want) {
			t.Errorf("query missing %q\nlog:\n%s", want, log)
		}
	}
}

// TestPreloadIssuesNothingToRead: no labels and no statuses is no query.
func TestPreloadIssuesNothingToRead(t *testing.T) {
	logPath := installIssueSnapshotBDStub(t)
	b := New(t.TempDir())
	if err := b.PreloadIssues(nil, nil); err != nil {
		t.Fatalf("PreloadIssues(nil, nil) error = %v", err)
	}
	if b.issueSnapshot != nil {
		t.Errorf("PreloadIssues(nil, nil) built a snapshot: %#v", b.issueSnapshot)
	}
	if data, _ := os.ReadFile(logPath); strings.Contains(string(data), "sql") {
		t.Errorf("PreloadIssues(nil, nil) ran a query:\n%s", data)
	}
}

// TestPreloadedIssuesAnswerEveryReader is the gt-0hmt2 fix end to end: with
// both snapshots warm, ListAgentBeads, ListMergeRequests and ListIssueStatuses
// spawn no bd process of their own. The stub fails bd list, bd query and
// bd mol, so any of them falling back is a test failure; the log then shows the
// two preload queries and nothing else.
func TestPreloadedIssuesAnswerEveryReader(t *testing.T) {
	b, logPath := preloadedIssueSnapshotBeads(t)
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		t.Fatalf("PreloadLabeledWisps() error = %v", err)
	}

	agents, err := b.ListAgentBeads()
	if err != nil {
		t.Fatalf("ListAgentBeads() error = %v", err)
	}
	if len(agents) != 1 || agents["gt-gastown-polecat-a"] == nil {
		t.Errorf("ListAgentBeads() = %v, want only gt-gastown-polecat-a (the closed agent bead is hidden, as bd list hides it)", agents)
	}

	// The MR issues half comes back from the snapshot; the hydration bd show
	// then finds none of them, which is the stub's doing and not under test.
	mrs, err := b.listIssues(ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
	if err != nil {
		t.Fatalf("listIssues(all) error = %v", err)
	}
	if got, want := sortedIDs(mrs), []string{"gt-mr-closed", "gt-mr-open"}; !reflect.DeepEqual(got, want) {
		t.Errorf("listIssues(all) = %v, want %v", got, want)
	}

	work, err := b.ListIssueStatuses(IssueStatusHooked, StatusInProgress, StatusOpen)
	if err != nil {
		t.Fatalf("ListIssueStatuses() error = %v", err)
	}
	if got, want := sortedIDs(work), []string{"gt-gastown-polecat-a", "gt-mr-open", "gt-plain", "gt-work1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListIssueStatuses() = %v, want %v", got, want)
	}

	logData, _ := os.ReadFile(logPath)
	if count := strings.Count(string(logData), "sql --json"); count != 2 {
		t.Errorf("sql --json count = %d, want 2 (the two preloads)\nlog:\n%s", count, logData)
	}
}

// TestIssueSnapshotListDefaults pins the two bd list defaults the snapshot
// reproduces: an empty status is "not closed", and ephemeral issues are hidden.
func TestIssueSnapshotListDefaults(t *testing.T) {
	b, _ := preloadedIssueSnapshotBeads(t)
	for _, tc := range []struct {
		status string
		want   []string
	}{
		{"", []string{"gt-mr-open"}},
		{"all", []string{"gt-mr-closed", "gt-mr-open"}},
		{"open", []string{"gt-mr-open"}},
		{"closed", []string{"gt-mr-closed"}},
	} {
		got, ok := b.issueSnapshot.list(ListOptions{Label: "gt:merge-request", Status: tc.status, Priority: -1})
		if !ok {
			t.Errorf("list(status %q) not answered from the snapshot", tc.status)
			continue
		}
		if !reflect.DeepEqual(sortedIDs(got), tc.want) {
			t.Errorf("list(status %q) = %v, want %v", tc.status, sortedIDs(got), tc.want)
		}
	}
}

// TestIssueSnapshotFallsThroughWhenNotCovered: a question outside what the
// snapshot read must be left to bd itself, never answered from a subset.
func TestIssueSnapshotFallsThroughWhenNotCovered(t *testing.T) {
	b, _ := preloadedIssueSnapshotBeads(t)
	s := b.issueSnapshot

	for name, opts := range map[string]ListOptions{
		"label not warmed": {Label: "gt:task", Priority: -1},
		"no label":         {Priority: -1},
		"priority filter":  {Label: "gt:merge-request", Priority: 1},
		"parent":           {Label: "gt:merge-request", Priority: -1, Parent: "gt-x"},
		"assignee":         {Label: "gt:merge-request", Priority: -1, Assignee: "a"},
		"no-assignee":      {Label: "gt:merge-request", Priority: -1, NoAssignee: true},
		"limit":            {Label: "gt:merge-request", Priority: -1, Limit: 5},
		"other status":     {Label: "gt:merge-request", Priority: -1, Status: "blocked"},
	} {
		if got, ok := s.list(opts); ok {
			t.Errorf("list(%s) answered from the snapshot: %v", name, sortedIDs(got))
		}
	}

	if got, ok := s.byStatus([]IssueStatus{StatusOpen, StatusBlocked}); ok {
		t.Errorf("byStatus(open, blocked) answered although blocked was not warmed: %v", sortedIDs(got))
	}

	var cold *issueSnapshot
	if _, ok := cold.list(ListOptions{Label: "gt:merge-request", Priority: -1}); ok {
		t.Error("nil snapshot answered list")
	}
	if _, ok := cold.agentBeads(); ok {
		t.Error("nil snapshot answered agentBeads")
	}
	if _, ok := cold.byStatus([]IssueStatus{StatusOpen}); ok {
		t.Error("nil snapshot answered byStatus")
	}

	agentless := &issueSnapshot{labels: map[string]bool{"gt:merge-request": true}}
	if _, ok := agentless.agentBeads(); ok {
		t.Error("snapshot not warmed for gt:agent answered agentBeads")
	}
}

// TestIssueSnapshotByStatusIsDurableOnly: bd query's `ephemeral=false` is part
// of ListIssueStatuses' contract, so the snapshot drops ephemeral rows.
func TestIssueSnapshotByStatusIsDurableOnly(t *testing.T) {
	b, _ := preloadedIssueSnapshotBeads(t)
	got, ok := b.issueSnapshot.byStatus([]IssueStatus{IssueStatusHooked, StatusInProgress, StatusOpen})
	if !ok {
		t.Fatal("byStatus not answered from the snapshot")
	}
	want := []string{"gt-gastown-polecat-a", "gt-mr-open", "gt-plain", "gt-work1"}
	if !reflect.DeepEqual(sortedIDs(got), want) {
		t.Errorf("byStatus() = %v, want %v", sortedIDs(got), want)
	}
}

// TestIssueSnapshotRowFields: the fields the polecat listing reads off these
// issues — description (agent fields), updated_at (spawn grace), labels
// (protected / submitted work), type, assignee, ephemeral — survive the SQL row.
func TestIssueSnapshotRowFields(t *testing.T) {
	b, _ := preloadedIssueSnapshotBeads(t)
	byID := map[string]*Issue{}
	for _, issue := range b.issueSnapshot.issues {
		byID[issue.ID] = issue
	}

	work := byID["gt-work1"]
	if work == nil {
		t.Fatal("gt-work1 missing from snapshot")
	}
	if work.Status != "hooked" || work.Type != "bug" || work.Assignee != "gastown/polecats/a" {
		t.Errorf("gt-work1 = %+v", work)
	}
	if !reflect.DeepEqual(work.Labels, []string{"ready-to-land", "other"}) {
		t.Errorf("gt-work1 labels = %v", work.Labels)
	}
	if work.Ephemeral {
		t.Error("gt-work1 read as ephemeral")
	}

	agent := byID["gt-gastown-polecat-a"]
	if agent.UpdatedAt != "2026-06-29T01:00:00Z" || !strings.Contains(agent.Description, "hook_bead: gt-work1") {
		t.Errorf("agent bead lost fields: %+v", agent)
	}
	if !byID["gt-mr-eph"].Ephemeral {
		t.Error("gt-mr-eph lost its ephemeral flag")
	}
	if plain := byID["gt-plain"]; plain.Assignee != "" || len(plain.Labels) != 0 {
		t.Errorf("gt-plain = %+v, want no assignee and no labels", plain)
	}
}
