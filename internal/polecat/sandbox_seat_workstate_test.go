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

// A sandbox seat's git answer comes from the gateway. While the sandbox runs
// (or when the gateway cannot be asked) the host clone is not measured and the
// answer is unknown, never clean, because a commit only the sandbox pushed
// would read as unpushed work (gt-qq6rg.1). Once the gateway affirms the
// sandbox gone, the seat is judged from what survives in the host worktree —
// HEAD's commits against the pushed branch on origin — so work already pushed
// reads clean and host-only work blocks (gt-zgoo3). A host seat is measured
// either way.
func TestWorkstateDispositionForASandboxSeat(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name          string
		agent         string
		probe         func(agent, session string) (LiveGitState, bool)
		pushBranch    bool   // the sandbox pushed the host commit: nothing is unique to the host
		wantHostBlock bool   // the host clone's commit (on no remote, with no MR) blocks the seat
		wantReason    string // non-empty: the git state is unknown and the reason contains this
	}{
		{"sandbox_gone_with_pushed_host_work_is_clean", "deepseek-flash-sandbox",
			func(string, string) (LiveGitState, bool) { return SandboxSeatGitState("", false, nil, "sb"), true },
			true, false, ""},
		{"sandbox_running_is_unknown", "deepseek-flash-sandbox",
			func(string, string) (LiveGitState, bool) { return SandboxSeatGitState("", true, nil, "sb"), true },
			false, false, "git state lives in the running sandbox sb"},
		{"gateway_error_is_unknown", "deepseek-flash-sandbox",
			func(string, string) (LiveGitState, bool) {
				return SandboxSeatGitState("", false, errors.New("connection refused"), "sb"), true
			}, false, false, "connection refused"},
		{"host_seat_is_measured", "deepseek-flash",
			func(string, string) (LiveGitState, bool) { return LiveGitState{}, false }, false, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, added, w := canonicalWithPolecats(t, true, "toast")
			p := added["toast"]
			// The host worktree holds a commit that is on no remote, unless the
			// case pushes it as the sandbox's own work.
			w.Commit(t, p.ClonePath, p.Branch, "only the sandbox pushed this", map[string]string{"stale.txt": "x\n"})
			if tt.pushBranch {
				if err := w.repo(p.ClonePath).Push("origin", p.Branch, false); err != nil {
					t.Fatalf("push %s: %v", p.Branch, err)
				}
			}
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

// A seat name the rule cannot establish is a refusal, not a match against any
// seat's branch: the rule never clears a seat on a branch it cannot tell is its
// own, and never asks origin about one.
func TestSandboxSeatLandedWithoutBranchNeedsASeatName(t *testing.T) {
	t.Parallel()
	issue := &beads.Issue{ID: "gt-work", Status: string(beads.StatusClosed), Notes: "LANDING RECORD\nlanded_commit: 0123456789abcdef\n"}
	asked := false
	reason := SandboxSeatLandedWithoutBranch("  ", issue, "gt-work", func(string) ([]string, error) {
		asked = true
		return []string{"polecat/toast/gt-work+zzz"}, nil
	})

	if asked {
		t.Error("origin was asked although no seat name could match a branch")
	}
	if !strings.Contains(reason, "no seat name to match a branch for gt-work") {
		t.Errorf("reason = %q, want a refusal naming the issue", reason)
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

// missingBeads is a Manager beads handle that reports one bead as absent with
// no error, the nil answer a gone bead is distinct from an unreadable one.
type missingBeads struct {
	polecatBeads
	id string
}

func (m missingBeads) Show(id string) (*beads.Issue, error) {
	if id == m.id {
		return nil, nil
	}
	return m.polecatBeads.Show(id)
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

			mgr.applySandboxLostWorkFacts(&facts, fields, "gt-work", "toast")

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

// A sandbox seat whose agent bead recorded no branch names nothing to ask
// origin about, so the reuse gate judges it by its issue instead: an issue
// closed with a landing record and no branch named for the seat and issue left
// on origin is finished, with nothing at risk, and the seat is reusable; an
// open issue, a no-changes close (closed with no landing record), a branch for
// the seat and issue still on origin, an unreadable issue and an origin that
// cannot be asked each keep today's unknown git state (gt-xhnn3). A branch for
// the issue naming another seat is not this seat's and does not refuse it.
//
// Mutation: dropping the landing-record check from SandboxSeatLandedWithoutBranch
// makes the no_changes_close row below red, and the same row red in the three
// cmd tests that drive the rule through check-recovery and the nuke gate.
func TestWorkstateDispositionOfASandboxSeatWithNoBranchTellsItsLanding(t *testing.T) {
	t.Parallel()
	const landingRecord = "LANDING RECORD\nlanded_commit: 0123456789abcdef\npatch_id: p1\n"
	tests := []struct {
		name      string
		status    string // the work bead's status
		notes     string // appended to the work bead
		seatOnRef string // seat a generated polecat branch for the issue on origin names
		showFails bool   // the work bead cannot be read
		missing   bool   // the work bead no longer exists, reported as nil, nil
		noRepo    bool   // the rig has no repo, so origin cannot be asked
		wantUnk   string // substring of the unknown reason; "" wants the safe case
	}{
		{name: "landed_no_branch_on_origin", status: string(beads.StatusClosed), notes: landingRecord},
		{name: "no_changes_close_is_not_a_landing", status: string(beads.StatusClosed), wantUnk: "closed with no landing record"},
		{name: "open_issue_that_landed", status: string(beads.StatusOpen), notes: landingRecord, wantUnk: "is open"},
		{name: "branch_for_the_seat_and_issue_on_origin", status: string(beads.StatusClosed), notes: landingRecord, seatOnRef: "toast", wantUnk: "still exists on origin"},
		{name: "another_seats_branch_for_the_issue_on_origin", status: string(beads.StatusClosed), notes: landingRecord, seatOnRef: "furiosa"},
		{name: "issue_no_longer_exists", status: string(beads.StatusClosed), missing: true},
		{name: "origin_cannot_be_asked", status: string(beads.StatusClosed), notes: landingRecord, noRepo: true, wantUnk: "could not ask origin whether a branch for gt-work survived"},
		{name: "issue_cannot_be_read", status: string(beads.StatusOpen), showFails: true, wantUnk: "could not read gt-work for a resume_branch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, mayorRig, bd, _, w := canonicalWithPolecats(t, true, "toast")
			if tt.seatOnRef != "" {
				// The rig's origin is mayor/rig itself (canonicalRig points origin
				// at it), so a refs/heads ref written there is a branch on origin.
				w.SetRef(t, mayorRig, "refs/heads/polecat/"+tt.seatOnRef+"/gt-work+zzz", w.rev(t, mayorRig, "main"))
			}
			if _, err := bd.Create(beads.CreateOptions{ID: "gt-work", Title: "work", Priority: 2}); err != nil {
				t.Fatal(err)
			}
			switch {
			case tt.showFails:
				mgr.beads = showFailsBeads{mgr.beads, "gt-work"}
			case tt.missing:
				mgr.beads = missingBeads{mgr.beads, "gt-work"}
			default:
				status := tt.status
				if err := bd.Update("gt-work", beads.UpdateOptions{Status: &status, AppendNotes: tt.notes}); err != nil {
					t.Fatal(err)
				}
			}
			if tt.noRepo {
				mgr.rig.Path = t.TempDir()
			}
			// The agent bead records no branch: the seat left only its issue.
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
				f.Agent, f.Branch, f.HookBead, f.LastSourceIssue = "deepseek-flash-sandbox", "", "", "gt-work"
			})
			mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
				return SandboxSeatGitState("", false, nil, "sb"), true
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

			if tt.wantUnk != "" {
				if d.GitStateSource != GitStateSourceUnknown || !strings.Contains(d.GitStateReason, tt.wantUnk) {
					t.Errorf("git state = %q %q, want unknown naming %q", d.GitStateSource, d.GitStateReason, tt.wantUnk)
				}
				if d.Reusable || d.SafeToNuke || d.Reason == "sandbox-work-lost" {
					t.Errorf("an unknown answer decided %+v: want neither reusable, safe to nuke, nor lost work", d)
				}
				return
			}
			if d.GitStateSource != GitStateSourceLive {
				t.Errorf("git state = %q %q, want live", d.GitStateSource, d.GitStateReason)
			}
			if !d.Reusable || !d.SafeToNuke {
				t.Errorf("landed seat reusable=%v safe=%v (reason %s blockers %v), want a safe, reusable seat", d.Reusable, d.SafeToNuke, d.Reason, d.Blockers)
			}
		})
	}
}

// A gone sandbox seat is judged from what survives on the host: the host
// worktree's commits are compared with the pushed branch on origin by patch
// identity, so work the sandbox already pushed is not reported as a git check
// failure, while a commit only the host holds still blocks and names its count
// (gt-zgoo3).
func TestSandboxGoneWithEveryCommitOnOriginIsNotGitCheckFailed(t *testing.T) {
	t.Parallel()
	mgr, _, bd, added, w := canonicalWithPolecats(t, true, "toast")
	p := added["toast"]
	// The sandbox pushed this commit; the host worktree holds the same patch
	// under a rewritten SHA, so only patch identity (git cherry) sees that
	// nothing is unique to the host.
	base := w.rev(t, p.ClonePath, "main")
	w.Commit(t, p.ClonePath, p.Branch, "the sandbox's fix", map[string]string{"fix.txt": "x\n"})
	if err := w.repo(p.ClonePath).Push("origin", p.Branch, false); err != nil {
		t.Fatalf("push %s: %v", p.Branch, err)
	}
	w.SetRef(t, p.ClonePath, "refs/heads/"+p.Branch, base)
	w.Commit(t, p.ClonePath, p.Branch, "the host's copy of the fix", map[string]string{"fix.txt": "x\n"})
	w.checkout(t, p.ClonePath)
	bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
		f.Agent, f.Branch = "deepseek-flash-sandbox", p.Branch
	})
	mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
		return SandboxSeatGitState("", false, nil, "sb"), true
	}

	d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

	blockers := strings.Join(d.Blockers, "\n")
	if strings.Contains(blockers, "git-check-failed") || d.GitStateReason != "" {
		t.Fatalf("a gone sandbox whose host commits are all on origin is git-check-failed: reason=%q blockers=%v", d.GitStateReason, d.Blockers)
	}
	if !d.Reusable || !d.SafeToNuke {
		t.Errorf("seat with nothing at risk reusable=%v safe=%v (reason=%s blockers=%v)", d.Reusable, d.SafeToNuke, d.Reason, d.Blockers)
	}
}

