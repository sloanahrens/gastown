package cmd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// primeTestToolTimeout is deliberately far larger than any property under test.
// Most tests in this file assert on what prime *does* with a stub tool's output,
// so the stub has to be allowed to finish: under gate load (GOFLAGS=-p=8 across
// the whole suite) a /bin/sh fork+exec can take well over a second, and a
// deadline tight enough to be interesting is a deadline tight enough to kill
// the stub before it writes anything — which is how gt-5clb turned a clean
// branch into "read call log: open .../calls.log: no such file or directory".
// Bound enforcement is covered separately, by deadline-driven tests below.
const primeTestToolTimeout = 60 * time.Second

// primeTestSlowToolStall is how long a stub on the slow path sleeps. It only
// has to outlive the moment prime abandons the tool (see
// withPrimeExternalToolDeadline), and it doubles as the safety-net ceiling for
// how long such a call may take.
const primeTestSlowToolStall = 10 * time.Second

// primeTestBarrierWait caps how long a test waits for a stub to announce it has
// started. It is a fallback, not a correctness bound: reaching it means the host
// could not fork+exec /bin/sh, which no assertion in this file can say anything
// about.
const primeTestBarrierWait = 60 * time.Second

func setupPrimeExternalToolTest(t *testing.T, bdScript, gtScript string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script subprocess test")
	}
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "calls.log")

	oldTimeout := primeExternalToolTimeout
	oldWaitDelay := primeExternalToolWaitDelay
	primeExternalToolTimeout = primeTestToolTimeout
	primeExternalToolWaitDelay = 150 * time.Millisecond
	t.Cleanup(func() {
		primeExternalToolTimeout = oldTimeout
		primeExternalToolWaitDelay = oldWaitDelay
	})

	binDir := filepath.Join(tmpDir, "bin")
	if err := os.Mkdir(binDir, 0700); err != nil {
		t.Fatalf("create bin dir: %v", err)
	}
	writePrimeToolScript(t, filepath.Join(binDir, "bd"), bdScript)
	writePrimeToolScript(t, filepath.Join(binDir, "gt"), gtScript)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("PRIME_TOOL_CALL_LOG", logPath)
	t.Setenv("TMUX", "")
	primeDryRun = false

	return t.TempDir()
}

func writePrimeToolScript(t *testing.T, path, body string) {
	t.Helper()
	tool := filepath.Base(path)
	script := "#!/bin/sh\n" +
		"printf '%s\\n' '" + tool + ":'\"$*\" >> \"$PRIME_TOOL_CALL_LOG\"\n" +
		body + "\n" +
		"printf '%s\\n' 'unexpected args: '\"$*\" >&2\n" +
		"exit 99\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func assertPrimeToolCalled(t *testing.T, want string) {
	t.Helper()
	logPath := os.Getenv("PRIME_TOOL_CALL_LOG")
	data, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if !strings.Contains(string(data), want) {
		t.Fatalf("call log missing %q:\n%s", want, string(data))
	}
}

func assertPrimeToolNotCalled(t *testing.T, unwanted string) {
	t.Helper()
	logPath := os.Getenv("PRIME_TOOL_CALL_LOG")
	data, err := os.ReadFile(logPath)
	if os.IsNotExist(err) {
		// A log that was never written is the strongest possible evidence that
		// nothing was called: the stub tool appends on entry, before it can do
		// anything else. Roles for which prime runs no subprocess at all end up
		// here, so a missing log is a pass, not an error.
		return
	}
	if err != nil {
		t.Fatalf("read call log: %v", err)
	}
	if strings.Contains(string(data), unwanted) {
		t.Fatalf("call log unexpectedly has %q:\n%s", unwanted, string(data))
	}
}

