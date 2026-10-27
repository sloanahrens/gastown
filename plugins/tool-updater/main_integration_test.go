//go:build integration

package main

import (
	"strings"
	"testing"
)

// TestIntegrationExecRun drives the real runner with real programs: the caller
// reads what the program wrote, both streams land in one buffer when asked,
// and a program that cannot start reports an error rather than an empty
// answer.
func TestIntegrationExecRun(t *testing.T) {
	t.Parallel()
	out, err := execRun("git", nil, false, "--version")
	if err != nil {
		t.Fatalf("execRun(git --version) = %v, want success", err)
	}
	if !strings.HasPrefix(out, "git version ") {
		t.Errorf("execRun(git --version) = %q, want git's version line", out)
	}

	missing := []string{"rev-parse", "--verify", "refs/heads/gt-tool-updater-no-such-ref"}
	combined, err := execRun("git", nil, true, missing...)
	if err == nil {
		t.Fatal("execRun(git rev-parse --verify <missing ref>) = nil, want a non-zero exit")
	}
	if combined == "" {
		t.Error("combined output of a failing program is empty; its stderr went missing")
	}
	if stdout, _ := execRun("git", nil, false, missing...); stdout != "" {
		t.Errorf("stdout-only output of a failing program = %q, want it to leave stderr out", stdout)
	}

	if _, err := execRun("gt-tool-updater-no-such-program", nil, false); err == nil {
		t.Fatal("execRun of a missing program = nil, want an error")
	}
}
