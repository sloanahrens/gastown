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
	if !strings.Contains(processBranch.Description, `echo $! > "$REVIEW.pid"`) {
		t.Fatal("process-branch must persist the review PID across the step boundary")
	}
	if !strings.Contains(processBranch.Description, `echo "$TEMP_SHA" > "$REVIEW.sha"`) {
		t.Fatal("process-branch must record the reviewed head, so a later join can refuse a verdict from another attempt")
	}
	if !strings.Contains(processBranch.Description, "nohup") {
		t.Fatal("process-branch must launch the review under nohup so it survives the tool call that started it")
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
	if !strings.Contains(qualityReview.Description, `"$REVIEW.pid"`) || !strings.Contains(qualityReview.Description, "kill -0") {
		t.Fatal("quality-review Step 1 must join the backgrounded review by PID file, not launch a new one")
	}
	if !strings.Contains(qualityReview.Description, "Step 1.5") || !strings.Contains(qualityReview.Description, "discard the verdict") {
		t.Fatal("quality-review must discard the review verdict when run-tests already failed")
	}
	if !strings.Contains(qualityReview.Description, "Do NOT send FIX_NEEDED for the review's verdict") {
		t.Fatal("quality-review must not send a second FIX_NEEDED for a discarded verdict")
	}

	// A verdict on disk is only this attempt's if it names this attempt's head.
	if !strings.Contains(qualityReview.Description, `"$LAUNCH_SHA" = "$HEAD_SHA"`) {
		t.Fatal("quality-review must compare the launch-recorded head against the one being reviewed before trusting a verdict")
	}
	if !strings.Contains(qualityReview.Description, "REVIEW_STATE=unknown_head") ||
		!strings.Contains(qualityReview.Description, "REVIEW_STATE=still_running") {
		t.Fatal("quality-review must distinguish a stale/absent launch record and an unfinished review from a verdict")
	}
	// An empty exit must be mapped to the fail-closed value in code, not in prose.
	if !strings.Contains(qualityReview.Description, "REVIEW_EXIT=${REVIEW_EXIT:-2}") {
		t.Fatal("quality-review must fail closed to exit 2 for an empty or unattributable verdict")
	}
	// The consumed files are what stop a later attempt reading this one's result.
	if !strings.Contains(qualityReview.Description, `rm -f "$REVIEW.sha" "$REVIEW.pid" "$REVIEW.exit" "$REVIEW.json"`) {
		t.Fatal("quality-review must consume the launch record and verdict files after reading them")
	}

	// The wait bound has to fit inside the Bash tool's 600s ceiling, or it can
	// never fire and the step has no defined next move when the tool kills it.
	poll := formulaDefault(t, qualityReview.Description, "REVIEW_POLL_SECS")
	iters := formulaDefault(t, qualityReview.Description, "REVIEW_WAIT_ITERS")
	if bound := poll * iters; bound >= 600 {
		t.Fatalf("join waits %ds x %ds = %ds, at or over the 600s Bash ceiling; the bound could never fire before the tool kills the call", poll, iters, bound)
	}
	// `still_running` is the escape hatch for a review that outlives one call:
	// it must leave the state files alone so re-running the same call joins it.
	if !strings.Contains(qualityReview.Description, `[ "$REVIEW_STATE" != still_running ]`) {
		t.Fatal("a review that outlasts the wait must not consume its state, so the join stays re-runnable")
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
	return extractBashAfter(t, "for the job. Join it now:")
}

// formulaDefault reads the `:-N` fallback out of an env override the formula
// snippet carries, so a test can assert on the value the step actually runs
// with rather than on the override alone.
func formulaDefault(t *testing.T, text, name string) int {
	t.Helper()
	marker := "${" + name + ":-"
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("formula has no %s default (looked for %q)", name, marker)
	}
	rest := text[i+len(marker):]
	end := strings.Index(rest, "}")
	if end < 0 {
		t.Fatalf("%s default is unterminated", name)
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		t.Fatalf("parsing %s default %q: %v", name, rest[:end], err)
	}
	return n
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
func shaFile(mrID string) string  { return "/tmp/" + mrID + "-review.sha" }

func cleanupReviewFiles(mrID string) {
	for _, p := range []string{shaFile(mrID), pidFile(mrID), exitFile(mrID), outFile(mrID), logFile(mrID)} {
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

// joinEnv adds the poll override the join's wait is built around. The 5s
// default is sized for the refinery, not for a test that would otherwise idle
// five seconds per case and race the stub it is waiting on.
func joinEnv(base []string, pollSecs string, iters int) []string {
	env := append([]string{}, base...)
	return append(env,
		"REVIEW_POLL_SECS="+pollSecs,
		"REVIEW_WAIT_ITERS="+strconv.Itoa(iters))
}

// runJoin renders and runs the join snippet. The join reports through shell
// variables rather than an exit code, so the snippet has to echo them for a
// caller to assert on.
func runJoin(t *testing.T, repo string, env []string, mrID, pollSecs string, iters int) string {
	t.Helper()
	join := renderPlaceholders(joinSnippet(t), mrID, 1, "true") + `
echo "TEST_REVIEW_EXIT=$REVIEW_EXIT"
echo "TEST_REVIEW_STATE=$REVIEW_STATE"
echo "TEST_REVIEW_JSON=$REVIEW_JSON"`
	out, err := runSnippetEnv(t, repo, joinEnv(env, pollSecs, iters), join)
	if err != nil {
		t.Fatalf("join snippet failed: %v\n%s", err, out)
	}
	return out
}

// reviewCase drives the launch and join snippets against a stub result and
// asserts the verdict they report back.
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

		out := runJoin(t, repo, env, mrID, "0.2", 20)

		if got := extractVar(t, out, "TEST_REVIEW_STATE"); got != "joined" {
			t.Fatalf("REVIEW_STATE = %q, want joined\noutput:\n%s", got, out)
		}
		gotExit := extractVar(t, out, "TEST_REVIEW_EXIT")
		if gotExit != strconv.Itoa(exitCode) {
			t.Fatalf("REVIEW_EXIT = %q, want %d\noutput:\n%s", gotExit, exitCode, out)
		}
		gotJSON := extractVar(t, out, "TEST_REVIEW_JSON")
		if gotJSON != body {
			t.Fatalf("REVIEW_JSON = %q, want %q", gotJSON, body)
		}

		// The recorded pid is the launch job's shell, which waits for the
		// review and writes its exit — so once a verdict was joined, the job
		// is gone and nothing is left running behind it.
		if processAlive(pid) {
			t.Fatalf("review job %d still alive after the join read its exit file", pid)
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
//
// The work stand-in is longer than the review, so by the time the join runs
// the review has finished and the join does not wait at all — that is the
// common case the design is built on, and it keeps the timing assertion
// independent of the join's poll interval.
func TestEditorialReviewOverlapsWithConcurrentWork(t *testing.T) {
	mrID := uniqueMRID(t)
	defer cleanupReviewFiles(mrID)
	const reviewMillis = 500
	const workMillis = 800
	repo, env := snippetFixture(t, reviewMillis, 0, `{"Class":"approve"}`)

	launch := renderPlaceholders(launchSnippet(t), mrID, 1, "true")
	start := time.Now()
	if out, err := runSnippetEnv(t, repo, env, launch); err != nil {
		t.Fatalf("launch snippet failed: %v\n%s", err, out)
	}

	// Stand-in for run-tests running in the same window as the review.
	time.Sleep(workMillis * time.Millisecond)

	out := runJoin(t, repo, env, mrID, "0.1", 200)
	if got := extractVar(t, out, "TEST_REVIEW_STATE"); got != "joined" {
		t.Fatalf("REVIEW_STATE = %q, want joined; the review should have finished inside the work window\noutput:\n%s", got, out)
	}
	elapsed := time.Since(start)

	serial := (reviewMillis + workMillis) * time.Millisecond
	if elapsed >= serial {
		t.Fatalf("total wall time %s did not beat the serial bound %s; review and work did not overlap", elapsed, serial)
	}
	// Half the review's wall time must have been reclaimed, leaving room for
	// process and join overhead on top of the slower of the two.
	ceiling := (workMillis + reviewMillis/2) * time.Millisecond
	if elapsed >= ceiling {
		t.Fatalf("total wall time %s exceeded the overlap ceiling %s (review: %dms, work: %dms)", elapsed, ceiling, reviewMillis, workMillis)
	}
	t.Logf("gt-cmcv overlap: review=%dms work=%dms serial-bound=%s measured=%s", reviewMillis, workMillis, serial, elapsed)
}

// ---- Fail-closed and re-runnable paths ----------------------------------
//
// These are the states the launch snippet does not normally leave behind, and
// they are the ones that decide whether a stale verdict can be read as this
// attempt's. Every one of them must end in exit 2 (fail closed), never in an
// empty REVIEW_EXIT and never in a verdict that belongs to another head.

// stageReviewFiles writes a launch record and verdict files by hand, so a test
// can stage a state the launch snippet would not produce: an earlier attempt's
// leftovers, a review that died mid-flight, no launch record at all.
func stageReviewFiles(t *testing.T, mrID, headSHA string, pid int, exit string) {
	t.Helper()
	if headSHA != "" {
		writeFile(t, shaFile(mrID), headSHA+"\n")
	}
	if pid > 0 {
		writeFile(t, pidFile(mrID), strconv.Itoa(pid)+"\n")
	}
	if exit != "" {
		writeFile(t, exitFile(mrID), "REVIEW_EXIT="+exit+"\n")
	}
}

// deadPID returns a pid that has already exited and been reaped, so `kill -0`
// fails on it exactly as it would for a review that died without recording an
// exit.
func deadPID(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 0")
	if err := cmd.Run(); err != nil {
		t.Fatalf("running throwaway process: %v", err)
	}
	return cmd.Process.Pid
}

func TestEditorialReviewJoinFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		stage func(t *testing.T, mrID, headSHA string)
	}{
		{
			name:  "no launch record at all",
			stage: func(t *testing.T, mrID, headSHA string) {},
		},
		{
			// The MR bead is reused across attempts, so a previous attempt's
			// approve can still be sitting on disk when the next join runs.
			// Reading it would approve a head nobody reviewed.
			name: "leftover verdict from another head",
			stage: func(t *testing.T, mrID, headSHA string) {
				stageReviewFiles(t, mrID, strings.Repeat("0", 40), deadPID(t), "0")
			},
		},
		{
			name: "review died without recording an exit",
			stage: func(t *testing.T, mrID, headSHA string) {
				stageReviewFiles(t, mrID, headSHA, deadPID(t), "")
			},
		},
		{
			name: "head recorded but no pid and no exit",
			stage: func(t *testing.T, mrID, headSHA string) {
				stageReviewFiles(t, mrID, headSHA, 0, "")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mrID := uniqueMRID(t)
			defer cleanupReviewFiles(mrID)
			repo, env := snippetFixture(t, 0, 0, `{"Class":"approve"}`)
			headSHA := gitIn(t, repo, "rev-parse", "temp")
			tc.stage(t, mrID, headSHA)

			out := runJoin(t, repo, env, mrID, "0.05", 2)

			if got := extractVar(t, out, "TEST_REVIEW_EXIT"); got != "2" {
				t.Fatalf("REVIEW_EXIT = %q, want 2 (fail closed)\noutput:\n%s", got, out)
			}
			if got := extractVar(t, out, "TEST_REVIEW_STATE"); got != "unknown_head" && got != "joined" {
				t.Fatalf("REVIEW_STATE = %q, want unknown_head or joined\noutput:\n%s", got, out)
			}
			// Nothing may be left on disk for a later join to pick up.
			for _, p := range []string{shaFile(mrID), pidFile(mrID), exitFile(mrID)} {
				if _, err := os.Stat(p); err == nil {
					t.Fatalf("%s survived a fail-closed join; a later attempt could read it", p)
				}
			}
		})
	}
}

// TestEditorialReviewJoinIsRerunnableWhileTheReviewRuns covers the wait bound:
// a review that outlasts one call must not be consumed or reported as a
// verdict, and re-running the same call must pick it up when it finishes.
// This is what replaces killing the job at the bound — the refinery never
// escalates on a review that is still running.
func TestEditorialReviewJoinIsRerunnableWhileTheReviewRuns(t *testing.T) {
	mrID := uniqueMRID(t)
	defer cleanupReviewFiles(mrID)
	repo, env := snippetFixture(t, 800, 0, `{"Class":"approve"}`)

	launch := renderPlaceholders(launchSnippet(t), mrID, 1, "true")
	if out, err := runSnippetEnv(t, repo, env, launch); err != nil {
		t.Fatalf("launch snippet failed: %v\n%s", err, out)
	}

	first := runJoin(t, repo, env, mrID, "0.05", 2)
	if got := extractVar(t, first, "TEST_REVIEW_STATE"); got != "still_running" {
		t.Fatalf("REVIEW_STATE = %q, want still_running for a review that outlasts the wait\noutput:\n%s", got, first)
	}
	if _, err := os.Stat(shaFile(mrID)); err != nil {
		t.Fatalf("the launch record was consumed by an inconclusive join: %v", err)
	}

	// The same call, re-run once the review has finished, must now join it.
	time.Sleep(1100 * time.Millisecond)
	second := runJoin(t, repo, env, mrID, "0.05", 200)
	if got := extractVar(t, second, "TEST_REVIEW_STATE"); got != "joined" {
		t.Fatalf("REVIEW_STATE = %q, want joined on the re-run\noutput:\n%s", got, second)
	}
	if got := extractVar(t, second, "TEST_REVIEW_EXIT"); got != "0" {
		t.Fatalf("REVIEW_EXIT = %q, want 0 on the re-run\noutput:\n%s", got, second)
	}
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
