package formula

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// gt-cmcv: the single-MR refinery path used to invoke `gt mq review`
// serially, after the test suite. process-branch's Step 3.5 now launches it
// in the background right after the rehearsal succeeds, and quality-review's
// Step 1 joins it after run-tests finishes — so the review and the suite
// overlap instead of stacking. These tests cover both halves: the formula
// text describes the right shape (structural), and the embedded bash
// snippets actually behave like a background launch + bounded join
// (executable, against a stub `gt mq review`).

// TestRefineryPatrolEditorialOverlapStructure pins the parts of the formula
// text a careless edit could silently regress: the launch happens in
// process-branch (not quality-review), it passes a resolved SHA rather than
// the moving branch name `temp`, the join in quality-review does not shell
// out to `gt mq review` a second time, and a run-tests failure discards the
// verdict without sending a second FIX_NEEDED for it.
func TestRefineryPatrolEditorialOverlapStructure(t *testing.T) {
	f := loadRefineryPatrolFormula(t)

	processBranch := requireFormulaStep(t, f, "process-branch")
	runTests := requireFormulaStep(t, f, "run-tests")
	qualityReview := requireFormulaStep(t, f, "quality-review")

	if !strings.Contains(processBranch.Description, "gt mq review") {
		t.Fatal("process-branch must launch gt mq review (Step 3.5)")
	}
	if !strings.Contains(processBranch.Description, `--rehearsed "$TEMP_SHA"`) {
		t.Fatal("process-branch must review a resolved SHA, not the moving branch name temp")
	}
	if !strings.Contains(processBranch.Description, "REVIEW_PID_FILE") {
		t.Fatal("process-branch must persist the review PID across the step boundary")
	}
	if strings.Contains(processBranch.Description, "--rehearsed temp\n") ||
		strings.Contains(processBranch.Description, "--rehearsed temp ") {
		t.Fatal("process-branch must not pass the bare branch name temp to --rehearsed")
	}

	// The review must be launched only on the successful-rehearsal path, not
	// from inside the conflict branch (which never proceeds to run-tests).
	successIdx := strings.Index(processBranch.Description, "If merge rehearsal SUCCEEDED")
	launchIdx := strings.Index(processBranch.Description, "Step 3.5: Launch editorial review")
	conflictIdx := strings.Index(processBranch.Description, "If merge rehearsal FAILED with conflicts")
	if successIdx < 0 || launchIdx < 0 || conflictIdx < 0 {
		t.Fatal("process-branch missing expected success/conflict/launch markers")
	}
	if !(successIdx < conflictIdx && conflictIdx < launchIdx) {
		t.Fatalf("expected order success(%d) < conflict(%d) < launch(%d) so the launch sits after both branches, reached only via the success path", successIdx, conflictIdx, launchIdx)
	}

	if !strings.Contains(runTests.Description, "gt-cmcv") {
		t.Fatal("run-tests should note the concurrently-running review so a reader isn't surprised by the background job")
	}

	// quality-review must join, not re-invoke.
	reviewCallCount := strings.Count(qualityReview.Description, "gt mq review <mr-bead-id>")
	if reviewCallCount != 0 {
		t.Fatalf("quality-review must not invoke `gt mq review <mr-bead-id>` itself (moved to process-branch); found %d occurrences", reviewCallCount)
	}
	if !strings.Contains(qualityReview.Description, "REVIEW_PID_FILE") || !strings.Contains(qualityReview.Description, "kill -0") {
		t.Fatal("quality-review Step 1 must join the backgrounded review by PID file, not launch a new one")
	}
	if !strings.Contains(qualityReview.Description, "Step 1.5") || !strings.Contains(qualityReview.Description, "discard the verdict") {
		t.Fatal("quality-review must discard the review verdict when run-tests already failed")
	}
	if !strings.Contains(qualityReview.Description, "Do NOT send FIX_NEEDED for the review's verdict") {
		t.Fatal("quality-review must not send a second FIX_NEEDED for a discarded verdict")
	}

	discardIdx := strings.Index(qualityReview.Description, "Step 1.5")
	branchIdx := strings.Index(qualityReview.Description, "Step 2: Branch on the exit code")
	if discardIdx < 0 || branchIdx < 0 || discardIdx > branchIdx {
		t.Fatal("Step 1.5 (discard on test failure) must precede Step 2 (branch on exit code, run-tests already passed)")
	}
}

