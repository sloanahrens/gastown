package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/lintlock"
	"github.com/steveyegge/gastown/internal/slot"
)

// initVerifyTestGoRepo builds a tiny Go module with two packages (pkga,
// pkgb) at HEAD "base", so tests can add commits on top and diff against
// that base to exercise changedGoPackages / runDefaultTestVerification.
func initVerifyTestGoRepo(t *testing.T) (dir, base string) {
	t.Helper()
	return cachedGitFixture(t, "verify-test-go-repo", func(dir string) (string, error) {
		return buildVerifyTestGoRepo(t, dir), nil
	})
}

// buildVerifyTestGoRepo makes initVerifyTestGoRepo's repo in dir and returns
// its base commit.
func buildVerifyTestGoRepo(t *testing.T, dir string) (base string) {
	t.Helper()
	runGit := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	runGit("init", "-q", "-b", "main")
	runGit("config", "user.email", "test@example.com")
	runGit("config", "user.name", "Test")

	mustWrite := func(rel, content string) {
		t.Helper()
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}

	mustWrite("go.mod", "module example.test\n\ngo 1.21\n")
	mustWrite("pkga/a.go", "package pkga\n\nfunc Add(a, b int) int { return a + b }\n")
	mustWrite("pkga/a_test.go", "package pkga\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n")
	mustWrite("pkgb/b.go", "package pkgb\n\nfunc Double(a int) int { return a * 2 }\n")
	mustWrite("pkgb/b_test.go", "package pkgb\n\nimport \"testing\"\n\nfunc TestDouble(t *testing.T) {\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"bad double\")\n\t}\n}\n")

	runGit("add", ".")
	runGit("commit", "-q", "-m", "base")
	base = runGit("rev-parse", "HEAD")
	runGit("update-ref", "refs/remotes/origin/main", base)
	return base
}

// stubVerifyGate replaces vg's slot-acquire and suite-runner hooks, so the gate can be driven without the real
// container-gate slot (which would contend with every other suite on a shared
// Gas Town host) and without running `go test` over a real package tree.
//
// It also stands the container watch's docker listing down (gt-0ss4). The
// watch is the gate's one `docker ps` caller, and a slot-free gate run now
// takes a baseline listing: a stray dolt/testcontainers/ryuk container on a
// shared host would otherwise decide a gate test's outcome. A test that drives
// the watch overrides the listing itself, after this call.
func stubVerifyGate(
	t *testing.T,
	vg *testVerifyGate,
	acquire func(townRoot, role string, timeout time.Duration) (func(), error),
	run func(ctx context.Context, worktree, script string, env []string, logFile *os.File) error,
) {
	t.Helper()
	stubNoContainers(t)
	if acquire != nil {
		vg.acquireSlot = acquire
	}
	if run != nil {
		vg.runSuite = run
	}
}

// stubVerifyLintRetryDelay shortens the waits between lint-lock retries so a
// test can drive the retry path (gt-xsty) without sleeping the real tens of
// seconds.
func stubVerifyLintRetryDelay(vg *testVerifyGate, delays ...time.Duration) {
	vg.lintRetryDelays = delays
}

// stubVerifyProgress shortens the gate's progress interval so a test can
// observe progress lines without waiting out the real one.
func stubVerifyProgress(vg *testVerifyGate, interval time.Duration) {
	vg.progressInterval = interval
}

// stubLintVerifyTimeout shrinks the lint gate's budget so a test can drive a
// real expiry (gt-taoz) rather than sleeping out the 10m default.
func stubLintVerifyTimeout(vg *testVerifyGate, budget time.Duration) {
	vg.lintTimeout = budget
}

