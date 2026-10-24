package daemon

import (
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/polecat"
)

// A sandbox seat's host worktree is a stale leftover, so patrol's git state for
// it comes from the gateway, never from the host probe (gt-b4waf): a sandbox
// the gateway says is gone reads clean (what was pushed is what survived), a
// running one and a gateway that cannot be asked are unknown, and none of them
// reaches the host worktree. A host seat still goes through the probe, also when
// the lookup that tells the two apart fails.
func TestPatrolScanGitStateOfASandboxSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		agent     string
		agentErr  error // the agent bead cannot be read
		exists    bool
		existsErr error
		wantErr   string // substring of the error; "" for an answer
		wantProbe bool
	}{
		{"sandbox_gone_reads_clean", "deepseek-flash-sandbox", nil, false, nil, "", false},
		{"sandbox_running_is_unknown", "deepseek-flash-sandbox", nil, true, nil, "running sandbox", false},
		{"gateway_error_is_unknown", "deepseek-flash-sandbox", nil, false, errors.New("connection refused"), "connection refused", false},
		{"host_seat_is_probed", "deepseek-flash", nil, false, nil, "", true},
		{"unreadable_agent_bead_probes_the_host_worktree", "", errors.New("bd down"), false, nil, "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTownFile(t, town, "settings/config.json", staleHookSettings)
			probed := false
			var asked []string
			h := &patrolScanHost{
				d:           &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)},
				agentRecord: func(string, string) (string, error) { return tt.agent, tt.agentErr },
				gitState: func(string) polecat.LiveGitState {
					probed = true
					// The stale worktree of a sandbox seat: one commit that only the sandbox
					// ever pushed from.
					return polecat.LiveGitState{Source: polecat.GitStateSourceLive, Branch: "polecat/x/gt-a+x", UnpushedCommits: 1}
				},
				sandboxExists: func(gateway, name string) (bool, error) {
					asked = append(asked, gateway+"|"+name)
					return tt.exists, tt.existsErr
				},
			}
			if tt.wantProbe {
				// A host seat's worktree, found the way the probe path finds it.
				writeTownFile(t, town, staleHookRig+"/polecats/"+staleHookPolecat+"/.git", "gitdir: /elsewhere\n")
			}

			got, err := h.GitState(staleHookRig, staleHookPolecat)

			if probed != tt.wantProbe {
				t.Fatalf("host probe ran = %v, want %v", probed, tt.wantProbe)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("GitState error = %v, want one naming %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("GitState: %v", err)
			}
			wantUnpushed := 0
			if tt.wantProbe {
				wantUnpushed = 1
			}
			if got.UnpushedCommits != wantUnpushed || got.Dirty || got.StashCount != 0 {
				t.Errorf("GitState = %+v, want unpushed %d and nothing else", got, wantUnpushed)
			}
			if tt.agent == "deepseek-flash-sandbox" && (len(asked) != 1 || asked[0] != "gw1|"+staleHookSession) {
				t.Errorf("gateway asked %v, want one question about %s on gw1", asked, staleHookSession)
			}
		})
	}
}
