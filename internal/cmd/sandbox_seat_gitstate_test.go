package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
)

const sandboxSeatTestSettings = `{"type":"town-settings","version":1,
"agents":{"deepseek-flash":{"command":"claude"},"deepseek-flash-sandbox":{"command":"claude"}},
"sandboxes":{"polecat-default":{"gateway":"gw1","image":"gastown-polecat:dev","policy":"deploy/polecat/policy.yaml"}},
"polecat_pool":{"seats":{
 "deepseek-flash":{"cap":1},
 "deepseek-flash-sandbox":{"cap":1,"backend":"openshell","sandbox":"polecat-default"}}}}`

const sandboxSeatAgent = "deepseek-flash-sandbox"

func sandboxSeatTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, "settings", "config.json"), []byte(sandboxSeatTestSettings), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// A sandbox seat's git state is the gateway's answer: the sandbox gone reads
// clean (what was pushed is what survived), a running one and a gateway that
// cannot be asked read unknown. A host seat, an unrecorded agent and a town
// with no seat map are not sandbox seats, and the gateway is never asked about
// them (gt-a0x2w).
func TestSandboxSeatProbe(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		agent      string
		exists     bool
		existsErr  error
		wantOK     bool
		wantSource string
		wantReason string
	}{
		{"sandbox_gone_reads_clean", sandboxSeatAgent, false, nil, true, polecat.GitStateSourceLive, ""},
		{"sandbox_running_is_unknown", sandboxSeatAgent, true, nil, true, polecat.GitStateSourceUnknown, "running sandbox"},
		{"gateway_error_is_unknown", sandboxSeatAgent, false, errors.New("connection refused"), true, polecat.GitStateSourceUnknown, "connection refused"},
		{"host_seat_is_not_a_sandbox", "deepseek-flash", false, nil, false, "", ""},
		{"unrecorded_agent_is_not_a_sandbox", "", false, nil, false, "", ""},
		{"agent_with_no_seat_is_not_a_sandbox", "claude-sonnet", false, nil, false, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var asked []string
			probe := sandboxSeatProbeFor(sandboxSeatTown(t), func(gateway, name string) (bool, error) {
				asked = append(asked, gateway+"|"+name)
				return tt.exists, tt.existsErr
			})

			got, ok := probe(tt.agent, "gt-ember")

			if ok != tt.wantOK {
				t.Fatalf("sandbox seat = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				if len(asked) != 0 {
					t.Errorf("gateway asked %v about a seat that is no sandbox seat", asked)
				}
				return
			}
			if len(asked) != 1 || asked[0] != "gw1|gt-ember" {
				t.Errorf("gateway asked %v, want one question about gt-ember on gw1", asked)
			}
			if got.Source != tt.wantSource || got.UnpushedCommits != 0 || got.Dirty || got.StashCount != 0 {
				t.Errorf("state = %+v, want source %q and nothing unpushed", got, tt.wantSource)
			}
			if tt.wantReason != "" && !strings.Contains(got.FailedReason, tt.wantReason) {
				t.Errorf("FailedReason = %q, want it to name %q", got.FailedReason, tt.wantReason)
			}
		})
	}
}

// The list inventory takes a sandbox seat's git state from the sandbox probe
// and never measures its stale host worktree, which here reports a commit that
// only the sandbox ever pushed from (gt-a0x2w).
func TestInventoryReadsASandboxSeatFromTheGateway(t *testing.T) {
	t.Parallel()
	stale := polecat.LiveGitState{Branch: "polecat/ember/gt-a+x", Source: polecat.GitStateSourceLive, UnpushedCommits: 1}
	tests := []struct {
		name       string
		sandbox    polecat.LiveGitState
		wantReason string // in the git state reason
	}{
		{"gone_reads_clean", polecat.LiveGitState{Branch: "polecat/ember/gt-a+x", Source: polecat.GitStateSourceLive}, ""},
		{"running_is_unknown", polecat.LiveGitState{Source: polecat.GitStateSourceUnknown, FailedReason: "git state lives in the running sandbox gt-ember"}, "running sandbox"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			env := polecatInventoryEnv{
				WorktreePath: filepath.Join("/rig", "polecats", "ember", "gastown"),
				probe: func(string, bool) polecat.LiveGitState {
					t.Error("the stale host worktree was measured for a sandbox seat")
					return stale
				},
				SandboxSeat: func(agent, sessionName string) (polecat.LiveGitState, bool) {
					if agent != sandboxSeatAgent || !strings.HasSuffix(sessionName, "ember") {
						t.Errorf("sandbox probe asked about agent %q session %q", agent, sessionName)
					}
					return tt.sandbox, true
				},
			}
			item := buildPolecatInventoryItem("gastown", "ember", &beads.AgentFields{
				AgentState: string(beads.AgentStateDone), CleanupStatus: string(polecat.CleanupClean),
				Agent: sandboxSeatAgent, Branch: "polecat/ember/gt-a+x",
			}, nil, polecatSessionSet{}, env)

			if strings.Contains(strings.Join(item.Disposition.Blockers, "\n"), "unpushed") {
				t.Errorf("a sandbox seat reads as holding unpushed work: %+v", item.Disposition)
			}
			if tt.wantReason != "" && !strings.Contains(item.GitStateReason, tt.wantReason) {
				t.Errorf("git state reason = %q, want it to name %q", item.GitStateReason, tt.wantReason)
			}
		})
	}
}