// waitForPath polls for path to appear, returning whether it did within max.
func waitForPath(path string, max time.Duration) bool {
	deadline := time.Now().Add(max)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// withPrimeExternalToolDeadline replaces the wall-clock deadline that bounds
// prime's external tool subprocesses. Tests pass a context they cancel from a
// barrier, so "prime abandoned the tool at its deadline" becomes a property of
// the code rather than of how loaded the host was.
func withPrimeExternalToolDeadline(t *testing.T, fn func(time.Duration) (context.Context, context.CancelFunc)) {
	t.Helper()
	old := primeExternalToolContext
	primeExternalToolContext = fn
	t.Cleanup(func() { primeExternalToolContext = old })
}

// primeTestStallSeconds renders primeTestSlowToolStall for embedding in a stub script,
// so the shell's sleep and the test's safety net can never drift apart.
var primeTestStallSeconds = strconv.Itoa(int(primeTestSlowToolStall / time.Second))

// TestRunPrimeExternalTools_RunsMemoryAndMail pins that the two sections are
// independent: a role that renders memories still gets its mail, both in one
// call. The mayor is the role that gets both, so it is the subject here; the
// roles that get only one are covered by the two tests below.
func TestRunPrimeExternalTools_RunsMemoryAndMail(t *testing.T) {
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") printf '%s\n' '{"gt.feedback.test":"remembered"}'; exit 0 ;;
esac
`, `
case "$*" in
  "mail check --inject") printf '%s\n' 'MAIL OUTPUT'; exit 0 ;;
esac
`)

	output := captureStdout(t, func() { runPrimeExternalTools(RoleContext{Role: RoleMayor}, workDir) })
	assertPrimeToolCalled(t, "bd:kv list --json")
	assertPrimeToolCalled(t, "gt:mail check --inject")

	if !strings.Contains(output, "remembered") {
		t.Fatalf("memory injection missing: %q", output)
	}
	if !strings.Contains(output, "MAIL OUTPUT") {
		t.Fatalf("mail injection missing: %q", output)
	}
}

// TestRunPrimeExternalTools_MemoryIsMayorAndCrewOnly pins the memory role gate
// (gt-o51s, plan Task 8 C3). It is a separate test from
// SkipsMailCheckForPatrolRoles because the two gates are independent: mail is
// withheld from patrol roles, memories from everyone but mayor and crew, and a
// polecat is the role that gets exactly one of them.
func TestRunPrimeExternalTools_MemoryIsMayorAndCrewOnly(t *testing.T) {
	for _, tc := range []struct {
		role       Role
		wantMemory bool
	}{
		{RoleMayor, true},
		{RoleCrew, true},
		{RolePolecat, false},
		{RoleWitness, false},
		{RoleRefinery, false},
		{RoleDeacon, false},
		{RoleBoot, false},
	} {
		t.Run(string(tc.role), func(t *testing.T) {
			workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") printf '%s\n' '{"gt.feedback.test":"remembered"}'; exit 0 ;;
esac
`, `
case "$*" in
  "mail check --inject") printf '%s\n' 'MAIL OUTPUT'; exit 0 ;;
esac
`)

			output := captureStdout(t, func() { runPrimeExternalTools(RoleContext{Role: tc.role}, workDir) })

			if tc.wantMemory {
				assertPrimeToolCalled(t, "bd:kv list --json")
				if !strings.Contains(output, "remembered") {
					t.Fatalf("role %s rendered no memories: %q", tc.role, output)
				}
				return
			}
			assertPrimeToolNotCalled(t, "bd:kv list --json")
			if strings.Contains(output, "remembered") {
				t.Fatalf("role %s rendered a memory index it should not have: %q", tc.role, output)
			}
			// The gate is about memories only: withholding them must not take
			// the role's mail with it.
			if tc.role == RolePolecat {
				assertPrimeToolCalled(t, "gt:mail check --inject")
				if !strings.Contains(output, "MAIL OUTPUT") {
					t.Fatalf("polecat lost its mail along with its memories: %q", output)
				}
			}
		})
	}
}