// The same gone sandbox seat, with one host commit that has no equivalent on
// the pushed branch, is a blocker that names the unique commit count.
func TestSandboxGoneWithAUniqueCommitStillBlocks(t *testing.T) {
	t.Parallel()
	mgr, _, bd, added, w := canonicalWithPolecats(t, true, "toast")
	p := added["toast"]
	base := w.rev(t, p.ClonePath, "main")
	// The sandbox pushed one commit; the host worktree holds a different one.
	w.Commit(t, p.ClonePath, p.Branch, "the sandbox's fix", map[string]string{"pushed.txt": "x\n"})
	if err := w.repo(p.ClonePath).Push("origin", p.Branch, false); err != nil {
		t.Fatalf("push %s: %v", p.Branch, err)
	}
	w.SetRef(t, p.ClonePath, "refs/heads/"+p.Branch, base)
	w.Commit(t, p.ClonePath, p.Branch, "only the host has this", map[string]string{"host.txt": "y\n"})
	w.checkout(t, p.ClonePath)
	bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
		f.Agent, f.Branch = "deepseek-flash-sandbox", p.Branch
	})
	mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
		return SandboxSeatGitState("", false, nil, "sb"), true
	}

	d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

	blockers := strings.Join(d.Blockers, "\n")
	if !strings.Contains(blockers, "unpushed_commits=1") {
		t.Fatalf("blockers = %v, want one naming the unique commit count (reason=%s)", d.Blockers, d.Reason)
	}
	if d.Reusable || d.SafeToNuke || !d.NeedsRecovery {
		t.Errorf("seat with a host-only commit reusable=%v safe=%v needs_recovery=%v", d.Reusable, d.SafeToNuke, d.NeedsRecovery)
	}
}

