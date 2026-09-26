package beads

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestMatchesMRSourceIssue(t *testing.T) {
	tests := []struct {
		name        string
		description string
		issueID     string
		want        bool
	}{
		{
			name:        "exact match",
			description: "branch: polecat/furiosa/gt-abc@mm4heq3e\ntarget: main\nsource_issue: gt-abc\nrig: gastown\n",
			issueID:     "gt-abc",
			want:        true,
		},
		{
			name:        "no match different issue",
			description: "branch: polecat/furiosa/gt-xyz@mm4heq3e\ntarget: main\nsource_issue: gt-xyz\nrig: gastown\n",
			issueID:     "gt-abc",
			want:        false,
		},
		{
			name:        "partial ID must not match — prefix",
			description: "branch: polecat/nux/gt-abcdef@mm4heq3e\ntarget: main\nsource_issue: gt-abcdef\nrig: gastown\n",
			issueID:     "gt-abc",
			want:        false,
		},
		{
			name:        "partial ID must not match — suffix",
			description: "branch: polecat/nux/gt-abc@mm4heq3e\ntarget: main\nsource_issue: gt-abc\nrig: gastown\n",
			issueID:     "gt-abcdef",
			want:        false,
		},
		{
			name:        "match with worker field after source_issue",
			description: "branch: polecat/furiosa/la-cagb2@mm4heq3e\ntarget: main\nsource_issue: la-cagb2\nworker: polecats/furiosa\n",
			issueID:     "la-cagb2",
			want:        true,
		},
		{
			name:        "source_issue at end of description (with trailing newline)",
			description: "branch: fix/thing\nsource_issue: gt-99\n",
			issueID:     "gt-99",
			want:        true,
		},
		{
			name:        "source_issue at end without trailing newline — no match",
			description: "branch: fix/thing\nsource_issue: gt-99",
			issueID:     "gt-99",
			want:        false,
		},
		{
			name:        "empty description",
			description: "",
			issueID:     "gt-abc",
			want:        false,
		},
		{
			name:        "empty issue ID",
			description: "source_issue: gt-abc\n",
			issueID:     "",
			want:        false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchesMRSourceIssue(tt.description, tt.issueID)
			if got != tt.want {
				t.Errorf("MatchesMRSourceIssue(%q, %q) = %v, want %v",
					tt.description, tt.issueID, got, tt.want)
			}
		})
	}
}

