package main

import (
	"bytes"
	"fmt"
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
	if d.Exec == nil || d.BuildExec == nil || d.ReadFile == nil || d.Now == nil {
		// A nil BuildExec would run the sweep's one go build with no deadline.
		t.Errorf("a seam is nil: exec %v, buildExec %v, readFile %v, now %v",
			d.Exec != nil, d.BuildExec != nil, d.ReadFile != nil, d.Now != nil)
	}
	if len(d.Environ) != len(os.Environ()) {
		t.Errorf("environment has %d entries, the process has %d", len(d.Environ), len(os.Environ()))
	}
}

// TestMainOutputStreams: through the command's wiring, summary lines are written
// to stdout only, and the progress lines and the unknown-tier line to stderr
// only. The process's host, clock and file reads are replaced, and the fake Exec
// answers each command by its argv: `git ls-files` with the listing, a go test
// (gt slot run) and the container trigger with success. Any other command fails
// the test, so nothing runs unannounced.
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
	// The sweep's go build goes through the fake Exec below: this test is about
	// which stream each line reaches, and the real seam would start a build.
	d.BuildExec = nil
	d.ReadFile = func(name string) ([]byte, error) {
		if strings.HasSuffix(name, "r/r_test.go") {
			return []byte("//go:build integration\n\npackage r\n"), nil
		}
		return []byte("# no container packages\n"), nil // docker.txt
	}
	d.Exec = func(dir string, env []string, argv []string, out io.Writer) int {
		switch {
		case argv[0] == "git" && argv[1] == "ls-files":
			if argv[len(argv)-1] == "*_test.go" {
				fmt.Fprintln(out, "r/r_test.go")
			}
			return 0
		case argv[0] == "git", argv[0] == "gt", argv[0] == "go":
			return 0 // rev-parse, the go tests and the trigger
		}
		t.Errorf("unexpected command %q", argv)
		return 1
	}
	var stdout, stderr bytes.Buffer
	code := tiersweep.Run([]string{"integration", "race", "nope"}, &stdout, &stderr, d)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2 (stderr %q)", code, stderr.String())
	}
	wantOut := "tier-sweep: integration GREEN passed=1 failed=0 skipped=0 (logs " + logDir + ") in 0s\n" +
		"tier-sweep: race GREEN passed=1 failed=0 skipped=0 (logs " + logDir + ") in 0s\n"
	if stdout.String() != wantOut {
		t.Errorf("stdout = %q, want %q", stdout.String(), wantOut)
	}
	for _, want := range []string{"tier-sweep: integration ./r ...", "tier-sweep: race ./internal/cmd ...", "tier-sweep: unknown tier nope (want shell, integration or race)"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr = %q, want it to contain %q", stderr.String(), want)
		}
	}
	if strings.Contains(stdout.String(), "...") || strings.Contains(stdout.String(), "unknown") {
		t.Errorf("progress or an error reached stdout: %q", stdout.String())
	}
	if strings.Contains(stderr.String(), "GREEN") {
		t.Errorf("a summary line reached stderr: %q", stderr.String())
	}
}
