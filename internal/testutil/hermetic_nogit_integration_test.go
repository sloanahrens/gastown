//go:build integration && unix

package testutil

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationRefusedGitFailsTheRunWhenSwallowed runs the refusing git the
// way production code would, ignores its failure the way tolerant code does,
// and shows the run still fails with the call named (gt-et9zp).
func TestIntegrationRefusedGitFailsTheRunWhenSwallowed(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "it's nogit") // a quote in the path must survive the script
	log, err := writeRefusingGit(bin)
	if err != nil {
		t.Fatal(err)
	}
	repo := t.TempDir()
	cmd := exec.Command(filepath.Join(bin, "git"), "rev-parse", "--show-toplevel")
	cmd.Dir = repo
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err = cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 || !strings.Contains(stderr.String(), noGitMessage) {
		t.Fatalf("refusing git: err=%v stderr=%q; want exit 1 and the refusal message", err, stderr.String())
	}
	// The caller swallowed err. The log still carries the call.
	var report bytes.Buffer
	if code := failOnRefusedGit(0, log, &report); code != 1 {
		t.Fatalf("a swallowed refusal left the run passing (code %d):\n%s", code, report.String())
	}
	wantDir, _ := filepath.EvalSymlinks(repo)
	data, _ := os.ReadFile(log)
	if got := string(data); got != wantDir+"\tgit rev-parse --show-toplevel\n" && got != repo+"\tgit rev-parse --show-toplevel\n" {
		t.Errorf("log = %q, want %q", got, repo+"\tgit rev-parse --show-toplevel\n")
	}
	if !strings.Contains(report.String(), "git rev-parse --show-toplevel") {
		t.Errorf("report does not name the call:\n%s", report.String())
	}
}