func TestUnresolvedBlockingDependencyIDs(t *testing.T) {
	tests := []struct {
		name string
		deps []IssueDep
		want []string
	}{
		{
			name: "open blocks dependency blocks",
			deps: []IssueDep{{ID: "gt-blocker", Status: "open", DependencyType: "blocks"}},
			want: []string{"gt-blocker"},
		},
		{
			name: "blocking types match ready-work semantics",
			deps: []IssueDep{
				{ID: "gt-conditional", Status: "open", DependencyType: "conditional-blocks"},
				{ID: "gt-waits", Status: "open", DependencyType: "waits-for"},
				{ID: "gt-merge", Status: "open", DependencyType: "merge-blocks"},
			},
			want: []string{"gt-conditional", "gt-waits", "gt-merge"},
		},
		{
			name: "closed and tombstone dependencies are resolved",
			deps: []IssueDep{
				{ID: "gt-closed", Status: "closed", DependencyType: "blocks"},
				{ID: "gt-tombstone", Status: "tombstone", DependencyType: "blocks"},
				{ID: "gt-pinned", Status: "pinned", DependencyType: "blocks"},
			},
		},
		{
			name: "merge-blocks requires merged close reason",
			deps: []IssueDep{
				{ID: "gt-closed-only", Status: "closed", DependencyType: "merge-blocks"},
				{ID: "gt-merged", Status: "closed", DependencyType: "merge-blocks", CloseReason: "Merged in abc123"},
			},
			want: []string{"gt-closed-only"},
		},
		{
			name: "nonblocking dependency types do not block",
			deps: []IssueDep{
				{ID: "gt-empty", Status: "open"},
				{ID: "gt-parent", Status: "open", DependencyType: "parent-child"},
				{ID: "gt-track", Status: "open", DependencyType: "tracks"},
				{ID: "gt-related", Status: "open", DependencyType: "related"},
				{ID: "gt-custom", Status: "open", DependencyType: "custom-link"},
			},
		},
		{
			name: "external dependency IDs are normalized",
			deps: []IssueDep{{ID: "external:gt:gt-blocker", Status: "open", DependencyType: "blocks"}},
			want: []string{"gt-blocker"},
		},
		{
			name: "missing status fails closed",
			deps: []IssueDep{{ID: "gt-unknown-status", DependencyType: "blocks"}},
			want: []string{"gt-unknown-status"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := unresolvedBlockingDependencyIDs(&Issue{Dependencies: tt.deps})
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("unresolvedBlockingDependencyIDs() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestIssueDepUnmarshalRelationTypeFallbackFeedsBlockerSemantics(t *testing.T) {
	issue := unmarshalIssueForTest(t, `{
		"id":"gt-target",
		"status":"open",
		"issue_type":"task",
		"dependencies":[
			{"id":"gt-blocks","status":"open","issue_type":"task","type":"blocks"},
			{"id":"gt-conditional","status":"open","issue_type":"task","type":"conditional-blocks"},
			{"id":"gt-waits","status":"open","issue_type":"task","type":"waits-for"},
			{"id":"gt-merge","status":"open","issue_type":"task","type":"merge-blocks"},
			{"id":"gt-task","status":"open","issue_type":"task","type":"task"}
		]
	}`)

	got, _ := unresolvedBlockingDependencyIDs(issue)
	want := []string{"gt-blocks", "gt-conditional", "gt-waits", "gt-merge"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unresolvedBlockingDependencyIDs() = %#v, want %#v", got, want)
	}
	if dep := issue.Dependencies[4]; dep.DependencyType != "" {
		t.Fatalf("unknown fallback type populated DependencyType = %q, want empty", dep.DependencyType)
	}
}

func TestIssueDepUnmarshalDependencyTypeTakesPrecedenceOverType(t *testing.T) {
	issue := unmarshalIssueForTest(t, `{
		"id":"gt-target",
		"status":"open",
		"issue_type":"task",
		"dependencies":[
			{"id":"gt-canonical-blocks","status":"open","issue_type":"task","dependency_type":"blocks","type":"tracks"},
			{"id":"gt-canonical-tracks","status":"open","issue_type":"task","dependency_type":"tracks","type":"blocks"}
		]
	}`)

	if got := issue.Dependencies[0].DependencyType; got != "blocks" {
		t.Fatalf("DependencyType with canonical blocks = %q, want blocks", got)
	}
	if got := issue.Dependencies[1].DependencyType; got != "tracks" {
		t.Fatalf("DependencyType with canonical tracks = %q, want tracks", got)
	}
	if got := HasUnresolvedBlockers(issue); !got {
		t.Fatal("canonical blocks dependency should still block")
	}
	if got := FirstUnresolvedBlockerID(issue); got != "gt-canonical-blocks" {
		t.Fatalf("FirstUnresolvedBlockerID() = %q, want gt-canonical-blocks", got)
	}
}

func TestIssueDepUnmarshalPreservesIssueTypeSeparatelyFromRelationType(t *testing.T) {
	issue := unmarshalIssueForTest(t, `{
		"id":"gt-target",
		"status":"open",
		"issue_type":"task",
		"dependencies":[
			{"id":"gt-blocker","status":"open","issue_type":"bug","type":"blocks"}
		]
	}`)

	dep := issue.Dependencies[0]
	if dep.Type != "bug" {
		t.Fatalf("IssueDep.Type = %q, want issue_type bug", dep.Type)
	}
	if dep.DependencyType != "blocks" {
		t.Fatalf("IssueDep.DependencyType = %q, want blocks", dep.DependencyType)
	}
}

func TestIssueDepUnmarshalPreservesNonblockingRelationTypes(t *testing.T) {
	issue := unmarshalIssueForTest(t, `{
		"id":"gt-target",
		"status":"open",
		"issue_type":"task",
		"dependencies":[
			{"id":"gt-track","status":"open","issue_type":"task","type":"tracks"},
			{"id":"gt-parent","status":"open","issue_type":"task","type":"parent-child"},
			{"id":"gt-related","status":"open","issue_type":"task","type":"related"},
			{"id":"gt-discovered","status":"open","issue_type":"task","type":"discovered-from"},
			{"id":"gt-thread","status":"open","issue_type":"task","type":"thread"}
		]
	}`)

	wantTypes := []string{"tracks", "parent-child", "related", "discovered-from", "thread"}
	for i, want := range wantTypes {
		if got := issue.Dependencies[i].DependencyType; got != want {
			t.Fatalf("Dependencies[%d].DependencyType = %q, want %q", i, got, want)
		}
	}
	if HasUnresolvedBlockers(issue) {
		t.Fatalf("nonblocking fallback relations should not block: %#v", issue.Dependencies)
	}
}

func TestIssueDepUnmarshalMergeBlocksRequireMergedCloseReason(t *testing.T) {
	issue := unmarshalIssueForTest(t, `{
		"id":"gt-target",
		"status":"open",
		"issue_type":"task",
		"dependencies":[
			{"id":"gt-open","status":"open","issue_type":"task","type":"merge-blocks"},
			{"id":"gt-closed","status":"closed","issue_type":"task","type":"merge-blocks"},
			{"id":"gt-merged","status":"closed","issue_type":"task","type":"merge-blocks","close_reason":"Merged in abc123"}
		]
	}`)

	got, _ := unresolvedBlockingDependencyIDs(issue)
	want := []string{"gt-open", "gt-closed"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("unresolvedBlockingDependencyIDs() = %#v, want %#v", got, want)
	}
}

func unmarshalIssueForTest(t *testing.T, data string) *Issue {
	t.Helper()
	var issue Issue
	if err := json.Unmarshal([]byte(data), &issue); err != nil {
		t.Fatalf("json.Unmarshal(Issue): %v", err)
	}
	return &issue
}

func TestHasUnresolvedBlockersFallsBackToListFields(t *testing.T) {
	if !HasUnresolvedBlockers(&Issue{BlockedByCount: 1}) {
		t.Fatal("BlockedByCount fallback should block when detailed dependencies are absent")
	}
	if !HasUnresolvedBlockers(&Issue{DependencyCount: 1}) {
		t.Fatal("DependencyCount fallback should fail closed when detailed dependencies are absent")
	}
	if got := FirstUnresolvedBlockerID(&Issue{DependencyCount: 1}); got != "" {
		t.Fatalf("FirstUnresolvedBlockerID() = %q, want empty when only count is available", got)
	}
	if got := FirstUnresolvedBlockerID(&Issue{BlockedBy: []string{"external:gt:gt-blocker"}}); got != "gt-blocker" {
		t.Fatalf("FirstUnresolvedBlockerID() = %q, want gt-blocker", got)
	}
	if HasUnresolvedBlockers(&Issue{Dependencies: []IssueDep{{ID: "gt-closed", Status: "closed", DependencyType: "blocks"}}, BlockedByCount: 1}) {
		t.Fatal("detailed closed dependency should override stale list blocker count")
	}
}

func TestListMergeRequestsHydratesWispMRBlockers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	installListMergeRequestsBDStub(t, false)

	b := New(t.TempDir())
	issues, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "open", Priority: -1})
	if err != nil {
		t.Fatalf("ListMergeRequests() error = %v", err)
	}
	if len(issues) != 1 {
		t.Fatalf("ListMergeRequests() returned %d issues, want 1", len(issues))
	}

	issue := issues[0]
	if !issue.Ephemeral {
		t.Fatal("hydrated wisp MR should preserve Ephemeral=true")
	}
	if !HasUnresolvedBlockers(issue) {
		t.Fatalf("hydrated MR should be blocked: %#v", issue)
	}
	if got := FirstUnresolvedBlockerID(issue); got != "gt-blocker" {
		t.Fatalf("FirstUnresolvedBlockerID() = %q, want gt-blocker", got)
	}
	if issue.BlockedByCount != 1 {
		t.Fatalf("BlockedByCount = %d, want 1", issue.BlockedByCount)
	}
	if len(issue.Dependencies) != 1 {
		t.Fatalf("Dependencies len = %d, want 1", len(issue.Dependencies))
	}
}

func TestListMergeRequestsReturnsHydrationError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	installListMergeRequestsBDStub(t, true)

	b := New(t.TempDir())
	_, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "open", Priority: -1})
	if err == nil {
		t.Fatal("ListMergeRequests() error = nil, want hydration error")
	}
	if !strings.Contains(err.Error(), "hydrating merge-request dependencies") {
		t.Fatalf("ListMergeRequests() error = %v, want hydration context", err)
	}
}

