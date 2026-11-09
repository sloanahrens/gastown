package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/ci/preflight"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

const (
	green     = "0123456789abcdef0123456789abcdef01234567"
	tag       = "0123456789ab"
	mango     = "fedcba9876543210fedcba9876543210fedcba98"
	mangoURL  = "http://forgejo:3000/example-owner/mango.git"
	forgejo   = "forgejo:3000"
	envFile   = "env"
	noSuchJQ  = "no-such-jq"
	envOutput = "SHA=" + green + "\nTAG=" + tag + "\n"
)

// installed answers the tool lookup with a hit for every tool.
func installed(string) (string, error) { return "/usr/local/bin/tool", nil }

// jobArgs is the command line the workflow passes for a passing job.
func jobArgs(envPath string) []string {
	return []string{
		"-sha", green,
		"-mango-url", mangoURL,
		"-mango-commit", mango,
		"-host", forgejo,
		"-env-out", envPath,
	}
}

// greenVersion answers the version read with the base image's jq.
func greenVersion() (string, error) { return preflight.JQVersion, nil }

// TestRunAcceptsTheGreenCommit holds the passing branch: the command exits
// zero and hands the later steps the commit and its tag.
func TestRunAcceptsTheGreenCommit(t *testing.T) {
	t.Parallel()
	envPath := filepath.Join(t.TempDir(), envFile)
	var stdout, stderr strings.Builder

	if code := run(jobArgs(envPath), &stdout, &stderr, installed, greenVersion); code != 0 {
		t.Fatalf("run exited %d, stderr %q", code, stderr.String())
	}
	got, err := os.ReadFile(envPath)
	if err != nil {
		t.Fatalf("reading the environment file: %v", err)
	}
	if string(got) != envOutput {
		t.Errorf("the environment file holds %q, want %q", got, envOutput)
	}
	if !strings.Contains(stdout.String(), tag) {
		t.Errorf("the command printed %q, want the tag %s", stdout.String(), tag)
	}
}

// TestRunRefusesWhatThePreflightRefuses holds the refusal branch: a check the
// preflight fails exits non-zero, and the environment file carries no commit.
func TestRunRefusesWhatThePreflightRefuses(t *testing.T) {
	t.Parallel()
	envPath := filepath.Join(t.TempDir(), envFile)
	var stdout, stderr strings.Builder

	code := run(jobArgs(envPath), &stdout, &stderr, installed, func() (string, error) {
		return "jq-1.6", nil
	})
	if code == 0 {
		t.Fatalf("run passed with another jq, stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "jq-1.6") {
		t.Errorf("the command wrote %q to stderr, want the version it found", stderr.String())
	}
	if got, err := os.ReadFile(envPath); err != nil {
		t.Fatalf("reading the environment file: %v", err)
	} else if len(got) != 0 {
		t.Errorf("the environment file holds %q for a refused job", got)
	}
}

// TestRunRefusesACommandLineWithoutTheEnvFile holds the flag rule: the path
// the later steps read is required, and the command says so.
func TestRunRefusesACommandLineWithoutTheEnvFile(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder

	code := run([]string{"-sha", green}, &stdout, &stderr, installed, greenVersion)
	if code == 0 {
		t.Fatalf("run passed without -env-out, stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "-env-out") {
		t.Errorf("the command wrote %q to stderr, want it to name -env-out", stderr.String())
	}
}

// TestRunRefusesAnEnvFileItCannotOpen holds the fail-closed branch: a file the
// runner cannot open fails the step instead of reporting a preflight it did
// not record.
func TestRunRefusesAnEnvFileItCannotOpen(t *testing.T) {
	t.Parallel()
	var stdout, stderr strings.Builder
	envPath := filepath.Join(t.TempDir(), "absent", envFile)

	if code := run(jobArgs(envPath), &stdout, &stderr, installed, greenVersion); code == 0 {
		t.Fatalf("run passed with an environment file it could not open, stdout %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), envPath) {
		t.Errorf("the command wrote %q to stderr, want it to name %s", stderr.String(), envPath)
	}
}

// TestJQVersionRefusesAProgramThatIsNotThere holds the version read's failure
// branch: a jq that does not run is an error, not an empty version.
func TestJQVersionRefusesAProgramThatIsNotThere(t *testing.T) {
	t.Parallel()
	if got, err := jqVersion(filepath.Join(t.TempDir(), noSuchJQ)); err == nil {
		t.Fatalf("jqVersion answered %q for a program that is not there", got)
	}
}