// ---- Executable snippet tests -------------------------------------------
//
// These extract the literal bash from process-branch's Step 3.5 (launch) and
// quality-review's Step 1 (join) and run them against a stub `gt` on PATH,
// the same way refinery_conflict_detector_test.go exercises the conflict
// snippet: the formula text IS the implementation here (no Go engine
// change), so the test has to run that text, not a Go reimplementation of it.

func launchSnippet(t *testing.T) string {
	t.Helper()
	return extractBashAfter(t, "quality-review reuses this same N in its FIX_NEEDED mail and MERGE\nREJECTION notes.")
}

func joinSnippet(t *testing.T) string {
	t.Helper()
	return extractBashAfter(t, "longer. Join it now:")
}

func extractBashAfter(t *testing.T, marker string) string {
	t.Helper()
	raw, err := formulasFS.ReadFile("formulas/mol-refinery-patrol.formula.toml")
	if err != nil {
		t.Fatalf("reading refinery formula: %v", err)
	}
	content := string(raw)
	i := strings.Index(content, marker)
	if i < 0 {
		t.Fatalf("formula no longer contains %q", marker)
	}
	rest := content[i+len(marker):]
	open := strings.Index(rest, "```bash")
	if open < 0 {
		t.Fatalf("no bash block after %q", marker)
	}
	body := rest[open+len("```bash"):]
	end := strings.Index(body, "```")
	if end < 0 {
		t.Fatalf("unterminated bash block after %q", marker)
	}
	return strings.TrimSpace(body[:end])
}

// renderPlaceholders substitutes the formula template placeholders the
// launch/join snippets carry with values usable in a test.
func renderPlaceholders(snippet, mrID string, attempt int, editorialRequired string) string {
	s := strings.ReplaceAll(snippet, "{{editorial_required}}", editorialRequired)
	s = strings.ReplaceAll(s, "<mr-bead-id>", mrID)
	s = strings.ReplaceAll(s, "<N>", strconv.Itoa(attempt))
	return s
}

