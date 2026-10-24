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

// A gone sandbox reads clean on git alone, so the work the seat still holds is
// caught by the checks that never needed the host clone: a bead still hooked on
// the seat blocks reuse and nuke whatever the gateway says, and only a hook
// that names finished work lets the seat go (gt-qq6rg.1; whether a closed
// bead's work reached origin is the recovery verdict's question).
func TestGoneSandboxSeatIsHeldByItsHookBead(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name       string
		status     string
		wantReuse  bool
		wantBlocks string
	}{
		{"hooked_bead_blocks", string(beads.IssueStatusHooked), false, "gt-work"},
		{"closed_bead_releases", string(beads.StatusClosed), true, ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, added, _ := canonicalWithPolecats(t, true, "toast")
			p := added["toast"]
			if _, err := bd.Fake.Create(beads.CreateOptions{ID: "gt-work", Title: "work", Priority: 2}); err != nil {
				t.Fatal(err)
			}
			if err := bd.Fake.Update("gt-work", beads.UpdateOptions{Status: &tt.status}); err != nil {
				t.Fatal(err)
			}
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
				f.Agent = "deepseek-flash-sandbox"
				f.Branch = p.Branch
				f.HookBead = "gt-work"
			})
			mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
				return SandboxSeatGitState("", false, nil, "sb"), true
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

			if d.Reusable != tt.wantReuse || d.SafeToNuke != tt.wantReuse {
				t.Errorf("reusable=%v safe=%v, want %v (reason=%s blockers=%v)", d.Reusable, d.SafeToNuke, tt.wantReuse, d.Reason, d.Blockers)
			}
			if !strings.Contains(strings.Join(d.Blockers, "\n"), tt.wantBlocks) {
				t.Errorf("blockers = %v, want one naming %q", d.Blockers, tt.wantBlocks)
			}
		})
	}
}

// With no probe injected the Manager builds the production probe from its town
// root: settings that cannot be read leave a recorded agent unknown, never a
// host seat, so the default path is closed as well as the injected one.
func TestManagerDefaultSandboxSeatProbeFailsClosed(t *testing.T) {
	t.Parallel()
	mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")
	mgr.townRoot = writeTownSettings(t, `{not json`)
	bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) { f.Agent = "deepseek-flash-sandbox" })

	d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

	if d.GitStateSource != GitStateSourceUnknown || !strings.Contains(d.GitStateReason, "deepseek-flash-sandbox") {
		t.Errorf("git state = %q %q, want unknown naming the agent", d.GitStateSource, d.GitStateReason)
	}
	if d.Reusable || d.SafeToNuke {
		t.Errorf("reusable=%v safe=%v with seat settings unreadable", d.Reusable, d.SafeToNuke)
	}
}

// The reuse gate tells lost work from a finished seat the way check-recovery and
// the nuke gate do (gt-qq6rg.4): with the sandbox gone, a bead the seat left
// unfinished with no branch on origin and no resume_branch is lost work
// (NEEDS_RECOVERY, sandbox-work-lost, not reusable); a branch on origin, a
// resume_branch, a finished bead or no bead is not. (A bead or an origin that
// cannot be read leaves the git state unknown; the cmd tests pin that.)
func TestWorkstateDispositionOfAGoneSandboxSeatTellsLostWork(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		hook       string
		source     string
		workStatus string
		resume     bool
		onOrigin   bool
		wantLost   bool
		wantReuse  bool
	}{
		{name: "hooked_open_nothing_pushed", hook: "gt-work", workStatus: "open", wantLost: true},
		{name: "source_open_hook_cleared_nothing_pushed", source: "gt-work", workStatus: "open", wantLost: true},
		{name: "source_open_branch_on_origin", source: "gt-work", workStatus: "open", onOrigin: true, wantReuse: true},
		{name: "source_open_resume_branch", source: "gt-work", workStatus: "open", resume: true, wantReuse: true},
		{name: "bead_closed", source: "gt-work", workStatus: "closed", wantReuse: true},
		{name: "no_bead", wantReuse: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, mayorRig, bd, _, w := canonicalWithPolecats(t, true, "toast")
			// The branch the sandbox pushed: the host rig repo has no such ref unless
			// the case puts it there.
			const branch = "polecat/toast/gt-a+x"
			if tt.onOrigin {
				w.SetRef(t, mayorRig, "refs/heads/"+branch, w.rev(t, mayorRig, "main"))
			}
			if tt.workStatus != "" {
				if _, err := bd.Create(beads.CreateOptions{ID: "gt-work", Title: "work", Priority: 2}); err != nil {
					t.Fatal(err)
				}
				status := tt.workStatus
				upd := beads.UpdateOptions{Status: &status}
				if tt.resume {
					upd.AppendNotes = "resume_branch: " + branch
				}
				if err := bd.Update("gt-work", upd); err != nil {
					t.Fatal(err)
				}
			}
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
				f.Agent, f.Branch, f.HookBead, f.LastSourceIssue = "deepseek-flash-sandbox", branch, tt.hook, tt.source
			})
			mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
				return SandboxSeatGitState("", false, nil, "sb"), true
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

			if lost := d.Reason == "sandbox-work-lost"; lost != tt.wantLost {
				t.Errorf("lost work = %v, want %v (reason %s blockers %v)", lost, tt.wantLost, d.Reason, d.Blockers)
			}
			if tt.wantLost && (d.SafeToNuke || d.Reusable || !d.NeedsRecovery) {
				t.Errorf("lost work is reusable=%v safe=%v needs_recovery=%v", d.Reusable, d.SafeToNuke, d.NeedsRecovery)
			}
			if tt.wantReuse && !d.Reusable {
				t.Errorf("seat with nothing lost is not reusable (reason %s blockers %v)", d.Reason, d.Blockers)
			}
		})
	}
}