func TestResolveTestVerifyBudgets(t *testing.T) {
	t.Parallel()

	t.Run("defaults: slot cap from the slot CLI default, run budget at the floor", func(t *testing.T) {
		t.Parallel()
		b := resolveTestVerifyBudgets(&config.MergeQueueConfig{TestCommand: "go test ./..."})
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the %s floor (gt-btw1: the gate runs the full suite, so there is nothing to scale by)", b.runTimeout, defaultTestVerifyRunFloor)
		}
		if !strings.Contains(b.runSource, "full test_command") {
			t.Errorf("runSource = %q, want it to explain the full-suite floor", b.runSource)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want %s", b.slotTimeout, defaultTestVerifySlotTimeout)
		}
	})

	t.Run("rig config overrides both budgets", func(t *testing.T) {
		t.Parallel()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "90m",
			TestVerifySlotTimeout: "15m",
		}
		b := resolveTestVerifyBudgets(mq)
		if b.runTimeout != 90*time.Minute {
			t.Errorf("runTimeout = %s, want 90m", b.runTimeout)
		}
		if b.slotSource != "merge_queue.test_verify_slot_timeout" {
			t.Errorf("slotSource = %q", b.slotSource)
		}
		if b.slotTimeout != 15*time.Minute {
			t.Errorf("slotTimeout = %s, want 15m", b.slotTimeout)
		}
	})

	t.Run("invalid durations fall back and say so", func(t *testing.T) {
		t.Parallel()
		mq := &config.MergeQueueConfig{
			TestCommand:           "go test ./...",
			TestVerifyRunTimeout:  "banana",
			TestVerifySlotTimeout: "-5m",
		}
		b := resolveTestVerifyBudgets(mq)
		if b.runTimeout != defaultTestVerifyRunFloor {
			t.Errorf("runTimeout = %s, want the derived floor", b.runTimeout)
		}
		if !strings.Contains(b.runSource, "banana") {
			t.Errorf("runSource = %q, want it to flag the rejected override", b.runSource)
		}
		if b.slotTimeout != defaultTestVerifySlotTimeout {
			t.Errorf("slotTimeout = %s, want the default", b.slotTimeout)
		}
		if !strings.Contains(b.slotSource, "-5m") {
			t.Errorf("slotSource = %q, want it to flag the rejected override", b.slotSource)
		}
	})
}

func containsEnv(env []string, want string) bool {
	for _, e := range env {
		if e == want {
			return true
		}
	}
	return false
}

// gt-0hbm: the slot decision and the environment it is made for have to be one
// fact, so for a Go rig the switch the gate resolves and the switch value the
// run's environment ends up carrying must always agree — including when the
// session's own value says the opposite.
func TestResolveContainerSwitchStatesTheSlotDecision(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		isGoRig  bool
		commands []string
		ambient  string
		want     string // the switch value the run's environment must carry
		slot     bool
	}{
		{"Go rig that asks nothing", true, []string{"GOFLAGS=-p=8 make test"}, "", "0", false},
		{"Go rig asking inline", true, []string{dockerTestsEnv + "=1 go test ./..."}, "", "1", true},
		{"Go rig asking after the packages token", true, []string{"make test", dockerTestsEnv + "=1 go test {packages}"}, "", "1", true},
		{"Go rig that asks nothing, with the session's opt-in on", true, []string{"GOFLAGS=-p=8 make test"}, "1", "0", false},
		{"Go rig that asks inline, with the session's opt-in off", true, []string{dockerTestsEnv + "=1 go test ./..."}, "0", "1", true},
		{"Go rig explicitly opting out, with the session's opt-in on", true, []string{dockerTestsEnv + "=0 go test ./..."}, "1", "0", false},
		{"non-Go rig, opaque command", false, []string{"npm test"}, "", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ambient := []string{"HOME=/nonexistent", dockerTestsEnv + "=" + tc.ambient}
			got := resolveContainerSwitch(tc.isGoRig, tc.commands...)
			if got.value != tc.want || got.slot != tc.slot {
				t.Errorf("resolveContainerSwitch = %+v, want value %q slot %t", got, tc.want, tc.slot)
			}
			// The run's environment is built from the same decision, and the
			// rig prefix deliberately disagrees with it: the environment is
			// the value the child reads, so the leak a pass-through would
			// leave here is the hole gt-0hbm closed.
			env := verifyGateEnv(ambient, []string{"GOFLAGS=-p=8", dockerTestsEnv + "=0"}, got.value)
			var seen []string
			for _, kv := range env {
				if strings.HasPrefix(kv, dockerTestsEnv+"=") {
					seen = append(seen, strings.TrimPrefix(kv, dockerTestsEnv+"="))
				}
			}
			if tc.want == "" {
				// A non-Go rig's environment is left alone: the gate cannot
				// know what the switch means to a command it cannot read.
				if !containsEnv(env, dockerTestsEnv+"=0") || containsEnv(env, dockerTestsEnv+"=1") {
					t.Errorf("non-Go rig env carries %v, want the rig's own value untouched and no switch the gate invented", seen)
				}
				return
			}
			if len(seen) != 1 || seen[0] != tc.want {
				t.Errorf("run env carries %v, want exactly [%s]: the value the child reads is the value the slot decision was made for", seen, tc.want)
			}
			if got.slot != (seen[0] == "1") {
				t.Errorf("slot=%t but the run's environment says %s — a container could start outside the slot, or a slot be held for nothing", got.slot, seen[0])
			}
		})
	}
}

