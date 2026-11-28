package cmd

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/rig"
)

// A missing cleanup_status is reusable only as the one blocker with every other
// fact safe on a bead that is not terminal; each refusal row breaks one of them.
func TestReusableSeatBlockedOnlyByCleanup(t *testing.T) {
	t.Parallel()
	missing := []string{cleanupStatusBlocker("")}
	tests := []struct {
		name                                          string
		status                                        polecat.CleanupStatus
		reasons                                       []string
		hookSafe, activeMRSafe, gitSafe, beadTerminal bool
		want                                          bool
	}{
		{name: "only its blocker, safe facts", reasons: missing, hookSafe: true, activeMRSafe: true, gitSafe: true, want: true},
		{name: "unknown status", status: polecat.CleanupUnknown, reasons: []string{cleanupStatusBlocker(polecat.CleanupUnknown)}, hookSafe: true, activeMRSafe: true, gitSafe: true},
		{name: "second reason", reasons: append([]string{"has open MR (mr-1)"}, missing...), hookSafe: true, activeMRSafe: true, gitSafe: true},
		{name: "unsafe hook", reasons: missing, activeMRSafe: true, gitSafe: true},
		{name: "unsafe active MR", reasons: missing, hookSafe: true, gitSafe: true},
		{name: "unsafe git", reasons: missing, hookSafe: true, activeMRSafe: true},
		{name: "terminal bead", reasons: missing, hookSafe: true, activeMRSafe: true, gitSafe: true, beadTerminal: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := reusableSeatBlockedOnlyByCleanup(tt.status, tt.reasons, tt.hookSafe, tt.activeMRSafe, tt.gitSafe, tt.beadTerminal); got != tt.want {
				t.Fatalf("reusableSeatBlockedOnlyByCleanup = %v, want %v", got, tt.want)
			}
		})
	}
}

// A seat with a missing cleanup_status on an open bead, whose branch is pushed,
// is reusable when its git is safe; the refusal keeps its blocker either way.
func TestCheckPolecatSafetyReusable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		running      bool
		wantReusable bool
	}{
		{name: "stopped sandbox is reusable", wantReusable: true},
		{name: "unmeasured running sandbox is not", running: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			r := &rig.Rig{Name: "gastown", Path: filepath.Join(town, "gastown")}
			fake := mrlessBeads{beadsfake.New()}
			agentID := polecatBeadIDForRig(r, "gastown", "ember")
			if _, err := fake.Create(beads.CreateOptions{
				ID: agentID, Title: agentID, Labels: []string{"gt:agent"}, Priority: -1,
				Description: beads.FormatAgentDescription(agentID, &beads.AgentFields{
					RoleType: "polecat", Rig: "gastown", AgentState: string(beads.AgentStateSpawning),
					Agent: sandboxSeatAgent, Branch: "polecat/ember/gt-a+x", LastSourceIssue: "gt-work",
				}),
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := fake.Create(beads.CreateOptions{ID: "gt-work", Title: "work", Priority: 2}); err != nil {
				t.Fatal(err)
			}
			env := sandboxSeatEnv{
				probe: func(string, string) (polecat.LiveGitState, bool) {
					return polecat.SandboxSeatGitState("", tt.running, nil, "gt-ember"), true
				},
				hostGitState: gatewayOnlySeat,
				remoteBranch: func(string, string) (bool, error) { return true, nil },
			}
			get := func(string) (*polecat.Polecat, error) {
				return &polecat.Polecat{Name: "ember", Branch: "polecat/ember/gt-a+x", ClonePath: filepath.Join(town, "gastown", "polecats", "ember", "gastown")}, nil
			}

			result := checkPolecatSafetyWith("gastown", "ember", r, get, fake, env)

			if !result.Blocked || len(result.Reasons) != 1 || result.Reasons[0] != "cleanup_status=<missing>" {
				t.Fatalf("blocked = %v reasons = %q, want exactly [cleanup_status=<missing>]", result.Blocked, strings.Join(result.Reasons, " | "))
			}
			if result.Reusable != tt.wantReusable {
				t.Errorf("Reusable = %v, want %v", result.Reusable, tt.wantReusable)
			}
		})
	}
}
