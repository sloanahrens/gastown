package polecat

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/config"
)

// A sandbox seat's host worktree is a stale leftover, so the reuse gate takes
// the seat's git facts from the sandbox probe and never measures the host clone,
// where a commit only the sandbox ever pushed from reads as unpushed work. A
// sandbox the gateway says is gone reads clean (its pushed work is the recovery
// verdict's to judge, gt-qq6rg), a running one or a gateway that cannot be
// asked reads unknown (never clean), and a host seat is still measured
// (gt-qq6rg.1).
func TestWorkstateDispositionForASandboxSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		agent         string
		probe         func(agent, session string) (LiveGitState, bool)
		wantHostBlock bool   // the host clone's commit (on no remote, with no MR) blocks the seat
		wantReason    string // non-empty: the git state is unknown and the reason contains this
	}{
		{"sandbox_gone_reads_clean", "deepseek-flash-sandbox",
			func(string, string) (LiveGitState, bool) { return SandboxSeatGitState("", false, nil, "sb"), true }, false, ""},
		{"sandbox_running_is_unknown", "deepseek-flash-sandbox",
			func(string, string) (LiveGitState, bool) { return SandboxSeatGitState("", true, nil, "sb"), true }, false,
			"git state lives in the running sandbox sb"},
		{"gateway_error_is_unknown", "deepseek-flash-sandbox",
			func(string, string) (LiveGitState, bool) {
				return SandboxSeatGitState("", false, errors.New("connection refused"), "sb"), true
			}, false, "connection refused"},
		{"host_seat_is_measured", "deepseek-flash",
			func(string, string) (LiveGitState, bool) { return LiveGitState{}, false }, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, added, w := canonicalWithPolecats(t, true, "toast")
			p := added["toast"]
			// The host worktree holds a commit that is on no remote.
			w.Commit(t, p.ClonePath, p.Branch, "only the sandbox pushed this", map[string]string{"stale.txt": "x\n"})
			w.checkout(t, p.ClonePath)
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.Agent = tt.agent; f.Branch = p.Branch })
			var asked []string
			mgr.sandboxSeat = func(agent, session string) (LiveGitState, bool) {
				asked = append(asked, agent+"|"+session)
				return tt.probe(agent, session)
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

			if len(asked) != 1 || asked[0] != tt.agent+"|"+mgr.sessionName("toast") {
				t.Fatalf("sandbox probe asked %v, want one question about %s", asked, tt.agent)
			}
			blockers := strings.Join(d.Blockers, "\n")
			hostBlock := strings.Contains(blockers, "unpushed") || strings.Contains(blockers, "mq_status")
			if hostBlock != tt.wantHostBlock {
				t.Errorf("host clone blocker = %v, want %v (reason=%s blockers=%v)", hostBlock, tt.wantHostBlock, d.Reason, d.Blockers)
			}
			if tt.wantReason == "" {
				if d.GitStateReason != "" {
					t.Errorf("git state reason = %q, want a known state", d.GitStateReason)
				}
				if !tt.wantHostBlock && (!d.Reusable || !d.SafeToNuke) {
					t.Errorf("a gone sandbox seat with nothing at risk is reusable=%v safe=%v (reason=%s blockers=%v)", d.Reusable, d.SafeToNuke, d.Reason, d.Blockers)
				}
				return
			}
			if d.GitStateSource != GitStateSourceUnknown || !strings.Contains(d.GitStateReason, tt.wantReason) {
				t.Errorf("git state = %q %q, want unknown with a reason containing %q", d.GitStateSource, d.GitStateReason, tt.wantReason)
			}
			if d.Reusable || d.SafeToNuke {
				t.Errorf("a seat whose git state is unknown is reusable=%v safe=%v", d.Reusable, d.SafeToNuke)
			}
		})
	}
}