func TestListMergeRequestsFiltersRigBeforeHydration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	installListMergeRequestsRigFilterBDStub(t)

	b := New(t.TempDir())
	issues, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "open", Priority: -1, Rig: "gastown"})
	if err != nil {
		t.Fatalf("ListMergeRequests() error = %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "gt-current" {
		t.Fatalf("ListMergeRequests() = %#v, want only gt-current", issues)
	}
}

func installListMergeRequestsBDStub(t *testing.T, failShow bool) {
	t.Helper()
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	binDir := t.TempDir()
	showCase := `
    printf '%s\n' '[{"id":"gt-wisp-mr","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","ephemeral":true,"labels":["gt:merge-request"],"dependencies":[{"id":"gt-blocker","title":"Blocker","status":"open","priority":1,"issue_type":"task","dependency_type":"blocks"}],"dependency_count":1}]'
    exit 0
`
	if failShow {
		showCase = `
    echo 'show failed' >&2
    exit 2
`
	}

	script := `#!/bin/sh
if [ "${1:-}" = "--allow-stale" ]; then
  if [ "${2:-}" = "version" ]; then
    echo "Error: unknown flag: --allow-stale" >&2
    exit 0
  fi
  shift
fi
case "${1:-}" in
  list)
    printf '%s\n' '[]'
    exit 0
    ;;
  sql)
    printf '%s\n' '[{"id":"gt-wisp-mr","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":"gt:merge-request"}]'
    exit 0
    ;;
  show)` + showCase + `
    ;;
  *)
    printf '%s\n' '[]'
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func installListMergeRequestsRigFilterBDStub(t *testing.T) {
	t.Helper()
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	binDir := t.TempDir()
	script := `#!/bin/sh
if [ "${1:-}" = "--allow-stale" ]; then
  if [ "${2:-}" = "version" ]; then
    echo "Error: unknown flag: --allow-stale" >&2
    exit 0
  fi
  shift
fi
case "${1:-}" in
  list)
    printf '%s\n' '[]'
    exit 0
    ;;
  sql)
    printf '%s\n' '[{"id":"gt-current","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":"gt:merge-request"},{"id":"gt-other","title":"Merge: gt-other","description":"branch: polecat/test/gt-other@abc\ntarget: main\nsource_issue: gt-other-source\nrig: other-rig\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":"gt:merge-request"}]'
    exit 0
    ;;
  show)
    case "$*" in
      *gt-other*) echo 'other rig should not be hydrated' >&2; exit 7 ;;
    esac
    printf '%s\n' '[{"id":"gt-current","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","ephemeral":true,"labels":["gt:merge-request"]}]'
    exit 0
    ;;
  *)
    printf '%s\n' '[]'
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// installLabeledWispsBDStub returns three wisps from "bd sql": one agent wisp,
// one merge-request wisp, and one carrying no labels at all, so a single
// PreloadLabeledWisps("gt:agent", "gt:merge-request") call can warm both
// caches at once. It also logs every invocation, so a test can assert
// PreloadLabeledWisps costs exactly one bd sql round trip and that a
// subsequent ListAgentBeadsFromWisps / ListMergeRequests reads the cache
// instead of spawning its own (gt-92zx).
//
// The unlabeled wisp is the regression case for the editorial rejection on
// gt-92zx: it is an agent bead by ID pattern only, so it is invisible to any
// label-filtered query and must still surface through the preloaded snapshot.
func installLabeledWispsBDStub(t *testing.T) string {
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
  list)
    printf '%s\n' '[]'
    exit 0
    ;;
  sql)
    printf '%s\n' '[{"id":"gt-om-witness","title":"witness agent","description":"role: witness\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":"gt:agent"},{"id":"gt-wisp-mr","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":"gt:merge-request"},{"id":"gt-gastown-polecat-bare","title":"polecat bead with no label metadata","description":"","status":"open","priority":2,"assignee":"","created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","created_by":"tester","labels_csv":""}]'
    exit 0
    ;;
  show)
    printf '%s\n' '[{"id":"gt-wisp-mr","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","ephemeral":true,"labels":["gt:merge-request"]}]'
    exit 0
    ;;
  "mol")
    echo 'ListAgentBeadsFromWisps should have used the preloaded cache' >&2
    exit 7
    ;;
  *)
    printf '%s\n' '[]'
    exit 0
    ;;
