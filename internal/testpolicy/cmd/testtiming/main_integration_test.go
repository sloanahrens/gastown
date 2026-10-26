//go:build integration

package main

import (
	"context"
	"testing"
)

// TestIntegrationRunCommand drives the real runner with real processes: a
// program's exit status must reach exitCode, and a program that never starts
// must not look like one that ran and failed.
func TestIntegrationRunCommand(t *testing.T) {
	t.Parallel()
	if err := runCommand(context.Background(), command{name: "git", args: []string{"--version"}}); err != nil {
		t.Errorf("runCommand(git --version) = %v, want success", err)
	}
	err := runCommand(context.Background(), command{name: "git", args: []string{"rev-parse", "--verify", "refs/heads/gt-test-timing-no-such-ref"}})
	if err == nil {
		t.Fatal("runCommand(git rev-parse --verify <missing ref>) = nil, want a non-zero exit")
	}
	if code, ok := exitCode(err); !ok || code == 0 {
		t.Errorf("exitCode(%v) = %d, %v, want git's non-zero status", err, code, ok)
	}
	err = runCommand(context.Background(), command{name: "gt-test-timing-no-such-program"})
	if err == nil {
		t.Fatal("runCommand of a missing program = nil, want an error")
	}
	if code, ok := exitCode(err); ok {
		t.Errorf("a program that never started reported exit code %d: %v", code, err)
	}
}