// A gone sandbox seat whose host worktree directory is missing is judged from
// the branch ref that worktree left in the rig's shared repository: a commit
// only that ref holds blocks and names its count, work already pushed and a
// ref the repo never held read safe, and a comparison that cannot run blocks
// (gt-zgoo3).
func TestGoneSandboxMissingWorktreeWithUnpushedLocalBranchBlocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		mutate      func(t *testing.T, w *world, mayorRig string, p *Polecat)
		wantBlocker string // "" means the seat must be safe
	}{
		{
			name:        "an unpushed local branch blocks and counts its commits",
			mutate:      func(*testing.T, *world, string, *Polecat) {},
			wantBlocker: "unpushed_commits=1",
		},
		{
			name: "a local branch already pushed reads safe",
			mutate: func(t *testing.T, w *world, _ string, p *Polecat) {
				if err := w.repo(p.ClonePath).Push("origin", p.Branch, false); err != nil {
					t.Fatalf("push %s: %v", p.Branch, err)
				}
			},
		},
		{
			name: "a local branch the shared repo never held reads safe",
			mutate: func(t *testing.T, w *world, mayorRig string, p *Polecat) {
				w.DeleteRef(t, mayorRig, "refs/heads/"+p.Branch)
			},
		},
		{
			name: "an unreadable comparison blocks",
			mutate: func(t *testing.T, w *world, mayorRig string, p *Polecat) {
				w.DeleteRef(t, mayorRig, "refs/remotes/origin/"+p.Branch)
				w.DeleteRef(t, mayorRig, "refs/remotes/origin/main")
			},
			wantBlocker: "comparing refs/heads/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, mayorRig, bd, added, w := canonicalWithPolecats(t, true, "toast")
			p := added["toast"]
			w.Commit(t, p.ClonePath, p.Branch, "only the host has this", map[string]string{"host.txt": "y\n"})
			w.checkout(t, p.ClonePath)
			tt.mutate(t, w, mayorRig, p)
			// The sandbox's host worktree directory is gone; only the shared
			// repository's branch ref survives.
			if err := os.RemoveAll(p.ClonePath); err != nil {
				t.Fatal(err)
			}
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
				f.Agent, f.Branch = "deepseek-flash-sandbox", p.Branch
			})
			mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
				return SandboxSeatGitState("", false, nil, "sb"), true
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

			blockers := strings.Join(d.Blockers, "\n")
			if tt.wantBlocker == "" {
				if d.NeedsRecovery || !d.Reusable || !d.SafeToNuke {
					t.Fatalf("seat with nothing at risk reusable=%v safe=%v needs_recovery=%v (reason=%s blockers=%v)",
						d.Reusable, d.SafeToNuke, d.NeedsRecovery, d.Reason, d.Blockers)
				}
				return
			}
			if !strings.Contains(blockers, tt.wantBlocker) {
				t.Fatalf("blockers = %v, want one containing %q (reason=%s)", d.Blockers, tt.wantBlocker, d.Reason)
			}
			if d.Reusable || d.SafeToNuke || !d.NeedsRecovery {
				t.Errorf("seat with %s reusable=%v safe=%v needs_recovery=%v", tt.wantBlocker, d.Reusable, d.SafeToNuke, d.NeedsRecovery)
			}
		})
	}
}

