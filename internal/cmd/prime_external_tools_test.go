package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func setupPrimeExternalToolTest(t *testing.T, bdScript, gtScript string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script subprocess test")
	}
	tmpDir := t.TempDir()
	logPath := filepath.Join(tmpDir, "calls.log")

	oldTimeout := primeExternalToolTimeout
	oldWaitDelay := primeExternalToolWaitDelay
	// 1.5s/150ms rather than a tighter bound: under host contention (many
	// concurrent agent subprocesses) even a trivial script can take >100ms
	// to spawn. This was previously tuned to 500ms/50ms (gt-80o), but that
	// still wasn't generous enough: reproduced under stress (`-count=200`
	// on a loaded dev box) as calls.log never being created at all because
	// the subprocess spawn itself didn't complete before the context
	// deadline (gt-0nk) — not, as first suspected, tests reading live-town
	// mail state. Dependent tests below use hardcoded 2s slow-path
	// sleeps/assertElapsedUnder bounds that assume
	// primeExternalToolTimeout+primeExternalToolWaitDelay stays under 2s;
	// keep headroom against that if tuning further.
	primeExternalToolTimeout = 1500 * time.Millisecond
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

func assertElapsedUnder(t *testing.T, elapsed time.Duration, max time.Duration) {
	t.Helper()
	if elapsed > max {
		t.Fatalf("elapsed = %v, want under %v", elapsed, max)
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

func TestRunPrimeExternalTools_RunsMemoryAndMail(t *testing.T) {
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") printf '%s\n' '{"memory.feedback.test":"remembered"}'; exit 0 ;;
esac
`, `
case "$*" in
  "mail check --inject") printf '%s\n' 'MAIL OUTPUT'; exit 0 ;;
esac
`)

	start := time.Now()
	output := captureStdout(t, func() { runPrimeExternalTools(RoleContext{Role: RolePolecat}, workDir) })
	assertElapsedUnder(t, time.Since(start), 2*time.Second)
	assertPrimeToolCalled(t, "bd:kv list --json")
	assertPrimeToolCalled(t, "gt:mail check --inject")

	if !strings.Contains(output, "remembered") {
		t.Fatalf("memory injection missing: %q", output)
	}
	if !strings.Contains(output, "MAIL OUTPUT") {
		t.Fatalf("mail injection missing: %q", output)
	}
}

func TestRunPrimeExternalTools_BoundsSlowMailCheck(t *testing.T) {
	markerDir := t.TempDir()
	startedPath := filepath.Join(markerDir, "child-started")
	survivedPath := filepath.Join(markerDir, "child-survived")
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
  "kv list --json") printf '%s\n' '{"memory.feedback.test":"remembered"}'; exit 0 ;;
esac
`, `
case "$*" in
  "mail check --inject")
    (: > "$PRIME_CHILD_STARTED"; sleep 2; : > "$PRIME_CHILD_SURVIVED") &
    while [ ! -f "$PRIME_CHILD_STARTED" ]; do sleep 0.01; done
    wait
    exit 0
    ;;
esac
`)
	t.Setenv("PRIME_CHILD_STARTED", startedPath)
	t.Setenv("PRIME_CHILD_SURVIVED", survivedPath)

	start := time.Now()
	output := captureStdout(t, func() { runPrimeExternalTools(RoleContext{Role: RolePolecat}, workDir) })
	assertElapsedUnder(t, time.Since(start), 2*time.Second)
	assertPrimeToolCalled(t, "bd:kv list --json")
	assertPrimeToolCalled(t, "gt:mail check --inject")

	if !strings.Contains(output, "remembered") {
		t.Fatalf("memory output missing: %q", output)
	}
	if _, err := os.Stat(startedPath); err != nil {
		t.Fatalf("child did not start before timeout: %v", err)
	}

	time.Sleep(2 * time.Second)
	if _, err := os.Stat(survivedPath); err == nil {
		t.Fatalf("child process survived command timeout and wrote %s", survivedPath)
	} else if !os.IsNotExist(err) {
		t.Fatalf("check survived marker: %v", err)
	}
}

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
			assertPrimeToolCalled(t, "bd:kv list --json")
			logData, err := os.ReadFile(os.Getenv("PRIME_TOOL_CALL_LOG"))
			if err != nil {
				t.Fatalf("read call log: %v", err)
			}
			if strings.Contains(string(logData), "gt:mail check --inject") {
				t.Fatalf("patrol role %s should not run startup mail check:\n%s", role, string(logData))
			}
			if strings.Contains(output, "MAIL OUTPUT") {
				t.Fatalf("patrol role %s injected mail output: %q", role, output)
			}
		})
	}
}

func TestCheckPendingEscalations_BoundsSlowBdList(t *testing.T) {
	workDir := setupPrimeExternalToolTest(t, `
case "$*" in
	  "list --status=open --label=gt:escalation --include-infra --json --flat") sleep 2; exit 0 ;;
esac
`, `
`)

	start := time.Now()
	output := captureStdout(t, func() {
		checkPendingEscalations(RoleContext{Role: RoleMayor, WorkDir: workDir})
	})
	assertElapsedUnder(t, time.Since(start), 2*time.Second)
	assertPrimeToolCalled(t, "bd:list --status=open --label=gt:escalation --include-infra --json --flat")

	if strings.Contains(output, "PENDING ESCALATIONS") {
		t.Fatalf("timed-out escalation output should not be emitted: %q", output)
	}
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