func runGitIn(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func runGitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

// TestLintFailureDetail pins what a failed lint attempt is allowed to tell the
// polecat. Only a lint that ran to completion may ask for findings to be fixed;
// lock contention, a lint that stopped at golangci-lint's own timeout, and a
// budget that ran out while the lint was still going all linted nothing
// (gt-xsty, gt-taoz).
//
// The verdicts come from the final attempt rather than from a retry count, so a
// lint that contended once and then reported a real finding is sent back as a
// finding (gt-ijqw, om-gate attempt 1 major).
func TestLintFailureDetail(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		outcome       lintlock.Outcome
		budgetExpired bool
		want          string
		notWant       string
	}{
		{
			name:    "a lint that completed and reported findings",
			want:    "fix the lint findings before resubmitting",
			notWant: "no finding is reported",
		},
		{
			name:    "lock contention the retries already proved",
			outcome: lintlock.Outcome{Err: errors.New("exit 2"), Waits: 2, Contended: true, Unfinished: true},
			want:    "held the lock across 2 retries",
			notWant: "fix the lint findings",
		},
		{
			name:    "golangci-lint's own timeout",
			outcome: lintlock.Outcome{Err: errors.New("exit 1"), Unfinished: true},
			want:    "stopped without reporting findings",
			notWant: "fix the lint findings",
		},
		{
			name:          "the budget ran out while the lint was still going",
			outcome:       lintlock.Outcome{Err: errors.New("signal: killed")},
			budgetExpired: true,
			want:          "killed at its 1m budget without finishing",
			notWant:       "fix the lint findings",
		},
		{
			name:          "both: contention is the more specific story",
			outcome:       lintlock.Outcome{Err: errors.New("exit 2"), Waits: 2, Contended: true, Unfinished: true},
			budgetExpired: true,
			want:          "held the lock across 2 retries",
			notWant:       "fix the lint findings",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := lintFailureDetail(tc.outcome, tc.budgetExpired, time.Minute)
			if !strings.Contains(got, tc.want) {
				t.Errorf("lintFailureDetail(%+v, %v) = %q, want it to contain %q", tc.outcome, tc.budgetExpired, got, tc.want)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("lintFailureDetail(%+v, %v) = %q, must not contain %q", tc.outcome, tc.budgetExpired, got, tc.notWant)
			}
		})
	}
}