// A gone sandbox seat whose host worktree still exists keeps its dirt and
// stashes as blockers, not only its unique commits (gt-zgoo3).
func TestGoneSandboxWithHostWorktreeDirtOrStashBlocks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(t *testing.T, w *world, dir string)
		want   string
	}{
		{
			name: "dirt",
			mutate: func(t *testing.T, w *world, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "churn.txt"), []byte("churn\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "has_uncommitted",
		},
		{
			name: "a stash",
			mutate: func(t *testing.T, w *world, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("changed\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				w.Stash(t, dir, "wip")
			},
			want: "stash_count=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			mgr, _, bd, added, w := canonicalWithPolecats(t, true, "toast")
			p := added["toast"]
			tt.mutate(t, w, p.ClonePath)
			bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
				f.Agent, f.Branch = "deepseek-flash-sandbox", p.Branch
			})
			mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
				return SandboxSeatGitState("", false, nil, "sb"), true
			}

			d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

			if blockers := strings.Join(d.Blockers, "\n"); !strings.Contains(blockers, tt.want) {
				t.Fatalf("blockers = %v, want one containing %q (reason=%s)", d.Blockers, tt.want, d.Reason)
			}
			if d.Reusable || d.SafeToNuke || !d.NeedsRecovery {
				t.Errorf("seat with %s reusable=%v safe=%v needs_recovery=%v", tt.want, d.Reusable, d.SafeToNuke, d.NeedsRecovery)
			}
		})
	}
}

// A seat whose sandbox still exists keeps today's answer byte for byte: the
// host worktree is not consulted, and the running sandbox is the git state.
func TestRunningSandboxSeatKeepsTodaysGitCheckFailed(t *testing.T) {
	t.Parallel()
	mgr, _, bd, added, w := canonicalWithPolecats(t, true, "toast")
	p := added["toast"]
	// A stale host commit proves the running seat is not judged from it.
	w.Commit(t, p.ClonePath, p.Branch, "only the sandbox pushed this", map[string]string{"stale.txt": "x\n"})
	w.checkout(t, p.ClonePath)
	bd.setAgent(t, mgr.agentBeadID("toast"), func(f *beads.AgentFields) {
		f.Agent, f.Branch = "deepseek-flash-sandbox", p.Branch
	})
	mgr.sandboxSeat = func(string, string) (LiveGitState, bool) {
		return SandboxSeatGitState("", true, nil, "sb"), true
	}

	d := mgr.WorkstateDispositionForPolecat("toast", StateIdle, "")

	if d.GitStateSource != GitStateSourceUnknown || !strings.Contains(d.GitStateReason, "git state lives in the running sandbox sb") {
		t.Fatalf("git state = %q %q, want the running-sandbox unknown", d.GitStateSource, d.GitStateReason)
	}
	if d.Reusable || d.SafeToNuke || !d.NeedsRecovery {
		t.Errorf("running-sandbox seat reusable=%v safe=%v needs_recovery=%v", d.Reusable, d.SafeToNuke, d.NeedsRecovery)
	}
}
