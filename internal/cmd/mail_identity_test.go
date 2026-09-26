package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectSenderFromCwdUsesAgentFileWitnessIdentity(t *testing.T) {
	t.Setenv("GT_ROLE", "")
	t.Setenv("GT_RIG", "")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")

	tmp := t.TempDir()
	witnessDir := filepath.Join(tmp, "x267", "witness")
	if err := os.MkdirAll(filepath.Join(witnessDir, "rig"), 0o755); err != nil {
		t.Fatalf("mkdir witness dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(witnessDir, ".gt-agent"),
		[]byte(`{"role":"witness","rig":"x267"}`),
		0o644,
	); err != nil {
		t.Fatalf("write .gt-agent: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(filepath.Join(witnessDir, "rig")); err != nil {
		t.Fatalf("chdir witness rig dir: %v", err)
	}

	got := detectSender()
	if got != "x267/witness" {
		t.Fatalf("detectSender() = %q, want %q", got, "x267/witness")
	}
}

func TestDetectSenderFromCwdUsesAgentFileRefineryIdentity(t *testing.T) {
	t.Setenv("GT_ROLE", "")
	t.Setenv("GT_RIG", "")
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_CREW", "")

	tmp := t.TempDir()
	refineryDir := filepath.Join(tmp, "x267", "refinery")
	if err := os.MkdirAll(filepath.Join(refineryDir, "rig"), 0o755); err != nil {
		t.Fatalf("mkdir refinery dir: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(refineryDir, ".gt-agent"),
		[]byte(`{"role":"refinery","rig":"x267"}`),
		0o644,
	); err != nil {
		t.Fatalf("write .gt-agent: %v", err)
	}

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(filepath.Join(refineryDir, "rig")); err != nil {
		t.Fatalf("chdir refinery rig dir: %v", err)
	}

	got := detectSender()
	if got != "x267/refinery" {
		t.Fatalf("detectSender() = %q, want %q", got, "x267/refinery")
	}
}

// TestDetectSenderPolecatWithoutRoleNeverFallsToOverseer pins the hooks
// live-fire probe's exact env shape (GT_ROLE and GT_RIG stripped, GT_POLECAT
// set — see liveFireProbeEnv in internal/doctor/hooks_live_fire_check.go):
// detectSender must never fall through to detectSenderFromCwd's "overseer"
// default here, or a probe running from its disposable sandbox cwd would
// have `gt mail check --inject` read and ACK the real human operator's mail
// (gt-wyia).
func TestDetectSenderPolecatWithoutRoleNeverFallsToOverseer(t *testing.T) {
	t.Setenv("GT_ROLE", "")
	t.Setenv("GT_RIG", "")
	t.Setenv("GT_POLECAT", "live-fire")
	t.Setenv("GT_CREW", "")

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir sandbox dir: %v", err)
	}

	got := detectSender()
	if got != "live-fire" {
		t.Fatalf("detectSender() = %q, want %q (never overseer)", got, "live-fire")
	}
}

// TestDetectSenderPolecatWithoutRoleUsesRigWhenPresent covers the case where
// GT_RIG survives alongside a role-less GT_POLECAT (e.g. a debugging session
// with GT_ROLE manually unset): the rig-qualified address is preferred over
// the bare polecat name.
func TestDetectSenderPolecatWithoutRoleUsesRigWhenPresent(t *testing.T) {
	t.Setenv("GT_ROLE", "")
	t.Setenv("GT_RIG", "gastown")
	t.Setenv("GT_POLECAT", "granite")
	t.Setenv("GT_CREW", "")

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatalf("chdir sandbox dir: %v", err)
	}

	got := detectSender()
	if got != "gastown/granite" {
		t.Fatalf("detectSender() = %q, want %q", got, "gastown/granite")
	}
}
