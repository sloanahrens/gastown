package beads

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// failingBDStub installs a `bd` on PATH that errors on every invocation, so a
// test using it proves the code path under test never shells out at all.
func failingBDStub(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	binDir := t.TempDir()
	script := "#!/bin/sh\necho 'unexpected bd invocation: '\"$*\" >&2\nexit 1\n"
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestFindMRForBranchAnyUsesPreloadedCache is the gt-b839 cache-hit case: once
// PreloadMergeRequests (or, here, a directly-set cache) has warmed mrCache,
// FindMRForBranchAny must answer from memory rather than repeating the
// full-table `bd list --label=gt:merge-request` scan.
func TestFindMRForBranchAnyUsesPreloadedCache(t *testing.T) {
	failingBDStub(t)

	mr := &Issue{ID: "gt-wisp-mr", Status: "open", Description: "branch: polecat/test/gt-source@abc\nsource_issue: gt-source\n"}
	b := NewIsolated(t.TempDir())
	b.mrCache = &mrCacheState{loaded: true, issues: []*Issue{mr}}

	got, err := b.FindMRForBranchAny("polecat/test/gt-source@abc")
	if err != nil {
		t.Fatalf("FindMRForBranchAny() error = %v (should never reach bd)", err)
	}
	if got == nil || got.ID != mr.ID {
		t.Fatalf("FindMRForBranchAny() = %#v, want %#v", got, mr)
	}

	if got, err := b.FindMRForBranchAny("no/such/branch"); err != nil || got != nil {
		t.Fatalf("FindMRForBranchAny(no/such/branch) = (%#v, %v), want (nil, nil)", got, err)
	}
}

// TestFindMRForBranchAnyCacheWarmedWithZeroResults proves a rig with zero
// merge-request beads (a legitimately empty preload) still reads as "warmed"
// rather than "never warmed" — see mergeRequestsForBranchSearch's loaded gate.
func TestFindMRForBranchAnyCacheWarmedWithZeroResults(t *testing.T) {
	failingBDStub(t)

	b := NewIsolated(t.TempDir())
	b.mrCache = &mrCacheState{loaded: true, issues: nil}

	got, err := b.FindMRForBranchAny("any/branch")
	if err != nil {
		t.Fatalf("FindMRForBranchAny() error = %v (should never reach bd)", err)
	}
	if got != nil {
		t.Fatalf("FindMRForBranchAny() = %#v, want nil", got)
	}
}

// TestGetAgentBeadUsesPreloadedCache is the agent-bead half of the same
// gt-b839 cache-hit case.
func TestGetAgentBeadUsesPreloadedCache(t *testing.T) {
	failingBDStub(t)

	agentIssue := &Issue{ID: "gt-gastown-polecat-nux", Type: "agent", Labels: []string{"gt:agent"}, Description: "role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null"}
	b := NewIsolated(t.TempDir())
	b.agentBeadCache = map[string]*Issue{agentIssue.ID: agentIssue}

	issue, fields, err := b.GetAgentBead(agentIssue.ID)
	if err != nil {
		t.Fatalf("GetAgentBead() error = %v (should never reach bd)", err)
	}
	if issue == nil || issue.ID != agentIssue.ID {
		t.Fatalf("GetAgentBead() issue = %#v, want %#v", issue, agentIssue)
	}
	if fields == nil || fields.RoleType != "polecat" {
		t.Fatalf("GetAgentBead() fields = %#v, want role_type=polecat", fields)
	}
}

// TestGetAgentBeadCacheMissFallsBackToShow covers the documented degrade:
// ListAgentBeads (what warms the cache) only returns open beads plus wisps,
// so an ID absent from a warmed cache must still resolve via Show(id) rather
// than being reported as missing outright.
func TestGetAgentBeadCacheMissFallsBackToShow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	installMockBDFixedShowOutput(t, `[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null"}]`)

	// Cache is warmed (non-nil) but doesn't contain this ID — e.g. the agent
	// bead was closed and so didn't come back from ListAgentBeads' open-only
	// query.
	b := NewIsolated(t.TempDir())
	b.agentBeadCache = map[string]*Issue{}

	issue, fields, err := b.GetAgentBead("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("GetAgentBead() error = %v", err)
	}
	if issue == nil || issue.ID != "gt-gastown-polecat-nux" {
		t.Fatalf("GetAgentBead() issue = %#v, want the Show()-resolved bead", issue)
	}
	if fields == nil || fields.RoleType != "polecat" {
		t.Fatalf("GetAgentBead() fields = %#v, want role_type=polecat", fields)
	}
}