// TestRunDefaultTestVerification_LintBudgetExpiry drives that case end to end:
// a lint that outlives its budget is killed by the gate's own context, and the
// refusal must not read as a finding. This is the failure gt-taoz opened —
// nothing in the log is attributable, because a lint blocked on a lock prints
// nothing at all.
func TestRunDefaultTestVerification_LintBudgetExpiry(t *testing.T) {
	t.Parallel()
	vg := newTestVerifyGate()
	stubNoContainers(t)
	townRoot := t.TempDir()
	stubLintVerifyTimeout(vg, 250*time.Millisecond)

	dir, _ := initVerifyTestGoRepo(t)
	changePkga(t, dir)
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

	testMarker := filepath.Join(dir, "tests-ran")
	mq := &config.MergeQueueConfig{
		TestCommand:       "go test ./...",
		LintCommand:       "sleep 30",
		TestVerifyCommand: "echo ran > '" + testMarker + "'",
	}
	g := git.NewGit(dir)
	result, err := vg.run(g, dir, "main", "main", mq, townRoot, "test/lint-budget-role")
	if err == nil {
		t.Fatalf("expected a refusal when the lint outlives its budget, got result=%+v", result)
	}
	if !strings.Contains(err.Error(), "budget without finishing") {
		t.Errorf("a lint killed by its budget was not attributed to the budget: %v", err)
	}
	if strings.Contains(err.Error(), "fix the lint findings") {
		t.Errorf("a lint killed by its budget was reported as a lint finding: %v", err)
	}
	if _, statErr := os.Stat(testMarker); statErr == nil {
		t.Error("tests ran despite the lint never finishing")
	}
}

// TestRunDefaultTestVerification_LintFindingAfterContention pins gt-1g9n's
// finding where it can still go wrong: a contended lint is retried, so the
// attempt that decides the verdict is not always the first one, and the gate
// must read that attempt's output rather than the log it shares with the
// attempts before it. Reading the whole log reports a real finding as
// contention — "nothing was linted; re-run", about the log the message cites —
// the misreport a retry count produces (om major on gt-wisp-bob, gt-1g9n;
// policy fixed under gt-ijqw).
//
// Both of gt done's lint gates share one log across attempts; only this one
// acts on the verdict the byte offset decides — the --pre-verified gate reports
// the failing gate's name and exit code and drops it, and the refinery's gate
// buffers each attempt separately. The sibling shapes in
// done_test_verify_lint_test.go cannot catch the offset going wrong (each
// prints the marker on every attempt, or runs a single attempt). This test
// needs no real suite run — the lint refuses first.
func TestRunDefaultTestVerification_LintFindingAfterContention(t *testing.T) {
	t.Parallel()
	vg := newTestVerifyGate()
	stubNoContainers(t)
	townRoot := t.TempDir()
	stubVerifyLintRetryDelay(vg, time.Millisecond, time.Millisecond)

	dir, _ := initVerifyTestGoRepo(t)
	changePkga(t, dir)
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch pkga")

	// Attempt 1 loses golangci-lint's lock and analyses nothing; attempt 2 gets
	// it and reports one real finding. The counter is both the switch between
	// the two attempts and the attempt count the assertions below read.
	counter := filepath.Join(dir, "lint-attempts")
	lint := fmt.Sprintf(`echo x >> %q; `+
		`if [ "$(wc -l < %q)" -lt 2 ]; then echo 'Error: parallel golangci-lint is running' >&2; exit 2; fi; `+
		`echo 'pkga/a.go:1:1: something is wrong (fakelint)'; exit 1`, counter, counter)

	testMarker := filepath.Join(dir, "tests-ran")
	mq := &config.MergeQueueConfig{
		TestCommand:       "go test ./...",
		LintCommand:       lint,
		TestVerifyCommand: "echo ran > '" + testMarker + "'",
	}
	g := git.NewGit(dir)
	result, err := vg.run(g, dir, "main", "main", mq, townRoot, "test/lint-finding-after-contention-role")
	if err == nil {
		t.Fatalf("expected a lint refusal, got result=%+v", result)
	}
	for _, want := range []string{
		"lint-verify failed",
		"exit 1",
		"fakelint",
		"fix the lint findings",
		"no tests were run",
		// The first attempt's marker really is in the log the refusal quotes,
		// so the assertion below is about the verdict, not about the marker
		// having gone missing from the gate's output.
		"parallel golangci-lint is running",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "held the lock") {
		t.Errorf("a finding reported by the retry was attributed to lock contention: %v", err)
	}
	if result.lintRan {
		t.Errorf("the lint was recorded as having run: %+v", result)
	}
	if got, readErr := os.ReadFile(counter); readErr != nil {
		t.Errorf("reading the lint attempt counter %s: %v", counter, readErr)
	} else if got := strings.Count(string(got), "\n"); got != 2 {
		t.Errorf("lint attempts = %d, want 2 (the collision, then the attempt that found something)", got)
	}
	if _, statErr := os.Stat(testMarker); statErr == nil {
		t.Error("tests ran despite the lint refusing the submission")
	}
}