// A host seat is probed as before, and a list env without the sandbox probe (the
// capacity path) leaves every seat to the worktree probe.
func TestInventoryProbesAHostSeat(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"host_seat", "no_sandbox_probe"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			probed := false
			env := polecatInventoryEnv{
				WorktreePath: filepath.Join("/rig", "polecats", "ember", "gastown"),
				probe: func(string, bool) polecat.LiveGitState {
					probed = true
					return polecat.LiveGitState{Source: polecat.GitStateSourceLive, UnpushedCommits: 1}
				},
			}
			if name == "host_seat" {
				env.SandboxSeat = func(string, string) (polecat.LiveGitState, bool) { return polecat.LiveGitState{}, false }
			}
			item := buildPolecatInventoryItem("gastown", "ember", &beads.AgentFields{
				AgentState: string(beads.AgentStateDone), CleanupStatus: string(polecat.CleanupClean), Agent: "deepseek-flash",
			}, nil, polecatSessionSet{}, env)

			if !probed {
				t.Fatal("the host worktree was not probed")
			}
			if !strings.Contains(strings.Join(item.Disposition.Blockers, "\n"), "unpushed") {
				t.Errorf("the host seat's unpushed commit was lost: %+v", item.Disposition)
			}
		})
	}
}

// The sling pool reads a sandbox seat the same way: its stale worktree is never
// measured, so a gone sandbox does not hold the seat as unpushed work, and a
// running one or a gateway that cannot be asked reads as unknown.
func TestPoolSeatReadsASandboxSeatFromTheGateway(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		sandbox     polecat.LiveGitState
		wantUnknown bool
	}{
		{"gone_reads_clean", polecat.LiveGitState{Branch: "polecat/ember/gt-a+x", Source: polecat.GitStateSourceLive}, false},
		{"running_is_unknown", polecat.SandboxSeatGitState("", true, nil, "gt-ember"), true},
		{"gateway_error_is_unknown", polecat.SandboxSeatGitState("", false, errors.New("connection refused"), "gt-ember"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", "ember", "gastown"), 0o755); err != nil {
				t.Fatal(err)
			}
			fields := &beads.AgentFields{
				AgentState: string(beads.AgentStateIdle), CleanupStatus: string(polecat.CleanupClean),
				Agent: sandboxSeatAgent, Branch: "polecat/ember/gt-a+x",
			}
			reads := poolSeatReadsAnswering(t, fields, polecatActiveWorkEvidence{})
			reads.probe = func(string) polecat.LiveGitState {
				t.Error("the stale host worktree was measured for a sandbox seat")
				return polecat.LiveGitState{Source: polecat.GitStateSourceLive, UnpushedCommits: 1}
			}
			reads.sandboxSeat = func(agent, _ string) (polecat.LiveGitState, bool) {
				return tt.sandbox, agent == sandboxSeatAgent
			}

			state, err := poolPolecatSeatStateWith(town, "gastown", "ember", reads)
			if err != nil {
				t.Fatalf("poolPolecatSeatStateWith: %v", err)
			}
			blockers := strings.Join(state.Disposition.Blockers, "\n")
			if strings.Contains(blockers, "unpushed") {
				t.Errorf("a sandbox seat reads as holding unpushed work: %+v", state.Disposition)
			}
			if unknown := state.Disposition.GitStateSource == polecat.GitStateSourceUnknown; unknown != tt.wantUnknown {
				t.Errorf("git state unknown = %v, want %v: %+v", unknown, tt.wantUnknown, state.Disposition)
			}
		})
	}
}

// The production wiring hands the sandbox probe to the list env and to the pool
// reads: without it every seat falls back to its host worktree and the stale
// commits read as unpushed again (gt-a0x2w).
func TestSandboxSeatProbeIsWiredIntoTheListAndThePool(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if env := polecatListInventoryEnv(filepath.Join(town, "gastown"), "gastown", "ember", polecatMRIndex{}, nil, polecatSpawnFacts{}); env.SandboxSeat == nil {
		t.Error("polecatListInventoryEnv leaves the sandbox probe unset")
	}
	if reads := poolSeatReadsForTown(town); reads.sandboxSeat == nil {
		t.Error("poolSeatReadsForTown leaves the sandbox probe unset")
	}
}

// A sandbox with a registry record is answered from the record: the state lives
// in the running sandbox and the gateway is not asked (the list path is
// network-free, gt-8q0s).
func TestSandboxSeatProbeAnswersFromTheRegistryWithoutTheGateway(t *testing.T) {
	t.Parallel()
	town := sandboxSeatTown(t)
	if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: "gt-ember", Rig: "gastown", Polecat: "ember"}); err != nil {
		t.Fatal(err)
	}
	probe := sandboxSeatProbeFor(town, func(string, string) (bool, error) {
		t.Error("the gateway was asked about a sandbox with a registry record")
		return true, nil
	})

	got, ok := probe(sandboxSeatAgent, "gt-ember")

	if !ok || got.Source != polecat.GitStateSourceUnknown || !strings.Contains(got.FailedReason, "running sandbox") {
		t.Fatalf("probe = %+v, %v; want a sandbox seat whose state is unknown because the sandbox is running", got, ok)
	}
}

// Classifying a seat reads the town settings and never creates them.
func TestSandboxSeatProbeDoesNotWriteTheTownSettings(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	if _, ok := sandboxSeatProbeFor(town, nil)(sandboxSeatAgent, "gt-ember"); ok {
		t.Error("a town with no seat map has a sandbox seat")
	}

	if entries, err := os.ReadDir(town); err != nil || len(entries) != 0 {
		t.Errorf("the probe left %v (err %v) in a town with no settings", entries, err)
	}
}
