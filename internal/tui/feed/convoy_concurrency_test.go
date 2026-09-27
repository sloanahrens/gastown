package feed

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// setupFakeBdForEnrich installs a fake `bd` binary that answers `dep list
// <convoyID> -t tracks --json` per-convoyID (from depByConvoy) and `show
// ... --json` with a single canned response (showJSON), logging every call
// to BD_TEST_LOG. Every invocation also bumps a shared counter (protected by
// an mkdir-based lock, atomic across processes) for the duration of a short
// sleep, recording the high-water mark to maxConcurrentPath — this is what
// lets tests assert "at most one bd child in flight at a time" (gt-05vk).
func setupFakeBdForEnrich(t *testing.T, depByConvoy map[string]string, showJSON string) (logPath, maxConcurrentPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd script uses /bin/sh, not available on Windows")
	}

	binDir := t.TempDir()
	logPath = filepath.Join(binDir, "bd-calls.log")
	counterPath := filepath.Join(binDir, "counter")
	lockPath := filepath.Join(binDir, "counter.lock")
	maxConcurrentPath = filepath.Join(binDir, "max-concurrent")

	var depCases strings.Builder
	for id, json := range depByConvoy {
		fmt.Fprintf(&depCases, "    %s) printf '%%s' '%s' ;;\n", id, json)
	}

	script := `#!/bin/sh
echo "ARGS:$*" >> "` + logPath + `"

# Atomic (mkdir-based) increment-then-decrement around a short sleep, so any
# real overlap between concurrent invocations shows up as counter > 1.
lock() { while ! mkdir "` + lockPath + `" 2>/dev/null; do sleep 0.01; done; }
unlock() { rmdir "` + lockPath + `"; }

lock
cur=$(cat "` + counterPath + `" 2>/dev/null || echo 0)
cur=$((cur+1))
echo "$cur" > "` + counterPath + `"
mx=$(cat "` + maxConcurrentPath + `" 2>/dev/null || echo 0)
if [ "$cur" -gt "$mx" ]; then echo "$cur" > "` + maxConcurrentPath + `"; fi
unlock

sleep 0.1

lock
cur=$(cat "` + counterPath + `" 2>/dev/null || echo 0)
cur=$((cur-1))
echo "$cur" > "` + counterPath + `"
unlock

cmd="$1"
case "$cmd" in
  dep)
    id="$3"
    case "$id" in
` + depCases.String() + `
      *) printf '[]' ;;
    esac
    exit 0
    ;;
  show)
    printf '%s' '` + showJSON + `'
    exit 0
    ;;
  *)
    echo '[]'
    exit 0
    ;;
esac
`
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath, maxConcurrentPath
}

func resetTrackedIDsCache() {
	trackedIDsCacheMu.Lock()
	trackedIDsCache = make(map[string]trackedIDsCacheEntry)
	trackedIDsCacheMu.Unlock()
}

func resetIssueStatusCache() {
	issueStatusCacheMu.Lock()
	issueStatusCache = make(map[string]string)
	issueStatusCacheMu.Unlock()
}

// setupFakeBdShowFailing installs a fake `bd` binary whose `show` subcommand
// always fails (non-zero exit), simulating the transient bd/Dolt failure
// that batchIssueStatus must tolerate. Other subcommands succeed with an
// empty list so trackedIssueIDs (if it ends up called) doesn't error out.
func setupFakeBdShowFailing(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake bd script uses /bin/sh, not available on Windows")
	}

	binDir := t.TempDir()
	script := `#!/bin/sh
cmd="$1"
case "$cmd" in
  show)
    exit 1
    ;;
  *)
    echo '[]'
    exit 0
    ;;
esac
`
	bdPath := filepath.Join(binDir, "bd")
	if err := os.WriteFile(bdPath, []byte(script), 0755); err != nil { //nolint:gosec // test fixture
		t.Fatalf("write fake bd: %v", err)
	}

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func readMaxConcurrent(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // test fixture, path constructed by test
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("reading max-concurrent file: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing max-concurrent value %q: %v", data, err)
	}
	return n
}

// TestEnrichConvoys_AtMostOneBdChildAtATime is the direct acceptance-criteria
// check for gt-05vk: gt feed must hold <=1 concurrent bd child. A prior fix
// (gt-3ony) fanned per-convoy bd calls out across up to 4 goroutines, which
// is exactly the concurrent-bd-child pileup that starved every other bd
// caller town-wide. This test fails if that fan-out ever comes back.
func TestEnrichConvoys_AtMostOneBdChildAtATime(t *testing.T) {
	resetTrackedIDsCache()
	items := []convoyListItem{
		{ID: "hq-cv-aaa1"}, {ID: "hq-cv-aaa2"}, {ID: "hq-cv-aaa3"}, {ID: "hq-cv-aaa4"},
	}
	depByConvoy := make(map[string]string, len(items))
	for _, item := range items {
		depByConvoy[item.ID] = `[]`
	}
	_, maxConcurrentPath := setupFakeBdForEnrich(t, depByConvoy, `[]`)

	enrichConvoys(t.TempDir(), items)

	if got := readMaxConcurrent(t, maxConcurrentPath); got > 1 {
		t.Fatalf("enrichConvoys() ran %d bd children concurrently, want <=1", got)
	}
}