// stubGT writes an executable `gt` on disk that answers `mq review ...
// --json` by sleeping `sleepMillis` and then printing `body` to stdout and
// exiting `exitCode` — standing in for the real `gt mq review` so these
// tests exercise the formula's process/file bookkeeping, not om itself.
func stubGT(t *testing.T, dir string, sleepMillis int, exitCode int, body string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
sleep %s
printf '%%s' %s
exit %d
`, formatSleepArg(sleepMillis), shQuote(body), exitCode)
	path := filepath.Join(dir, "gt")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub gt: %v", err)
	}
}

func formatSleepArg(ms int) string {
	return strconv.FormatFloat(float64(ms)/1000.0, 'f', 3, 64)
}

func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// snippetFixture returns a repo checked out on branch "temp" (process-branch
// always runs the launch snippet from there) with a stub `gt` on PATH and a
// PATH env ready to hand to exec.Cmd.
func snippetFixture(t *testing.T, sleepMillis, exitCode int, body string) (dir string, env []string) {
	t.Helper()
	requireGitWorktreeSupport(t)
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, repo, "init", "-q", "-b", "main")
	writeFile(t, filepath.Join(repo, "f.txt"), "base\n")
	gitIn(t, repo, "add", "-A")
	gitIn(t, repo, "commit", "-qm", "base")
	gitIn(t, repo, "checkout", "-qb", "temp")

	stubGT(t, bin, sleepMillis, exitCode, body)

	env = append(testGitEnv(), "PATH="+bin+":"+os.Getenv("PATH"))
	return repo, env
}

// runSnippetEnv is runSnippet plus a caller-supplied environment, needed here
// because the stub gt must be findable on PATH ahead of the real one.
func runSnippetEnv(t *testing.T, dir string, env []string, snippet string) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", "-c", snippet)
	cmd.Dir = dir
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func pidFile(mrID string) string  { return "/tmp/" + mrID + "-review.pid" }
func exitFile(mrID string) string { return "/tmp/" + mrID + "-review.exit" }
func outFile(mrID string) string  { return "/tmp/" + mrID + "-review.json" }
func logFile(mrID string) string  { return "/tmp/" + mrID + "-review.log" }

func cleanupReviewFiles(mrID string) {
	for _, p := range []string{pidFile(mrID), exitFile(mrID), outFile(mrID), logFile(mrID)} {
		os.Remove(p)
	}
}

func readPID(t *testing.T, mrID string) int {
	t.Helper()
	raw, err := os.ReadFile(pidFile(mrID))
	if err != nil {
		t.Fatalf("reading pid file: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parsing pid %q: %v", raw, err)
	}
	return pid
}

func processAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Unix: FindProcess always succeeds; signal 0 is the liveness probe the
	// join snippet itself uses (`kill -0`).
	return proc.Signal(syscall.Signal(0)) == nil
}

// TestEditorialReviewLaunchIsBackgroundedAndDisowned proves process-branch's
// launch snippet does not block: it must return long before the stub `gt mq
// review` it started has finished, and the started process must still be
// alive at that point — otherwise there is nothing for run-tests to overlap
// with.
func TestEditorialReviewLaunchIsBackgroundedAndDisowned(t *testing.T) {
	mrID := uniqueMRID(t)
	defer cleanupReviewFiles(mrID)
	repo, env := snippetFixture(t, 400, 0, `{"Class":"approve"}`)

	snippet := renderPlaceholders(launchSnippet(t), mrID, 1, "true")
	start := time.Now()
	out, err := runSnippetEnv(t, repo, env, snippet)
	if err != nil {
		t.Fatalf("launch snippet failed: %v\n%s", err, out)
	}
	elapsed := time.Since(start)
	if elapsed >= 350*time.Millisecond {
		t.Fatalf("launch snippet took %s; it must return immediately, not block on the review it starts", elapsed)
	}

	pid := readPID(t, mrID)
	if !processAlive(pid) {
		t.Fatal("review process is not alive right after launch returned; it was not really backgrounded")
	}

	// Let the stub finish and reap it so the test doesn't leak a process.
	time.Sleep(500 * time.Millisecond)
	if processAlive(pid) {
		t.Fatal("stub gt did not exit on its own; test fixture assumption broken")
	}
}

// TestEditorialReviewLaunchSkippedWhenNotRequired proves the launch is a
// no-op on a rig that hasn't opted into editorial review — there must be
// nothing left for quality-review to join later.
func TestEditorialReviewLaunchSkippedWhenNotRequired(t *testing.T) {
	mrID := uniqueMRID(t)
	defer cleanupReviewFiles(mrID)
	repo, env := snippetFixture(t, 0, 0, `{}`)

	snippet := renderPlaceholders(launchSnippet(t), mrID, 1, "false")
	out, err := runSnippetEnv(t, repo, env, snippet)
	if err != nil {
		t.Fatalf("launch snippet failed: %v\n%s", err, out)
	}
	if _, statErr := os.Stat(pidFile(mrID)); statErr == nil {
		t.Fatal("editorial_required=false must not launch a review (found a pid file)")
	}
}

// reviewCase drives the join snippet against a stub result and asserts the
// exit code and JSON body it reports back.
func reviewCase(t *testing.T, name string, exitCode int, body string) {
	t.Run(name, func(t *testing.T) {
		mrID := uniqueMRID(t)
		defer cleanupReviewFiles(mrID)
		repo, env := snippetFixture(t, 150, exitCode, body)

		launch := renderPlaceholders(launchSnippet(t), mrID, 1, "true")
		if out, err := runSnippetEnv(t, repo, env, launch); err != nil {
			t.Fatalf("launch snippet failed: %v\n%s", err, out)
		}
		pid := readPID(t, mrID)

		join := renderPlaceholders(joinSnippet(t), mrID, 1, "true") +
			"\necho \"TEST_REVIEW_EXIT=$REVIEW_EXIT\"\necho \"TEST_REVIEW_JSON=$REVIEW_JSON\""
		out, err := runSnippetEnv(t, repo, env, join)
		if err != nil {
			t.Fatalf("join snippet failed: %v\n%s", err, out)
		}

		gotExit := extractVar(t, out, "TEST_REVIEW_EXIT")
		if gotExit != strconv.Itoa(exitCode) {
			t.Fatalf("REVIEW_EXIT = %q, want %d\noutput:\n%s", gotExit, exitCode, out)
		}
		gotJSON := extractVar(t, out, "TEST_REVIEW_JSON")
		if gotJSON != body {
			t.Fatalf("REVIEW_JSON = %q, want %q", gotJSON, body)
		}

		// gt-cmcv: whatever the verdict, the join must fully reap the
		// process — no orphan left running or zombied regardless of outcome
		// (this is the "cancellation" case from the required test matrix:
		// even a discarded verdict must not leak a background job).
		if processAlive(pid) {
			t.Fatalf("review process %d still alive after join; it was not reaped (orphan)", pid)
		}
	})
}

// TestEditorialReviewJoinReadsEachVerdict covers the three exit codes
// quality-review branches on, matching the design's required matrix:
// approve (exit 0, paired with a red suite in the full flow — this test
// covers the review half), request_changes (exit 1, paired with a green
// suite), and an infra failure (exit 2). The suite-side pairing (red/green)
// is exercised by run-tests itself, which this change does not touch.
func TestEditorialReviewJoinReadsEachVerdict(t *testing.T) {
	reviewCase(t, "approve", 0, `{"Class":"approve","Note":{"Score":0.9}}`)
	reviewCase(t, "request_changes", 1, `{"Class":"request_changes","Note":{"Score":0.4,"FindingsCount":2}}`)
	reviewCase(t, "infra_error", 2, `{"Class":"timeout"}`)
}

// TestEditorialReviewOverlapsWithConcurrentWork is the wall-time claim made
// by the design (max(suite, review) instead of suite + review): launch the
// review, do a stand-in for run-tests' own work concurrently, then join.
// Total wall time must land near the slower of the two, not their sum.
func TestEditorialReviewOverlapsWithConcurrentWork(t *testing.T) {
	mrID := uniqueMRID(t)
	defer cleanupReviewFiles(mrID)
	const reviewMillis = 600
	const workMillis = 600
	repo, env := snippetFixture(t, reviewMillis, 0, `{"Class":"approve"}`)

	launch := renderPlaceholders(launchSnippet(t), mrID, 1, "true")
	start := time.Now()
	if out, err := runSnippetEnv(t, repo, env, launch); err != nil {
		t.Fatalf("launch snippet failed: %v\n%s", err, out)
	}

	// Stand-in for run-tests running in the same window as the review.
	time.Sleep(workMillis * time.Millisecond)

	join := renderPlaceholders(joinSnippet(t), mrID, 1, "true")
	if out, err := runSnippetEnv(t, repo, env, join); err != nil {
		t.Fatalf("join snippet failed: %v\n%s", err, out)
	}
	elapsed := time.Since(start)

	serial := (reviewMillis + workMillis) * time.Millisecond
	if elapsed >= serial {
		t.Fatalf("total wall time %s did not beat the serial bound %s; review and work did not overlap", elapsed, serial)
	}
	// Generous ceiling: overlapped work should land close to
	// max(review, work) plus process/join overhead, well under the serial sum.
	ceiling := (workMillis + reviewMillis/2) * time.Millisecond
	if elapsed >= ceiling {
		t.Fatalf("total wall time %s exceeded the overlap ceiling %s (review: %dms, work: %dms)", elapsed, ceiling, reviewMillis, workMillis)
	}
	t.Logf("gt-cmcv overlap: review=%dms work=%dms serial-bound=%s measured=%s", reviewMillis, workMillis, serial, elapsed)
}

func extractVar(t *testing.T, out, name string) string {
	t.Helper()
	prefix := name + "="
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	t.Fatalf("output missing %s=...; full output:\n%s", name, out)
	return ""
}

func uniqueMRID(t *testing.T) string {
	t.Helper()
	safe := strings.NewReplacer("/", "-", " ", "-").Replace(t.Name())
	return fmt.Sprintf("gt-test-%s-%d", safe, time.Now().UnixNano())
}
