package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/session"
)

const (
	staleHookRig     = "gastown"
	staleHookPolecat = "capable"
	staleHookSession = "gt-capable"
	staleHookWork    = "gt-nad3h"
)

const staleHookSettings = `{"type":"town-settings","version":1,
"agents":{"deepseek-flash":{"command":"claude"},"deepseek-flash-sandbox":{"command":"claude"}},
"sandboxes":{"polecat-default":{"gateway":"gw1","image":"gastown-polecat:dev","policy":"deploy/polecat/policy.yaml"}},
"polecat_pool":{"seats":{
 "deepseek-flash":{"cap":1},
 "deepseek-flash-sandbox":{"cap":1,"backend":"openshell","sandbox":"polecat-default"}}}}`

// staleHook is one reap scenario: the town, the work bead, the agent bead, the
// gateway's answer, and what the reap did.
type staleHook struct {
	t         *testing.T
	town      string
	host      *patrolScanHost
	agentBead string
	agentDB   *beadsfake.Fake
	gateway   []string // the "gateway|sandbox" questions asked
}

type staleHookOpts struct {
	agent        string // the agent the agent bead recorded
	hook         string // the agent bead's hook_bead
	workStatus   string // the work bead's status, "" for no work bead
	workAssignee string
	record       bool  // a sandbox registry record exists
	exists       bool  // the gateway has the sandbox
	existsErr    error // the gateway cannot be asked
}

func newStaleHook(t *testing.T, o staleHookOpts) *staleHook {
	t.Helper()
	town := t.TempDir()
	writeTownFile(t, town, "settings/config.json", staleHookSettings)
	s := &staleHook{t: t, town: town, agentDB: beadsfake.New()}
	s.agentBead = beads.AgentBeadIDFor(constants.RolePolecat, staleHookRig, staleHookPolecat, town)
	if _, err := s.agentDB.Create(beads.CreateOptions{
		ID: s.agentBead, Title: s.agentBead, Labels: []string{"gt:agent"}, Priority: -1,
		Description: beads.FormatAgentDescription(s.agentBead, &beads.AgentFields{
			RoleType: "polecat", Rig: staleHookRig, AgentState: "spawning", HookBead: o.hook, Agent: o.agent,
		}),
	}); err != nil {
		t.Fatal(err)
	}
	bd := newWorkBD(t)
	if o.workStatus != "" {
		if _, err := bd.db.Create(beads.CreateOptions{ID: staleHookWork, Title: "work", Priority: 2, Assignee: o.workAssignee}); err != nil {
			t.Fatal(err)
		}
		status := o.workStatus
		if err := bd.db.Update(staleHookWork, beads.UpdateOptions{Status: &status}); err != nil {
			t.Fatal(err)
		}
	}
	if o.record {
		if err := session.WriteSandboxRecord(town, session.SandboxRecord{Session: staleHookSession, Rig: staleHookRig, Polecat: staleHookPolecat}); err != nil {
			t.Fatal(err)
		}
	}
	d := &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0), openWorkBeads: bd.open}
	s.host = &patrolScanHost{
		d:                 d,
		openRecoveryBeads: func([]string) beads.Client { return s.agentDB },
		sandboxExists: func(gateway, name string) (bool, error) {
			s.gateway = append(s.gateway, gateway+"|"+name)
			return o.exists, o.existsErr
		},
	}
	return s
}

func (s *staleHook) reap() bool {
	s.t.Helper()
	return s.host.reapStaleSandboxHook(staleHookRig, staleHookPolecat, staleHookSession, staleHookWork)
}

func (s *staleHook) hook() string {
	s.t.Helper()
	issue, err := s.agentDB.Show(s.agentBead)
	if err != nil {
		s.t.Fatal(err)
	}
	return beads.ParseAgentFields(issue.Description).HookBead
}

// A sandbox seat whose sandbox and record are gone and whose work was handed
// back (open, unassigned) has its agent-bead hook cleared. Every other state
// leaves the hook for patrol: a host seat (its restart needs the hook), a live
// sandbox or record, a gateway that cannot be asked, work still assigned or
// finished, and a seat that took new work.
func TestReapStaleSandboxHook(t *testing.T) {
	t.Parallel()
	sandboxAgent := "deepseek-flash-sandbox"
	tests := []struct {
		name      string
		opts      staleHookOpts
		wantReap  bool
		wantAsked bool // the gateway is asked about the sandbox
	}{
		{"sandbox_gone_work_handed_back", staleHookOpts{agent: sandboxAgent, hook: staleHookWork, workStatus: "open"}, true, true},
		{"host_seat_is_left", staleHookOpts{agent: "deepseek-flash", hook: staleHookWork, workStatus: "open"}, false, false},
		{"agent_not_recorded_is_left", staleHookOpts{agent: "", hook: staleHookWork, workStatus: "open"}, false, false},
		{"registry_record_present_is_left", staleHookOpts{agent: sandboxAgent, hook: staleHookWork, workStatus: "open", record: true}, false, false},
		{"gateway_has_the_sandbox_is_left", staleHookOpts{agent: sandboxAgent, hook: staleHookWork, workStatus: "open", exists: true}, false, true},
		{"gateway_unreachable_is_left", staleHookOpts{agent: sandboxAgent, hook: staleHookWork, workStatus: "open", existsErr: errors.New("connection refused")}, false, true},
		{"work_still_assigned_is_left", staleHookOpts{agent: sandboxAgent, hook: staleHookWork, workStatus: "in_progress", workAssignee: "gastown/polecats/capable"}, false, false},
		{"work_bead_unreadable_is_left", staleHookOpts{agent: sandboxAgent, hook: staleHookWork, workStatus: ""}, false, false},
		{"seat_took_new_work_is_left", staleHookOpts{agent: sandboxAgent, hook: "gt-newer", workStatus: "open"}, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := newStaleHook(t, tt.opts)
			if got := s.reap(); got != tt.wantReap {
				t.Fatalf("reapStaleSandboxHook = %v, want %v", got, tt.wantReap)
			}
			if tt.wantReap && s.hook() != "" {
				t.Errorf("agent bead hook = %q after the reap, want cleared", s.hook())
			}
			if !tt.wantReap && s.hook() != tt.opts.hook {
				t.Errorf("agent bead hook = %q, want %q untouched", s.hook(), tt.opts.hook)
			}
			if asked := len(s.gateway) > 0; asked != tt.wantAsked {
				t.Errorf("gateway asked = %v (%v), want %v", asked, s.gateway, tt.wantAsked)
			}
			if tt.wantAsked && len(s.gateway) == 1 && s.gateway[0] != "gw1|"+staleHookSession {
				t.Errorf("gateway question = %q, want the seat profile's gateway and the session's sandbox name", s.gateway[0])
			}
		})
	}
}

// A town with no readable settings reaps nothing: which seat is a sandbox seat
// cannot be told.
func TestReapStaleSandboxHookWithoutSettingsReapsNothing(t *testing.T) {
	t.Parallel()
	s := newStaleHook(t, staleHookOpts{agent: "deepseek-flash-sandbox", hook: staleHookWork, workStatus: "open"})
	if err := os.WriteFile(filepath.Join(s.town, "settings", "config.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if s.reap() {
		t.Fatal("reaped with unreadable settings")
	}
	if s.hook() != staleHookWork {
		t.Errorf("hook = %q, want untouched", s.hook())
	}
}