// TestPreloadMergeRequestsWarmsCacheForSubsequentLookups is the end-to-end
// wiring test: PreloadMergeRequests issues its one bulk fetch, and every
// FindMRForBranchAny call afterward — as check-recovery-batch makes one per
// polecat in the rig — answers from that same fetch instead of repeating it.
func TestPreloadMergeRequestsWarmsCacheForSubsequentLookups(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	logPath := installLoggingListMergeRequestsBDStub(t)

	b := New(t.TempDir())
	if err := b.PreloadMergeRequests(); err != nil {
		t.Fatalf("PreloadMergeRequests() error = %v", err)
	}

	callsAfterPreload := bdCallCount(t, logPath)
	if callsAfterPreload == 0 {
		t.Fatal("PreloadMergeRequests() made no bd calls at all")
	}

	mr, err := b.FindMRForBranchAny("polecat/test/gt-source@abc")
	if err != nil {
		t.Fatalf("FindMRForBranchAny() error = %v", err)
	}
	if mr == nil || mr.ID != "gt-wisp-mr" {
		t.Fatalf("FindMRForBranchAny() = %#v, want gt-wisp-mr", mr)
	}

	// A second polecat's branch lookup in the same batch sweep must not
	// trigger any further bd calls — that's the whole point of preloading.
	if _, err := b.FindMRForBranchAny("some/other/branch"); err != nil {
		t.Fatalf("FindMRForBranchAny() (second lookup) error = %v", err)
	}

	if got := bdCallCount(t, logPath); got != callsAfterPreload {
		t.Fatalf("FindMRForBranchAny() issued %d more bd call(s) after PreloadMergeRequests warmed the cache", got-callsAfterPreload)
	}
}

// TestPreloadAgentBeadsWarmsCacheForSubsequentLookups mirrors the merge-request
// wiring test above for agent beads.
func TestPreloadAgentBeadsWarmsCacheForSubsequentLookups(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	logPath := installLoggingListAgentBeadsBDStub(t)

	b := New(t.TempDir())
	if err := b.PreloadAgentBeads(); err != nil {
		t.Fatalf("PreloadAgentBeads() error = %v", err)
	}

	callsAfterPreload := bdCallCount(t, logPath)
	if callsAfterPreload == 0 {
		t.Fatal("PreloadAgentBeads() made no bd calls at all")
	}

	_, fields, err := b.GetAgentBead("gt-gastown-polecat-nux")
	if err != nil {
		t.Fatalf("GetAgentBead() error = %v", err)
	}
	if fields == nil || fields.RoleType != "polecat" {
		t.Fatalf("GetAgentBead() fields = %#v, want role_type=polecat", fields)
	}

	if _, _, err := b.GetAgentBead("gt-gastown-polecat-amber"); err != nil {
		t.Fatalf("GetAgentBead() (second polecat's lookup) error = %v", err)
	}

	if got := bdCallCount(t, logPath); got != callsAfterPreload {
		t.Fatalf("GetAgentBead() issued %d more bd call(s) after PreloadAgentBeads warmed the cache", got-callsAfterPreload)
	}
}

func bdCallCount(t *testing.T, logPath string) int {
	t.Helper()
	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read bd log: %v", err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return 0
	}
	return len(strings.Split(trimmed, "\n"))
}

// installLoggingListMergeRequestsBDStub answers ListMergeRequests' two-call
// sequence (list, then sql) with a single MR bead, logging every invocation.
func installLoggingListMergeRequestsBDStub(t *testing.T) string {
	t.Helper()
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + shellQuote(logPath) + `
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
  show)
    printf '%s\n' '[{"id":"gt-wisp-mr","title":"Merge: gt-source","description":"branch: polecat/test/gt-source@abc\ntarget: main\nsource_issue: gt-source\nrig: gastown\n","status":"open","priority":1,"created_at":"2026-06-29T00:00:00Z","updated_at":"2026-06-29T00:00:00Z","ephemeral":true,"labels":["gt:merge-request"]}]'
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
	return logPath
}

// installLoggingListAgentBeadsBDStub answers ListAgentBeads' two-call
// sequence (list --label=gt:agent, then mol wisp list) with two agent beads —
// standing in for two polecats in the same rig sweep — logging every
// invocation.
func installLoggingListAgentBeadsBDStub(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "bd.log")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + shellQuote(logPath) + `
case "${1:-}" in
  list)
    printf '%s\n' '[{"id":"gt-gastown-polecat-nux","title":"Polecat nux","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null"},{"id":"gt-gastown-polecat-amber","title":"Polecat amber","issue_type":"agent","labels":["gt:agent"],"description":"role_type: polecat\nrig: gastown\nagent_state: idle\nhook_bead: null"}]'
    exit 0
    ;;
  mol)
    printf '%s\n' '{"wisps":[]}'
    exit 0
    ;;
  *)
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

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
