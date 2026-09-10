package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// TestRunPreVerificationGates guards the om-gate T8 fix: `gt done
// --pre-verified` must perform the verification it stamps, not merely trust
// the flag. A red gate must report which gate failed and produce no
// stamp-worthy log hash; a green run over multiple gates must run all of
// them, in order, and produce a stable log hash.
func TestRunPreVerificationGates(t *testing.T) {
	t.Run("failing gate reports its name and exit code, no success", func(t *testing.T) {
		dir := t.TempDir()
		mq := &config.MergeQueueConfig{TestCommand: "false"}

		result, err := runPreVerificationGates(dir, mq)
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if result.success {
			t.Error("success = true, want false for a failing gate command")
		}
		if result.failedGate != "test" {
			t.Errorf("failedGate = %q, want %q", result.failedGate, "test")
		}
		if result.exitCode != 1 {
			t.Errorf("exitCode = %d, want 1", result.exitCode)
		}
		if result.logSHA256 != "" {
			t.Error("logSHA256 should be empty when the run did not succeed")
		}
		if _, statErr := os.Stat(filepath.Join(dir, ".gt-preverify.log")); statErr != nil {
			t.Errorf(".gt-preverify.log should exist after a failed run: %v", statErr)
		}
	})

	t.Run("all-zero exit runs every configured gate and produces a log hash", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "ran")
		mq := &config.MergeQueueConfig{
			SetupCommand: "echo setup >> " + marker,
			LintCommand:  "echo lint >> " + marker,
			TestCommand:  "echo test >> " + marker,
		}

		result, err := runPreVerificationGates(dir, mq)
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if !result.success {
			t.Fatalf("success = false, want true: failedGate=%q exitCode=%d", result.failedGate, result.exitCode)
		}
		if result.exitCode != 0 {
			t.Errorf("exitCode = %d, want 0", result.exitCode)
		}
		if result.logSHA256 == "" {
			t.Error("logSHA256 should be non-empty on success")
		}

		got, readErr := os.ReadFile(marker)
		if readErr != nil {
			t.Fatalf("reading marker file: %v", readErr)
		}
		want := "setup\nlint\ntest\n"
		if string(got) != want {
			t.Errorf("gates ran in wrong order or not all ran: got %q want %q", string(got), want)
		}
	})

	t.Run("stops at the first failing gate, later gates never run", func(t *testing.T) {
		dir := t.TempDir()
		marker := filepath.Join(dir, "ran")
		mq := &config.MergeQueueConfig{
			SetupCommand: "echo setup >> " + marker,
			LintCommand:  "false",
			TestCommand:  "echo test >> " + marker,
		}

		result, err := runPreVerificationGates(dir, mq)
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if result.success {
			t.Fatal("success = true, want false")
		}
		if result.failedGate != "lint" {
			t.Errorf("failedGate = %q, want %q", result.failedGate, "lint")
		}

		got, readErr := os.ReadFile(marker)
		if readErr != nil {
			t.Fatalf("reading marker file: %v", readErr)
		}
		if string(got) != "setup\n" {
			t.Errorf("expected only setup to have run, got %q", string(got))
		}
	})

	t.Run("no configured gate commands is a no-op success", func(t *testing.T) {
		dir := t.TempDir()
		result, err := runPreVerificationGates(dir, &config.MergeQueueConfig{})
		if err != nil {
			t.Fatalf("runPreVerificationGates: %v", err)
		}
		if !result.success {
			t.Error("success = false, want true when there are no gates to run")
		}
	})
}
