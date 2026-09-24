package cmd

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/nudge"
)

// TestDrainSessionNudgesNoTmuxSession verifies drainSessionNudges degrades to
// a no-op (nil, no error) when the caller isn't inside a tmux pane, which is
// the path any non-interactive test process takes. It must not shell out or
// fail just because TMUX_PANE is unset.
func TestDrainSessionNudgesNoTmuxSession(t *testing.T) {
	t.Setenv("TMUX_PANE", "")

	got := drainSessionNudges(t.TempDir())
	if got != nil {
		t.Errorf("expected nil with no tmux pane, got %v", got)
	}
}

// TestPrintSessionNudgesGoesToStderrNotStdout is the gt-dekkl regression: a
// queued nudge must reach the agent while staying out of the stdout that
// mol-refinery-patrol (MR=$(gt mq next <rig> --quiet)), stuck-work-dog and
// mol-boot-triage parse as machine output. On stdout the nudge block breaks
// the parse AND, because the drain already removed it from the queue, is lost
// with nothing legible delivered.
func TestPrintSessionNudgesGoesToStderrNotStdout(t *testing.T) {
	const session = "gt-gastown-refinery"
	fakeTmuxSession(t, session)
	t.Setenv("TMUX_PANE", "%1")
	townRoot := fakeTownRoot(t)

	if err := nudge.Enqueue(townRoot, session, nudge.QueuedNudge{
		Sender:  "mayor/",
		Message: "stop gating and read the P0 first",
	}); err != nil {
		t.Fatalf("enqueue nudge: %v", err)
	}

	stdout, stderr := captureStdio(t, printSessionNudges)

	if !strings.Contains(stderr, "stop gating and read the P0 first") {
		t.Errorf("drained nudge missing from stderr: %q", stderr)
	}
	if !strings.Contains(stderr, "<system-reminder>") {
		t.Errorf("stderr missing the injection block: %q", stderr)
	}
	if stdout != "" {
		t.Errorf("nudge leaked onto stdout, corrupting every machine reader of this command: %q", stdout)
	}
	// The nudge must actually have been drained, or the assertions above pass
	// vacuously against a queue this test never filled.
	if left := nudge.QueueLen(townRoot, session); left != 0 {
		t.Errorf("nudge queue still holds %d entr(ies) after the drain", left)
	}
}

// TestPrintSessionNudgesSilentWhenQueueEmpty pins the empty-queue path: a
// per-cycle command must print nothing at all, on either stream, when there
// is nothing queued.
func TestPrintSessionNudgesSilentWhenQueueEmpty(t *testing.T) {
	fakeTmuxSession(t, "gt-gastown-refinery")
	t.Setenv("TMUX_PANE", "%1")
	fakeTownRoot(t)

	stdout, stderr := captureStdio(t, printSessionNudges)

	if stdout != "" || stderr != "" {
		t.Errorf("empty queue produced output: stdout=%q stderr=%q", stdout, stderr)
	}
}

// TestPrintSessionNudgesNoTmuxSession verifies the helper degrades to a no-op
// when the caller isn't inside a tmux pane — same path drainSessionNudges
// guards, and the one case where there is no queue to drain at all.
func TestPrintSessionNudgesNoTmuxSession(t *testing.T) {
	t.Setenv("TMUX_PANE", "")
	fakeTownRoot(t)

	stdout, stderr := captureStdio(t, printSessionNudges)

	if stdout != "" || stderr != "" {
		t.Errorf("no tmux pane produced output: stdout=%q stderr=%q", stdout, stderr)
	}
}

// fakeTownRoot returns a directory that passes workspace.IsWorkspace and
// points GT_TOWN_ROOT at it, so findMailWorkDir resolves there instead of
// walking up into the live town this test runs inside.
func fakeTownRoot(t *testing.T) string {
	t.Helper()

	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatalf("create mayor marker dir: %v", err)
	}
	t.Setenv("GT_TOWN_ROOT", townRoot)
	return townRoot
}

// fakeTmuxSession installs a tmux shim on PATH whose display-message answers
// with sessionName, so tmux.CurrentSessionName resolves without a tmux server.
// Every other subcommand exits 0 silently.
func fakeTmuxSession(t *testing.T, sessionName string) {
	t.Helper()

	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	binDir := t.TempDir()
	script := "#!/bin/sh\n" +
		"for a in \"$@\"; do case \"$a\" in display-message) printf '%s\\n' '" + sessionName + "'; exit 0;; esac; done\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// captureStdio redirects os.Stdout and os.Stderr to pipes, calls fn, and
// returns what each received. Both read sides are drained concurrently so a
// pipe buffer cannot deadlock the writer.
func captureStdio(t *testing.T, fn func()) (stdout, stderr string) {
	t.Helper()

	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(stdout): %v", err)
	}
	errR, errW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe(stderr): %v", err)
	}

	// Restored on the way out even if fn panics: a leaked os.Stderr would
	// swallow every later test's output in this process.
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	defer func() { os.Stdout, os.Stderr = oldOut, oldErr }()

	var outBuf, errBuf bytes.Buffer
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(&outBuf, outR); done <- struct{}{} }()
	go func() { _, _ = io.Copy(&errBuf, errR); done <- struct{}{} }()

	fn()

	_ = outW.Close()
	_ = errW.Close()
	<-done
	<-done

	return outBuf.String(), errBuf.String()
}