const sandboxSeatSettings = `{"type":"town-settings","version":1,
"agents":{"deepseek-flash":{"command":"claude"},"deepseek-flash-sandbox":{"command":"claude"}},
"sandboxes":{"polecat-default":{"gateway":"gw1","image":"gastown-polecat:dev","policy":"deploy/polecat/policy.yaml"}},
"polecat_pool":{"seats":{
 "deepseek-flash":{"cap":1},
 "deepseek-flash-sandbox":{"cap":1,"backend":"openshell","sandbox":"polecat-default"}}}}`

// writeTownSettings puts content where the probe reads the town's settings.
func writeTownSettings(t *testing.T, content string) string {
	t.Helper()
	town := t.TempDir()
	path := config.TownSettingsPath(town)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// NewSandboxSeatProbe classifies a seat from the town's seat map and asks the
// seat's gateway; a host seat, an unrecorded agent and an agent with no seat are
// not sandbox seats and never reach the gateway.
func TestNewSandboxSeatProbe(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name   string
		agent  string
		wantOK bool
	}{
		{"sandbox_seat", "deepseek-flash-sandbox", true},
		{"host_seat", "deepseek-flash", false},
		{"unrecorded_agent", "", false},
		{"agent_with_no_seat", "claude-sonnet", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := writeTownSettings(t, sandboxSeatSettings)
			var asked []string
			probe := NewSandboxSeatProbe(town, func(gateway, name string) (bool, error) {
				asked = append(asked, gateway+"|"+name)
				return false, nil
			})

			got, ok := probe(tt.agent, "gt-toast")

			if ok != tt.wantOK {
				t.Fatalf("sandbox seat = %v, want %v", ok, tt.wantOK)
			}
			if tt.wantOK && (len(asked) != 1 || asked[0] != "gw1|gt-toast" || got.Source != GitStateSourceLive) {
				t.Errorf("asked %v, state %+v: want one question about gt-toast on gw1 and a live clean state", asked, got)
			}
			if !tt.wantOK && len(asked) != 0 {
				t.Errorf("gateway asked %v about a seat that is no sandbox seat", asked)
			}
		})
	}
}

// A town whose settings or seat map cannot be read cannot say which seats are
// sandbox seats, so an agent that is recorded answers unknown instead of being
// taken for a host seat and measured on its stale worktree; an unrecorded agent
// is still no sandbox seat, and the gateway is never asked.
func TestNewSandboxSeatProbeFailsClosedOnUnreadableSettings(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name     string
		settings string
	}{
		{"malformed_json", `{not json`},
		{"bad_pool_timezone", `{"type":"town-settings","version":1,"polecat_pool":{"timezone":"Not/AZone","seats":{"deepseek-flash":{"cap":1}}}}`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			town := writeTownSettings(t, tt.settings)
			probe := NewSandboxSeatProbe(town, func(string, string) (bool, error) {
				t.Error("gateway asked although no seat could be classified")
				return false, nil
			})

			got, ok := probe("deepseek-flash-sandbox", "gt-toast")

			if !ok || got.Source != GitStateSourceUnknown || !strings.Contains(got.FailedReason, "deepseek-flash-sandbox") {
				t.Errorf("recorded agent = %+v ok=%v, want an unknown sandbox-seat answer naming the agent", got, ok)
			}
			if _, ok := probe("", "gt-toast"); ok {
				t.Error("an unrecorded agent was taken for a sandbox seat")
			}
		})
	}
}

// One probe reads the town's settings once, however many seats it is asked
// about, so the reuse gate does not reload them for every polecat it weighs.
func TestNewSandboxSeatProbeReadsSettingsOnce(t *testing.T) {
	t.Parallel()
	town := writeTownSettings(t, sandboxSeatSettings)
	probe := NewSandboxSeatProbe(town, func(string, string) (bool, error) { return false, nil })
	if _, ok := probe("deepseek-flash-sandbox", "gt-a"); !ok {
		t.Fatal("sandbox seat not classified")
	}
	if err := os.Remove(config.TownSettingsPath(town)); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe("deepseek-flash-sandbox", "gt-b"); !ok {
		t.Error("the second question reread the settings instead of the first read")
	}
}