// TestRunPrimeMemoryInject_BoundsTheIndex is the acceptance guard for the size
// half of gt-o51s: at the live corpus's scale the section a mayor's prime
// carries must fit inside memoryInjectMaxChars, which is what makes it fit
// inside primeHookBudget alongside the hooked work at all.
func TestRunPrimeMemoryInject_BoundsTheIndex(t *testing.T) {
	// The live corpus, in shape: 38 entries of ~1200 chars each.
	kvJSON, err := json.Marshal(syntheticMemories(38, 1200))
	if err != nil {
		t.Fatalf("marshal kv: %v", err)
	}
	jsonPath := filepath.Join(t.TempDir(), "kv.json")
	if err := os.WriteFile(jsonPath, kvJSON, 0600); err != nil {
		t.Fatalf("write kv json: %v", err)
	}
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") cat "$KV_JSON_FILE"; exit 0 ;;
esac
`, ``)
	t.Setenv("KV_JSON_FILE", jsonPath)

	out := captureStdout(t, func() {
		runPrimeMemoryInject(RoleContext{Role: RoleMayor}, workDir)
	})

	if !strings.Contains(out, "# Agent Memories (38)") {
		t.Fatalf("mayor rendered no index over a 38-entry corpus:\n%s", out[:min(len(out), 400)])
	}
	if len(out) > memoryInjectMaxChars+memoryIndexFooterSlack {
		t.Errorf("memory section = %d chars, want <= %d (cap %d + footer)",
			len(out), memoryInjectMaxChars+memoryIndexFooterSlack, memoryInjectMaxChars)
	}
	// The section is only useful if the whole of it is affordable: a bound that
	// still exceeded the hook budget would be no bound at all.
	if len(out) > primeHookBudget {
		t.Errorf("memory section = %d chars, over the %d-char hook budget by itself", len(out), primeHookBudget)
	}
}

// TestRunPrimeExternalTools_BoundsSlowMailCheck proves prime abandons a mail
// check that outlives its deadline instead of blocking startup on it.
//
// The deadline is driven by a barrier — the stub announces it has started, and
// that announcement cancels the context — not by host wall-clock time. An
// elapsed<N assertion measured the host, not the code: under gate load a
// correctly bounded prime still measured 2.687s and reddened the suite for a
// branch that touched no prime file (gt-v2a5). The barrier removes both
// failure modes at once: the stub is guaranteed to have run before the deadline
// fires, and the assertions are about what prime did with the stub, not about
// how fast the host was.
func TestRunPrimeExternalTools_BoundsSlowMailCheck(t *testing.T) {
	markerDir := t.TempDir()
	startedPath := filepath.Join(markerDir, "child-started")

	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") printf '%s\n' '{"gt.feedback.test":"remembered"}'; exit 0 ;;
esac
`, `
case "$*" in
  "mail check --inject")
    (: > "$PRIME_CHILD_STARTED"; sleep `+primeTestStallSeconds+`; : > "$PRIME_CHILD_SURVIVED") &
    wait
    exit 0
    ;;
esac
`)
	survivedPath := filepath.Join(markerDir, "child-survived")
	t.Setenv("PRIME_CHILD_STARTED", startedPath)
	t.Setenv("PRIME_CHILD_SURVIVED", survivedPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	withPrimeExternalToolDeadline(t, func(time.Duration) (context.Context, context.CancelFunc) {
		// The mailbox check is the only call still in flight once the stub has
		// announced itself — the memory injection that precedes it has already
		// returned — so cancelling this context is exactly the deadline firing.
		return context.WithCancel(ctx)
	})

	barrierReached := make(chan bool, 1)
	go func() {
		reached := waitForPath(startedPath, primeTestBarrierWait)
		cancel() // never leave prime blocked, even if the barrier never came
		barrierReached <- reached
	}()

	start := time.Now()
	// The mayor is the subject because this test asserts that a stalled mail
	// check leaves the memory section standing, which needs a role that renders
	// one (see shouldRenderMemories).
	output := captureStdout(t, func() { runPrimeExternalTools(RoleContext{Role: RoleMayor}, workDir) })
	elapsed := time.Since(start)

	if !<-barrierReached {
		t.Fatalf("mail stub never announced it started within %v — nothing can be concluded about prime's bound", primeTestBarrierWait)
	}

	// The stub was mid-stall when the deadline fired, so the call must have
	// returned without it: a prime that waited the tool out would have reaped
	// the child, and the child writes its survived marker before exiting.
	if _, err := os.Stat(survivedPath); err == nil {
		t.Fatalf("mail check child ran to completion — prime waited past its deadline instead of abandoning the tool")
	} else if !os.IsNotExist(err) {
		t.Fatalf("check survived marker: %v", err)
	}

	// Safety net rather than a bound: the stub stalls for primeTestSlowToolStall,
	// so only a prime that sat out the whole stall can approach it. Load cannot
	// fake this — it would have to inflate a sub-second call by a factor of ten.
	if elapsed > primeTestSlowToolStall {
		t.Fatalf("prime waited out the stalled mail check: elapsed = %v", elapsed)
	}

	assertPrimeToolCalled(t, "gt:mail check --inject")
	if !strings.Contains(output, "remembered") {
		t.Fatalf("a slow mail check must not suppress the memory section: %q", output)
	}
}