// TestRunDefaultTestVerification_SlotWaitIsBounded covers the bounded-wait
// path gt-7dxw asks for: `gt done` waits for the container-gate slot on its
// own, for a wait BOUNDED by the rig's configured cap, printing a progress
// line while it waits, and fails with a message that tells the polecat what to
// do instead of looping.
//
// This is the shape the incident's improvised retry loop was working around,
// so the three things a polecat needs are asserted together: the cap the rig
// configured is the cap the acquire actually gets (not the 60m default), the
// wait ends when that cap expires rather than re-arming, and both the progress
// line and the giving-up message reach the polecat.
func TestRunDefaultTestVerification_SlotWaitIsBounded(t *testing.T) {
	t.Parallel()
	vg := newTestVerifyGate()
	const slotCap = 80 * time.Millisecond
	dir, _ := initVerifyTestGoRepo(t)
	for _, rel := range []string{"pkga/a.go", "pkgb/b.go"} {
		path := filepath.Join(dir, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, []byte("\n// touched\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch both packages")

	// One progress tick must land inside the cap, or the pane looks hung for
	// the whole wait — the failure gt-pnkd's victims could not diagnose.
	stubVerifyProgress(vg, slotCap/4)

	var (
		gotTimeout    time.Duration
		acquires      int
		acquireWindow time.Duration
	)
	stubVerifyGate(t, vg,
		func(_ string, _ string, timeout time.Duration) (func(), error) {
			acquires++
			gotTimeout = timeout
			// Stand in for the real slot.AcquirePool: block until the cap
			// expires, then give up with the slot package's own error text.
			started := time.Now()
			time.Sleep(timeout)
			acquireWindow = time.Since(started)
			return nil, errors.New("timed out after " + timeout.String() + " waiting for container-gate slot")
		},
		nil)

	mq := &config.MergeQueueConfig{
		TestCommand:           dockerTestsEnv + "=1 go test ./...",
		TestVerifySlotTimeout: slotCap.String(),
	}
	g := git.NewGit(dir)
	_, err := vg.run(g, dir, "main", "main", mq, t.TempDir(), "test/bounded-wait-role")

	if err == nil {
		t.Fatal("expected a slot-contention error once the cap expired")
	}
	if gotTimeout != slotCap {
		t.Errorf("acquire got a %s cap, want the rig's configured %s", gotTimeout, slotCap)
	}
	if acquires != 1 {
		t.Errorf("the gate acquired the slot %d times, want exactly 1 — the wait is not re-armed", acquires)
	}
	// Bounded: the acquire waits its cap out and gives up inside it. The
	// window is timed from inside the stub, because everything the gate does
	// before the acquire — the diff, the package list — is subprocess work
	// whose latency would otherwise land in the bound and make it flaky
	// (gt-7dxw review). acquires == 1 is what proves the wait is not re-armed.
	if acquireWindow < slotCap {
		t.Errorf("gave up after %s, before the %s cap expired", acquireWindow, slotCap)
	}
	if acquireWindow > slotCap+10*time.Second {
		t.Errorf("held the acquire for %s under a %s cap", acquireWindow, slotCap)
	}
	logPath := testVerifyLogPath(dir)
	msg := err.Error()
	for _, want := range []string{"slot contention", "NOT a test failure", slotCap.String(), "merge_queue.test_verify_slot_timeout", "gt escalate -s medium", logPath} {
		if !strings.Contains(msg, want) {
			t.Errorf("failure message is missing %q: %v", want, err)
		}
	}
	if strings.Contains(msg, "Re-run gt done") {
		t.Errorf("failure message still invites a re-run of the command that just failed: %v", err)
	}
}

// TestRunDefaultTestVerification_SlotWaitPrintsProgressBeforeGivingUp pins the
// progress half of the same path: while the wait is inside its cap the verify
// log — the artifact a bystander reads after the fact — already carries a
// waiting line, so a polecat's quiet pane is diagnosable without a restart
// (gt-pnkd's frozen 144-byte log).
func TestRunDefaultTestVerification_SlotWaitPrintsProgressBeforeGivingUp(t *testing.T) {
	t.Parallel()
	vg := newTestVerifyGate()
	const slotCap = 60 * time.Millisecond
	dir, _ := initVerifyTestGoRepo(t)
	for _, rel := range []string{"pkga/a.go", "pkgb/b.go"} {
		path := filepath.Join(dir, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, []byte("\n// touched\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	runGitIn(t, dir, "add", ".")
	runGitIn(t, dir, "commit", "-q", "-m", "touch both packages")

	stubVerifyProgress(vg, slotCap/6)
	stubVerifyGate(t, vg,
		func(_ string, _ string, timeout time.Duration) (func(), error) {
			time.Sleep(timeout)
			return nil, errors.New("timed out after " + timeout.String() + " waiting for container-gate slot")
		},
		nil)

	mq := &config.MergeQueueConfig{
		TestCommand:           dockerTestsEnv + "=1 go test ./...",
		TestVerifySlotTimeout: slotCap.String(),
	}
	g := git.NewGit(dir)
	_, err := vg.run(g, dir, "main", "main", mq, t.TempDir(), "test/bounded-log-role")
	if err == nil {
		t.Fatal("expected a slot-contention error once the cap expired")
	}
	logPath := testVerifyLogPath(dir)
	data, readErr := os.ReadFile(logPath)
	if readErr != nil {
		t.Fatalf("reading %s: %v", logPath, readErr)
	}
	logText := string(data)
	if !strings.Contains(logText, "still waiting for the container-gate slot") {
		t.Errorf("verify log has no slot-wait progress line, so a quiet pane is undiagnosable:\n%s", logText)
	}
	if !strings.Contains(logText, "slot cap:") {
		t.Errorf("verify log does not record the cap it was waiting under:\n%s", logText)
	}
}

// newTestVerifyGate is the default gate for a test to adjust: production's
// collaborators, which the stub helpers above replace one at a time.
func newTestVerifyGate() *testVerifyGate {
	return defaultTestVerifyGate()
}

// TestDefaultTestVerifyGateWiring guards the defaults gt done's gate runs
// with: every test drives its own testVerifyGate, so nothing else notices a
// default that silently stops taking the slot (a no-op acquireSlot would run
// container suites outside it) or stops watching for stray containers (an
// interval of 0 disables gt-0ss4's watch).
func TestDefaultTestVerifyGateWiring(t *testing.T) {
	t.Parallel()
	vg := defaultTestVerifyGate()
	funcIs := func(name string, got, want any) {
		t.Helper()
		if reflect.ValueOf(got).Pointer() != reflect.ValueOf(want).Pointer() {
			t.Errorf("default gate's %s is not the production function", name)
		}
	}
	funcIs("acquireSlot", vg.acquireSlot, acquireVerifySlot)
	funcIs("runSuite", vg.runSuite, runVerifySuite)
	funcIs("watch.containers", vg.watch.containers, slot.GateContainers)
	funcIs("watch.slotHeld", vg.watch.slotHeld, gateSlotHeld)
	if vg.watch.interval != containerWatchInterval {
		t.Errorf("watch.interval = %s, want %s", vg.watch.interval, containerWatchInterval)
	}
	if vg.lintTimeout != defaultLintVerifyTimeout {
		t.Errorf("lintTimeout = %s, want %s", vg.lintTimeout, defaultLintVerifyTimeout)
	}
	if vg.progressInterval != testVerifyProgressInterval {
		t.Errorf("progressInterval = %s, want %s", vg.progressInterval, testVerifyProgressInterval)
	}
	if vg.lintRetryDelays != nil || vg.env != nil {
		t.Errorf("lintRetryDelays = %v, env set = %t; want nil (lintlock.RetryDelay, os.Environ)", vg.lintRetryDelays, vg.env != nil)
	}
}

// TestRunDefaultTestVerification_RunsTheWholeSuite pins D9's one gate
// definition (gt-ik4a1.1): the gate runs the rig's whole test command every
// time, labels it scope=full, records no changed-package list, and refuses a
// test_verify_command that still carries the retired {packages} token instead
// of running it with the token left in.
func TestRunDefaultTestVerification_RunsTheWholeSuite(t *testing.T) {
	t.Parallel()
	readLog := func(t *testing.T, worktree string) string {
		t.Helper()
		data, err := os.ReadFile(testVerifyLogPath(worktree))
		if err != nil {
			t.Fatalf("read verify log: %v", err)
		}
		return string(data)
	}

	t.Run("a branch that changed no .go file still runs the whole suite", func(t *testing.T) {
		t.Parallel()
		vg := newTestVerifyGate()
		stubNoContainers(t)
		var ran []string
		stubVerifyGate(t, vg,
			func(string, string, time.Duration) (func(), error) { return func() {}, nil },
			func(_ context.Context, _, script string, _ []string, _ *os.File) error {
				ran = append(ran, script)
				return nil
			})
		dir, _ := initVerifyTestGoRepo(t)
		if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("docs only\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		runGitIn(t, dir, "add", ".")
		runGitIn(t, dir, "commit", "-q", "-m", "docs only")
		mq := &config.MergeQueueConfig{TestCommand: "make gate"}
		result, err := vg.run(git.NewGit(dir), dir, "main", "main", mq, t.TempDir(), "test/whole-suite")
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if !result.ran || !result.success {
			t.Fatalf("result = %+v, want a run that passed", result)
		}
		if len(ran) != 1 || ran[0] != "make gate" {
			t.Errorf("suite commands = %q, want exactly [make gate]", ran)
		}
		if result.scope != "full" || len(result.packages) != 0 {
			t.Errorf("scope = %q, packages = %v; want full and none", result.scope, result.packages)
		}
		log := readLog(t, dir)
		if !strings.Contains(log, "scope=full") || !strings.Contains(log, "base: ") {
			t.Errorf("verify log header lacks scope=full or the base it verified against:\n%s", log)
		}
	})

	t.Run("a test_verify_command with the retired {packages} token is refused", func(t *testing.T) {
		t.Parallel()
		vg := newTestVerifyGate()
		stubNoContainers(t)
		ran := 0
		stubVerifyGate(t, vg,
			func(string, string, time.Duration) (func(), error) { return func() {}, nil },
			func(context.Context, string, string, []string, *os.File) error { ran++; return nil })
		dir, _ := initVerifyTestGoRepo(t)
		mq := &config.MergeQueueConfig{
			TestCommand:       "make gate",
			TestVerifyCommand: "make test-changed PKGS='{packages}'",
		}
		_, err := vg.run(git.NewGit(dir), dir, "main", "main", mq, t.TempDir(), "test/retired-token")
		if err == nil || !strings.Contains(err.Error(), "{packages}") || !strings.Contains(err.Error(), "make gate") {
			t.Fatalf("err = %v, want a refusal naming {packages} and make gate", err)
		}
		if ran != 0 {
			t.Errorf("the gate ran %d commands before refusing, want 0", ran)
		}
	})

	t.Run("a test_verify_command without the token replaces the test command", func(t *testing.T) {
		t.Parallel()
		vg := newTestVerifyGate()
		stubNoContainers(t)
		var ran []string
		stubVerifyGate(t, vg,
			func(string, string, time.Duration) (func(), error) { return func() {}, nil },
			func(_ context.Context, _, script string, _ []string, _ *os.File) error {
				ran = append(ran, script)
				return nil
			})
		dir, _ := initVerifyTestGoRepo(t)
		mq := &config.MergeQueueConfig{TestCommand: "make gate", TestVerifyCommand: "make gate-lite"}
		if _, err := vg.run(git.NewGit(dir), dir, "main", "main", mq, t.TempDir(), "test/override"); err != nil {
			t.Fatalf("run: %v", err)
		}
		if len(ran) != 1 || ran[0] != "make gate-lite" {
			t.Errorf("suite commands = %q, want exactly [make gate-lite]", ran)
		}
	})
}
