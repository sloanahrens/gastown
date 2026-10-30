package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
	"github.com/steveyegge/gastown/internal/tiersweep"
)

func TestMain(m *testing.M) { os.Exit(unittier.Main(m)) }

// TestMainProcessDepsAreWired: every seam the command hands the sweep is set, and
// the environment it passes children is the process's own.
func TestMainProcessDepsAreWired(t *testing.T) {
	t.Parallel()
	d := processDeps()
	if d.Host.Env == nil || d.Host.MkdirTemp == nil || d.Host.MkdirAll == nil {
		t.Errorf("the host is not fully wired: %+v", d.Host)
	}
	if d.Exec == nil || d.ReadFile == nil || d.Now == nil {
		t.Errorf("a seam is nil: exec %v, readFile %v, now %v", d.Exec != nil, d.ReadFile != nil, d.Now != nil)
	}
	if len(d.Environ) != len(os.Environ()) {
		t.Errorf("environment has %d entries, the process has %d", len(d.Environ), len(os.Environ()))
	}
}

// TestMainOutputStreams: through the command's wiring, a summary line is written
// to stdout only, and the progress line and the unknown-tier line to stderr
// only. The process's host and clock are replaced; nothing starts a process.
func TestMainOutputStreams(t *testing.T) {
	t.Parallel()
	logDir := t.TempDir()
	d := processDeps()
	d.Host = tiersweep.Host{
		Env:       func(name string) string { return map[string]string{tiersweep.EnvLogDir: logDir}[name] },
		MkdirTemp: func(string, string) (string, error) { return logDir, nil },
		MkdirAll:  func(string, os.FileMode) error { return nil },
	}
	d.Now = func() time.Time { return time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC) }
	d.Exec = func(dir string, env []string, argv []string, out io.Writer) int {
		if argv[0] == "gt" {
			return 0 // the race package passes
		}
		return 1
	}
	var stdout, stderr bytes.Buffer
	code := tiersweep.Run([]string{"race", "nope"}, &stdout, &stderr, d)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr %q)", code, stderr.String())
	}
	wantLine := "tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs " + logDir + ") in 0s\n"
	if stdout.String() != wantLine {
		t.Errorf("stdout = %q, want %q", stdout.String(), wantLine)
	}
	for _, want := range []string{"tier-sweep: race ./internal/cmd ...", "tier-sweep: unknown tier nope (want shell, integration or race)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
	if strings.Contains(stdout.String(), "...") || strings.Contains(stdout.String(), "unknown") {
		t.Errorf("progress or an error reached stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "race GREEN") {
		t.Errorf("a summary line reached stderr: %q", stderr.String())
	}
}