// TestRunPrimeExternalTools_SkipsMailCheckForPatrolRoles pins that a patrol
// role's prime runs no external tool: mail is withheld by
// shouldSkipStartupMailInject and memories by shouldRenderMemories. The bd
// assertion is the half that catches the memory gate landing without its tests
// (gt-o51s) — these roles have no other reason to shell out at all.
func TestRunPrimeExternalTools_SkipsMailCheckForPatrolRoles(t *testing.T) {
	for _, role := range []string{string(RoleWitness), string(RoleRefinery), string(RoleDeacon), string(RoleBoot)} {
		t.Run(role, func(t *testing.T) {
			workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") printf '%s\n' '{}'; exit 0 ;;
esac
`, `
case "$*" in
  "mail check --inject") printf '%s\n' 'MAIL OUTPUT'; exit 0 ;;
esac
`)

			output := captureStdout(t, func() { runPrimeExternalTools(RoleContext{Role: Role(role)}, workDir) })
			assertPrimeToolNotCalled(t, "bd:kv list --json")
			assertPrimeToolNotCalled(t, "gt:mail check --inject")
			if strings.Contains(output, "MAIL OUTPUT") {
				t.Fatalf("patrol role %s injected mail output: %q", role, output)
			}
			if strings.Contains(output, "# Agent Memories") {
				t.Fatalf("patrol role %s injected a memory index: %q", role, output)
			}
		})
	}
}

// TestCheckPendingEscalations_BoundsSlowBdList is the escalation-query sibling
// of TestRunPrimeExternalTools_BoundsSlowMailCheck: a bd list that outlives the
// deadline must be abandoned, not waited out, and its output must never be
// shown. Deadlines are barrier-driven for the same reason (gt-5clb: the stubbed
// bd never got to write within the old 1.5s bound on a loaded host).
func TestCheckPendingEscalations_BoundsSlowBdList(t *testing.T) {
	markerDir := t.TempDir()
	startedPath := filepath.Join(markerDir, "bd-started")

	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "list --status=open --label=gt:escalation --include-infra --json --flat")
    : > "$PRIME_BD_STARTED"
    sleep `+primeTestStallSeconds+`
    printf '%s\n' '[{"id":"hq-wisp1","title":"Dolt unreachable","priority":0,"labels":["gt:escalation"]}]'
    exit 0
    ;;
esac
`, `
`)
	t.Setenv("PRIME_BD_STARTED", startedPath)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	withPrimeExternalToolDeadline(t, func(time.Duration) (context.Context, context.CancelFunc) {
		return context.WithCancel(ctx)
	})

	barrierReached := make(chan bool, 1)
	go func() {
		reached := waitForPath(startedPath, primeTestBarrierWait)
		cancel()
		barrierReached <- reached
	}()

	start := time.Now()
	output := captureStdout(t, func() {
		checkPendingEscalations(RoleContext{Role: RoleMayor, WorkDir: workDir})
	})
	elapsed := time.Since(start)

	if !<-barrierReached {
		t.Fatalf("bd stub never announced it started within %v — nothing can be concluded about prime's bound", primeTestBarrierWait)
	}

	// The stub prints its payload only after the stall, so a payload that
	// reached prime's output means the query was not abandoned at the deadline.
	if strings.Contains(output, "PENDING ESCALATIONS") {
		t.Fatalf("abandoned escalation query still emitted output: %q", output)
	}
	if elapsed > primeTestSlowToolStall {
		t.Fatalf("prime waited out the stalled escalation query: elapsed = %v", elapsed)
	}

	assertPrimeToolCalled(t, "bd:list --status=open --label=gt:escalation --include-infra --json --flat")
}

// TestCheckPendingEscalations_SurfacesOpenEphemeralEscalation proves the
// mayor-startup check actually displays an open escalation end to end.
// Regression test for gt-fcsf: the original query used a nonexistent
// `--tag=escalation` flag, which made `bd list` error on every invocation
// (silently, by design, since the check is best-effort) — this had never
// once surfaced a real escalation. The fixed query uses --label=gt:escalation
// --include-infra, since escalations are ephemeral wisps that bd list hides
// by default.
func TestCheckPendingEscalations_SurfacesOpenEphemeralEscalation(t *testing.T) {
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "list --status=open --label=gt:escalation --include-infra --json --flat")
    printf '%s\n' '[{"id":"hq-wisp1","title":"Dolt unreachable","priority":0,"labels":["gt:escalation"]}]'
    exit 0
    ;;
esac
`, `
`)

	output := captureStdout(t, func() {
		checkPendingEscalations(RoleContext{Role: RoleMayor, WorkDir: workDir})
	})
	assertPrimeToolCalled(t, "bd:list --status=open --label=gt:escalation --include-infra --json --flat")

	if !strings.Contains(output, "PENDING ESCALATIONS") {
		t.Fatalf("expected escalation banner in output, got: %q", output)
	}
	if !strings.Contains(output, "hq-wisp1") && !strings.Contains(output, "1 escalation") {
		t.Fatalf("expected output to reflect the open escalation, got: %q", output)
	}
}

// TestCheckPendingEscalations_SkipsMailDeliveryBeads mirrors the dashboard
// fetcher's filtering (gt-kl7): escalation mail-delivery beads carry the same
// gt:escalation label so ack/close can find them, but they aren't escalation
// wisps themselves and must not inflate the startup count.
func TestCheckPendingEscalations_SkipsMailDeliveryBeads(t *testing.T) {
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "list --status=open --label=gt:escalation --include-infra --json --flat")
    printf '%s\n' '[{"id":"hq-wisp1","title":"Real escalation","priority":0,"labels":["gt:escalation"]},{"id":"hq-885m","title":"[HIGH] Real escalation","priority":0,"labels":["gt:escalation","gt:message"]}]'
    exit 0
    ;;
esac
`, `
`)

	output := captureStdout(t, func() {
		checkPendingEscalations(RoleContext{Role: RoleMayor, WorkDir: workDir})
	})

	if !strings.Contains(output, "1 escalation") {
		t.Fatalf("expected count to exclude the mail-delivery bead, got: %q", output)
	}
	if strings.Contains(output, "hq-885m") {
		t.Fatalf("mail-delivery bead should not appear in output: %q", output)
	}
}