// TestTrackedIssueIDs_CachesWithinTTL asserts a second lookup for the same
// convoy within the cache TTL does not spawn another `bd dep list` — convoy
// membership rarely changes, so re-deriving it on every 10s tick was pure
// waste (gt-05vk).
func TestTrackedIssueIDs_CachesWithinTTL(t *testing.T) {
	resetTrackedIDsCache()
	logPath, _ := setupFakeBdForEnrich(t, map[string]string{
		"hq-cv-bbb1": `[{"id":"gt-1","status":"open"}]`,
	}, `[]`)

	beadsDir := t.TempDir()
	first := trackedIssueIDs(beadsDir, "hq-cv-bbb1")
	second := trackedIssueIDs(beadsDir, "hq-cv-bbb1")

	if len(first) != 1 || first[0] != "gt-1" {
		t.Fatalf("trackedIssueIDs() first call = %v, want [gt-1]", first)
	}
	if len(second) != 1 || second[0] != "gt-1" {
		t.Fatalf("trackedIssueIDs() cached call = %v, want [gt-1]", second)
	}

	lines := readLogLines(t, logPath)
	depCalls := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "ARGS:dep ") {
			depCalls++
		}
	}
	if depCalls != 1 {
		t.Fatalf("expected exactly 1 `bd dep list` call across 2 lookups within TTL, got %d: %v", depCalls, lines)
	}
}

// TestEnrichConvoys_BatchesStatusIntoOneBdShowCall asserts that resolving
// tracked-issue status for multiple convoys costs exactly one `bd show`
// call, not one per convoy — the "one batched query, not N concurrent bd
// dep list calls per convoy" fix for gt-05vk.
func TestEnrichConvoys_BatchesStatusIntoOneBdShowCall(t *testing.T) {
	resetTrackedIDsCache()
	items := []convoyListItem{
		{ID: "hq-cv-ccc1"},
		{ID: "hq-cv-ccc2"},
		{ID: "hq-cv-ccc3"},
	}
	depByConvoy := map[string]string{
		"hq-cv-ccc1": `[{"id":"gt-1","status":"open"}]`,
		"hq-cv-ccc2": `[{"id":"gt-2","status":"open"}]`,
		"hq-cv-ccc3": `[]`,
	}
	showJSON := `[{"id":"gt-1","status":"closed"},{"id":"gt-2","status":"open"}]`
	logPath, _ := setupFakeBdForEnrich(t, depByConvoy, showJSON)

	result := enrichConvoys(t.TempDir(), items)

	if len(result) != 3 {
		t.Fatalf("enrichConvoys() returned %d convoys, want 3", len(result))
	}
	if result[0].Total != 1 || result[0].Completed != 1 {
		t.Errorf("convoy hq-cv-ccc1 = %+v, want Total=1 Completed=1 (gt-1 closed)", result[0])
	}
	if result[1].Total != 1 || result[1].Completed != 0 {
		t.Errorf("convoy hq-cv-ccc2 = %+v, want Total=1 Completed=0 (gt-2 open)", result[1])
	}

	lines := readLogLines(t, logPath)
	showCalls := 0
	for _, line := range lines {
		if strings.HasPrefix(line, "ARGS:show ") {
			showCalls++
			if !strings.Contains(line, "gt-1") || !strings.Contains(line, "gt-2") {
				t.Errorf("expected the single bd show call to cover both tracked IDs, got: %s", line)
			}
		}
	}
	if showCalls != 1 {
		t.Fatalf("expected exactly 1 `bd show` call for 3 convoys, got %d: %v", showCalls, lines)
	}
}

// TestBatchIssueStatus_FallsBackToCacheOnBdShowFailure is the regression
// test for gt-7g8i. Once enrichConvoys batches every convoy's tracked
// issues into a single `bd show` call (gt-05vk), a single failure of that
// call — one bad ID, a transient Dolt hiccup — used to zero out Completed
// for every convoy in the town at once, not just the one convoy affected.
// batchIssueStatus must fall back to each issue's last known status
// instead of returning an empty map on failure.
func TestBatchIssueStatus_FallsBackToCacheOnBdShowFailure(t *testing.T) {
	resetTrackedIDsCache()
	resetIssueStatusCache()

	items := []convoyListItem{
		{ID: "hq-cv-eee1"},
		{ID: "hq-cv-eee2"},
	}
	depByConvoy := map[string]string{
		"hq-cv-eee1": `[{"id":"gt-1"}]`,
		"hq-cv-eee2": `[{"id":"gt-2"}]`,
	}
	showJSON := `[{"id":"gt-1","status":"closed"},{"id":"gt-2","status":"open"}]`
	setupFakeBdForEnrich(t, depByConvoy, showJSON)

	first := enrichConvoys(t.TempDir(), items)
	if first[0].Completed != 1 || first[1].Completed != 0 {
		t.Fatalf("initial enrichConvoys() = %+v, want convoy 1 Completed=1 (gt-1 closed), convoy 2 Completed=0 (gt-2 open)", first)
	}

	// Simulate a Dolt hiccup on the next poll tick: `bd show` now fails.
	// trackedIssueIDs still serves gt-1/gt-2 from its own warm cache, so
	// only batchIssueStatus's error path is exercised here.
	setupFakeBdShowFailing(t)

	second := enrichConvoys(t.TempDir(), items)
	if second[0].Total != 1 || second[1].Total != 1 {
		t.Fatalf("Total should still reflect cached tracked IDs after bd show failure: got %+v", second)
	}
	if second[0].Completed != 1 {
		t.Errorf("after a failed bd show, convoy hq-cv-eee1 Completed = %d, want 1 (fallback to last known status, not zeroed)", second[0].Completed)
	}
	if second[1].Completed != 0 {
		t.Errorf("after a failed bd show, convoy hq-cv-eee2 Completed = %d, want 0 (unchanged)", second[1].Completed)
	}
}