// showFailsBeads is a Manager beads handle whose Show fails for one bead with an
// error that is not "not found".
type showFailsBeads struct {
	polecatBeads
	id string
}

func (f showFailsBeads) Show(id string) (*beads.Issue, error) {
	if id == f.id {
		return nil, errors.New("dolt unreachable")
	}
	return f.polecatBeads.Show(id)
}

// With the sandbox gone, anything the lost-work check cannot establish leaves the
// git state unknown instead of claiming the work lost or clean: a work bead that
// cannot be read, an origin that cannot be asked, an agent bead that records no
// branch. A work bead that no longer exists leaves nothing at risk (gt-qq6rg.4).
func TestSandboxLostWorkFactsFailClosedWhenTheAnswerIsUnknown(t *testing.T) {
	t.Parallel()
	const branch = "polecat/toast/gt-a+x"
	tests := []struct {
		name      string
		branch    string
		workBead  bool // the work bead exists (open, no resume_branch)
		showFails bool
		noRepo    bool // the rig has no repo, so origin cannot be asked
		wantUnk   string
		wantGone  bool // SandboxGone is set (the check reached a verdict)
	}{
		{name: "work_bead_cannot_be_read", branch: branch, workBead: true, showFails: true, wantUnk: "could not read gt-work for a resume_branch"},
		{name: "origin_cannot_be_asked", branch: branch, workBead: true, noRepo: true, wantUnk: "could not ask origin whether " + branch},
		{name: "no_branch_recorded", branch: "", workBead: true, wantUnk: "records no branch"},
		{name: "work_bead_no_longer_exists", branch: branch},
		{name: "branch_absent_from_origin_is_lost", branch: branch, workBead: true, wantGone: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, _, _ := canonicalWithPolecats(t, true, "toast")
			if tt.workBead {
				if _, err := bd.Create(beads.CreateOptions{ID: "gt-work", Title: "work", Priority: 2}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.showFails {
				mgr.beads = showFailsBeads{mgr.beads, "gt-work"}
			}
			if tt.noRepo {
				mgr.rig.Path = t.TempDir()
			}
			fields := &beads.AgentFields{Agent: "deepseek-flash-sandbox", Branch: tt.branch, LastSourceIssue: "gt-work"}
			facts := WorkstateFacts{State: StateIdle, CleanupStatus: CleanupClean, HookBeadSafe: true, GitStateSource: GitStateSourceLive, AgentBeadRead: true}

			mgr.applySandboxLostWorkFacts(&facts, fields, "gt-work")

			if (tt.wantUnk != "") != facts.GitCheckFailed || facts.SandboxGone != tt.wantGone {
				t.Fatalf("facts = {GitCheckFailed:%v SandboxGone:%v}, want {%v %v}", facts.GitCheckFailed, facts.SandboxGone, tt.wantUnk != "", tt.wantGone)
			}
			if tt.wantUnk != "" {
				if !strings.Contains(facts.GitCheckFailedReason, tt.wantUnk) || facts.GitStateSource != GitStateSourceUnknown {
					t.Errorf("unknown = (%q, %q), want a reason naming %q", facts.GitCheckFailedReason, facts.GitStateSource, tt.wantUnk)
				}
				d := DecideWorkstate(NewWorkstateInput(facts))
				if d.Reusable || d.SafeToNuke || d.Reason == "sandbox-work-lost" {
					t.Errorf("an unknown answer decided %+v: want neither reusable, safe to nuke, nor lost work", d)
				}
			}
		})
	}
}