esac
`
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// TestPreloadLabeledWispsSingleQuery covers the gt-92zx fix: warming both the
// agent and merge-request wisp caches costs one bd sql round trip, not one
// per label.
func TestPreloadLabeledWispsSingleQuery(t *testing.T) {
	logPath := installLabeledWispsBDStub(t)

	b := New(t.TempDir())
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		t.Fatalf("PreloadLabeledWisps() error = %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	logOutput := string(logData)
	if count := strings.Count(logOutput, "sql --json"); count != 1 {
		t.Fatalf("sql --json count = %d, want 1\nlog:\n%s", count, logOutput)
	}
}

// TestPreloadLabeledWispsQueryIsUnfiltered: the preload must read the whole
// wisps table, not a label-filtered subset. ListAgentBeadsFromWisps runs its
// type/ID fallbacks over whatever the preload read, so a `WHERE l.label IN`
// here would make those fallbacks unreachable for exactly the wisps that need
// them and silently drop a live polecat from the listing — the regression the
// editorial gate caught on the first attempt (gt-92zx). Asserting on the
// query text is the point: the behavior it protects is an absence, which the
// stub's happy data cannot show.
func TestPreloadLabeledWispsQueryIsUnfiltered(t *testing.T) {
	logPath := installLabeledWispsBDStub(t)

	b := New(t.TempDir())
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		t.Fatalf("PreloadLabeledWisps() error = %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	logOutput := string(logData)
	if strings.Contains(logOutput, "WHERE l.label IN") {
		t.Fatalf("PreloadLabeledWisps filtered its query by label; the preloaded snapshot must be the full wisps read so ListAgentBeadsFromWisps's fallbacks still see unlabeled wisps\nlog:\n%s", logOutput)
	}
	if strings.Contains(logOutput, "'gt:agent'") || strings.Contains(logOutput, "'gt:merge-request'") {
		t.Fatalf("PreloadLabeledWisps pushed a label literal into SQL; bucketing belongs in Go so one read serves both consumers\nlog:\n%s", logOutput)
	}
}

// TestListMergeRequestsUsesPreloadedWispCache covers the gt-92zx fix: once
// PreloadLabeledWisps has warmed the cache, ListMergeRequests reads it
// instead of running its own "bd sql" query.
func TestListMergeRequestsUsesPreloadedWispCache(t *testing.T) {
	logPath := installLabeledWispsBDStub(t)

	b := New(t.TempDir())
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		t.Fatalf("PreloadLabeledWisps() error = %v", err)
	}

	issues, err := b.ListMergeRequests(ListOptions{Label: "gt:merge-request", Status: "all", Priority: -1})
	if err != nil {
		t.Fatalf("ListMergeRequests() error = %v", err)
	}
	if len(issues) != 1 || issues[0].ID != "gt-wisp-mr" {
		t.Fatalf("ListMergeRequests() = %#v, want only gt-wisp-mr", issues)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read bd log: %v", err)
	}
	if count := strings.Count(string(logData), "sql --json"); count != 1 {
		t.Fatalf("sql --json count = %d, want 1 (the preload's, none from ListMergeRequests)\nlog:\n%s", count, logData)
	}
}

// TestListAgentBeadsFromWispsUsesPreloadedCache covers the gt-92zx fix: once
// PreloadLabeledWisps has warmed the cache, ListAgentBeadsFromWisps reads it
// instead of running its own "bd mol wisp list" (the stub fails the test if
// that subcommand is invoked). The unlabeled wisp must still come back — a
// cached run has to classify the same wisps an uncached one would (gt-92zx,
// editorial rejection attempt 1).
func TestListAgentBeadsFromWispsUsesPreloadedCache(t *testing.T) {
	installLabeledWispsBDStub(t)

	b := New(t.TempDir())
	if err := b.PreloadLabeledWisps("gt:agent", "gt:merge-request"); err != nil {
		t.Fatalf("PreloadLabeledWisps() error = %v", err)
	}

	agents, err := b.ListAgentBeadsFromWisps()
	if err != nil {
		t.Fatalf("ListAgentBeadsFromWisps() error = %v", err)
	}
	if agent, ok := agents["gt-om-witness"]; !ok || agent.ID != "gt-om-witness" {
		t.Fatalf("ListAgentBeadsFromWisps() = %#v, want gt-om-witness", agents)
	}
	if _, ok := agents["gt-gastown-polecat-bare"]; !ok {
		t.Fatalf("ListAgentBeadsFromWisps() dropped the wisp that is an agent bead by ID pattern only — the preloaded snapshot must be the unfiltered wisps read: %#v", agents)
	}
	if _, ok := agents["gt-wisp-mr"]; ok {
		t.Fatalf("ListAgentBeadsFromWisps() should not include the merge-request wisp: %#v", agents)
	}
}
