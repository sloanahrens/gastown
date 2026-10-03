package cmd

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDetectSenderFromCwdUsesAgentFileWitnessIdentity(t *testing.T) {
	t.Parallel()
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

	got := detectSenderWith(envMap(map[string]string{}), filepath.Join(witnessDir, "rig"))
	if got != "x267/witness" {
		t.Fatalf("detectSender() = %q, want %q", got, "x267/witness")
	}
}

func TestDetectSenderFromCwdUsesAgentFileRefineryIdentity(t *testing.T) {
	t.Parallel()
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

	got := detectSenderWith(envMap(map[string]string{}), filepath.Join(refineryDir, "rig"))
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
	t.Parallel()
	got := detectSenderWith(envMap(map[string]string{"GT_POLECAT": "live-fire"}), t.TempDir())
	if got != "live-fire" {
		t.Fatalf("detectSender() = %q, want %q (never overseer)", got, "live-fire")
	}
}

// TestDetectSenderPolecatWithoutRoleUsesRigWhenPresent covers the case where
// GT_RIG survives alongside a role-less GT_POLECAT (e.g. a debugging session
// with GT_ROLE manually unset): the rig-qualified address is preferred over
// the bare polecat name.
func TestDetectSenderPolecatWithoutRoleUsesRigWhenPresent(t *testing.T) {
	t.Parallel()
	got := detectSenderWith(envMap(map[string]string{"GT_RIG": "gastown", "GT_POLECAT": "granite"}), t.TempDir())
	if got != "gastown/granite" {
		t.Fatalf("detectSender() = %q, want %q", got, "gastown/granite")
	}
}

// TestDetectSenderDaemonActorNeverFallsToOverseer pins the environment the
// daemon gives every gt it forks: daemonGTEnv drops the identity variables
// and sets BD_ACTOR=daemon (gt-kyik6), so gt escalate run from there must
// attribute the escalation to the daemon, not to the human operator the cwd
// fallback names (gt-bw6ai). The daemon runs from the town root, which no
// agent-directory rule matches, so this is the exact shape that regressed.
func TestDetectSenderDaemonActorNeverFallsToOverseer(t *testing.T) {
	t.Parallel()
	got := detectSenderWith(envMap(map[string]string{"BD_ACTOR": "daemon"}), t.TempDir())
	if got != "daemon" {
		t.Fatalf("detectSender() = %q, want %q (never overseer)", got, "daemon")
	}
}

// TestDetectSenderAgentIdentityWinsOverDaemonActor: an agent session carries
// both its own role and, in some spawn paths, a BD_ACTOR set to that same
// address; the agent's own identity must win either way, so escalations
// raised by agents still record the agent (gt-bw6ai).
func TestDetectSenderAgentIdentityWinsOverDaemonActor(t *testing.T) {
	t.Parallel()
	got := detectSenderWith(envMap(map[string]string{
		"GT_ROLE":  "gastown/polecats/granite",
		"BD_ACTOR": "daemon",
	}), t.TempDir())
	if got != "gastown/polecats/granite" {
		t.Fatalf("detectSender() = %q, want the agent's own address", got)
	}
}

// TestDetectSenderPolecatWinsOverDaemonActor: the hooks live-fire probe keeps
// BD_ACTOR from the session it runs in while setting its synthetic GT_POLECAT
// (liveFireProbeEnv strips GT_ROLE and GT_RIG). The synthetic polecat must
// still win, or the probe would name whatever actor it inherited instead
// (gt-wyia, gt-bw6ai).
func TestDetectSenderPolecatWinsOverDaemonActor(t *testing.T) {
	t.Parallel()
	got := detectSenderWith(envMap(map[string]string{
		"GT_POLECAT": "live-fire",
		"BD_ACTOR":   "daemon",
	}), t.TempDir())
	if got != "live-fire" {
		t.Fatalf("detectSender() = %q, want %q", got, "live-fire")
	}
}
