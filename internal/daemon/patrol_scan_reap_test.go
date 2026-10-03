package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	agentconfig "github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/patrolscan"
	"github.com/steveyegge/gastown/internal/supervisor"
)

// reapGolden is the captured `gt polecat check-recovery-batch gastown --json`
// (2026-10-03, 12 seats). It is the shape the host must parse for real.
const reapGolden = "testdata/polecat_check_recovery_batch_gastown.json"

func readGolden(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(reapGolden)
	if err != nil {
		t.Fatalf("reading the captured batch: %v", err)
	}
	return string(data)
}

// reapHost builds the host under a config whose worktree_cleanup block is
// exactly wc, with bd reads answered by bd and gt runs by gt.
func reapHost(t *testing.T, wc *agentconfig.WorktreeCleanupConfig, bd *workBD, gt *fakeCLI) *patrolScanHost {
	t.Helper()
	d := &Daemon{
		logger: log.New(io.Discard, "", 0),
		gtPath: "/fake/gt",
		config: &Config{TownRoot: t.TempDir()},
		patrolConfig: &DaemonPatrolConfig{Patrols: &PatrolsConfig{
			PatrolScan: &PatrolScanConfig{Enabled: true, WorktreeCleanup: wc},
		}},
	}
	if bd != nil {
		d.openWorkBeads = bd.open
	}
	if gt != nil {
		d.execCmd = gt.run
	}
	return &patrolScanHost{d: d}
}

// enabledCleanup is an enabled block covering rig, dry-run.
func enabledCleanup(rig string) *agentconfig.WorktreeCleanupConfig {
	return &agentconfig.WorktreeCleanupConfig{Enabled: true, Rigs: []string{rig}}
}

// The host must read the real sweep field for field; the golden is a captured
// run, so a renamed JSON key fails here rather than at runtime.
func TestReapRecoveryParsesCapturedBatch(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(func([]string) cliReply { return cliReply{stdout: readGolden(t)} })
	h := reapHost(t, enabledCleanup("gastown"), nil, gt)

	got, err := h.Recovery("gastown", "agate")
	if err != nil {
		t.Fatalf("Recovery: %v", err)
	}
	want := patrolscan.Recovery{
		Verdict:        "SAFE_TO_NUKE",
		Reusable:       true,
		SafeToNuke:     true,
		Reason:         "reusable",
		Branch:         "polecat/agate/gt-7hgug+muszr4ir",
		Issue:          "gt-7hgug",
		GitStateSource: "live",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Recovery(agate) = %+v, want %+v", got, want)
	}
	working, err := h.Recovery("gastown", "basalt")
	if err != nil {
		t.Fatalf("Recovery(basalt): %v", err)
	}
	if working.Verdict != "WORKING" || working.Reusable || working.SafeToNuke {
		t.Fatalf("Recovery(basalt) = %+v, want a non-reusable WORKING seat", working)
	}
}

// blockers and active_mr are omitted from the captured sweep (no blocked seat
// was present), so their mapping is pinned on a hand-written status.
func TestReapRecoveryParsesBlockersAndActiveMR(t *testing.T) {
	t.Parallel()
	const body = `[{"rig":"gastown","polecat":"onyx","verdict":"NEEDS_RECOVERY",` +
		`"reusable":false,"safe_to_nuke":false,"reason":"dirty","active_mr":"gt-mr1",` +
		`"blockers":["uncommitted changes","stash present"],"git_state_source":"unknown"}]`
	gt := newFakeCLI(func([]string) cliReply { return cliReply{stdout: body} })
	h := reapHost(t, enabledCleanup("gastown"), nil, gt)

	got, err := h.Recovery("gastown", "onyx")
	if err != nil {
		t.Fatalf("Recovery: %v", err)
	}
	if got.ActiveMR != "gt-mr1" || len(got.Blockers) != 2 ||
		got.Blockers[0] != "uncommitted changes" || got.Blockers[1] != "stash present" {
		t.Fatalf("Recovery(onyx) = %+v, want the active MR and both blockers", got)
	}
	if got.GitStateSource != "unknown" {
		t.Fatalf("git_state_source = %q, want unknown", got.GitStateSource)
	}
}

// A batch reply that is empty, malformed, or a failed run must never become a
// zero Recovery: the pure pass would read that as a real verdict.
func TestReapRecoveryFailedBatchIsAnError(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		gt   *fakeCLI
	}{
		{"empty output", newFakeCLI(func([]string) cliReply { return cliReply{} })},
		{"bad JSON", newFakeCLI(func([]string) cliReply { return cliReply{stdout: "not json"} })},
		{"non-zero exit", newFakeCLI(func([]string) cliReply {
			return cliReply{stderr: "bd: connection refused", code: 1}
		})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := reapHost(t, enabledCleanup("gastown"), nil, tc.gt)
			got, err := h.Recovery("gastown", "agate")
			if err == nil {
				t.Fatalf("Recovery answered %+v, want an error", got)
			}
			if !reflect.DeepEqual(got, patrolscan.Recovery{}) {
				t.Fatalf("a failed read answered %+v, want the zero verdict", got)
			}
		})
	}
}

// A polecat the sweep did not mention is unknown: a rotation the batch missed
// must not read as a safe seat.
func TestReapRecoveryMissingPolecatIsAnError(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(func([]string) cliReply { return cliReply{stdout: readGolden(t)} })
	h := reapHost(t, enabledCleanup("gastown"), nil, gt)

	if _, err := h.Recovery("gastown", "nobody"); err == nil {
		t.Fatal("a polecat missing from the batch answered without error")
	}
}

// The batch is one exec per rig per tick even when the pass asks for a verdict
// on every seat: 12 seats, one sweep (gt-b839).
func TestReapRecoveryIsOneBatchExecPerTick(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(func([]string) cliReply { return cliReply{stdout: readGolden(t)} })
	h := reapHost(t, enabledCleanup("gastown"), nil, gt)

	names := []string{"agate", "basalt", "malachite", "marble", "mica", "pearl",
		"pyrite", "sapphire", "shale", "slate", "topaz", "turquoise"}
	for _, name := range names {
		if _, err := h.Recovery("gastown", name); err != nil {
			t.Fatalf("Recovery(%s): %v", name, err)
		}
	}
	calls := gt.recorded()
	if len(calls) != 1 {
		t.Fatalf("12 seats made %d execs, want 1: %+v", len(calls), calls)
	}
	want := []string{"polecat", "check-recovery-batch", "gastown", "--json"}
	if !slices.Equal(calls[0].args, want) {
		t.Fatalf("argv = %q, want %q", calls[0].args, want)
	}
	if slices.Contains(calls[0].args, "--reconcile-cleanup") {
		t.Fatal("the sweep must never pass --reconcile-cleanup")
	}
}

// Reap runs exactly `gt polecat nuke <rig>/<name>`: the verdict that
// authorized it is check-recovery's, so --force is never passed.
func TestReapArgvNeverForces(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(func([]string) cliReply { return cliReply{} })
	h := reapHost(t, enabledCleanup("gastown"), nil, gt)

	if err := h.Reap("gastown", "onyx"); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	calls := gt.recorded()
	if len(calls) != 1 {
		t.Fatalf("Reap made %d execs, want 1", len(calls))
	}
	want := []string{"polecat", "nuke", "gastown/onyx"}
	if !slices.Equal(calls[0].args, want) {
		t.Fatalf("argv = %q, want %q", calls[0].args, want)
	}
	if slices.Contains(calls[0].args, "--force") {
		t.Fatal("Reap passed --force")
	}
}

// Exit status 3 is a safety refusal; the tick must read it as blocked, not
// failed. Every other failure stays a plain error.
func TestReapRefusalExitIsSentinel(t *testing.T) {
	t.Parallel()
	refused := newFakeCLI(func([]string) cliReply {
		return cliReply{stderr: "blocked: 1 polecat(s) failed nuke safety checks", code: nukeRefusedExitCode}
	})
	h := reapHost(t, enabledCleanup("gastown"), nil, refused)
	err := h.Reap("gastown", "onyx")
	if !errors.Is(err, errReapRefused) {
		t.Fatalf("Reap on exit 3 = %v, want errReapRefused", err)
	}
	if !strings.Contains(err.Error(), "nuke safety checks") {
		t.Fatalf("refusal error %q drops the CLI's reason", err)
	}

	broken := newFakeCLI(func([]string) cliReply { return cliReply{stderr: "boom", code: 1} })
	h = reapHost(t, enabledCleanup("gastown"), nil, broken)
	if err := h.Reap("gastown", "onyx"); err == nil || errors.Is(err, errReapRefused) {
		t.Fatalf("Reap on exit 1 = %v, want a plain error", err)
	}
}

// IdleSince is the grace clock: the later of the agent bead's update and the
// hooked work bead's close (its update when never closed), plus a parked
// seat's park time.
func TestReapIdleSince(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	agentID := beads.PolecatBeadIDWithPrefix("gt", "gastown", "onyx")
	workID := "gt-work1"

	cases := []struct {
		name      string
		agentAt   time.Time
		workAt    time.Time
		workClose time.Time
		parkedAt  time.Time
		want      time.Time
	}{
		{name: "agent update only", agentAt: base, want: base},
		{name: "close is later than the agent bead", agentAt: base, workClose: base.Add(time.Hour), want: base.Add(time.Hour)},
		{name: "an open work bead's update counts", agentAt: base, workAt: base.Add(30 * time.Minute), want: base.Add(30 * time.Minute)},
		{name: "the close wins over the update", agentAt: base, workAt: base.Add(time.Minute), workClose: base.Add(2 * time.Hour), want: base.Add(2 * time.Hour)},
		{name: "a park stamps the clock", agentAt: base, parkedAt: base.Add(3 * time.Hour), want: base.Add(3 * time.Hour)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := newWorkBD(t)
			agent := beads.Issue{ID: agentID, UpdatedAt: tc.agentAt.Format(time.RFC3339)}
			work := beads.Issue{ID: workID, Status: "open"}
			if !tc.workAt.IsZero() {
				work.UpdatedAt = tc.workAt.Format(time.RFC3339)
			}
			if !tc.workClose.IsZero() {
				work.Status = "closed"
				work.ClosedAt = tc.workClose.Format(time.RFC3339)
			}
			if !tc.workAt.IsZero() || !tc.workClose.IsZero() {
				agent.Description = beads.FormatAgentDescription("onyx", &beads.AgentFields{
					RoleType: constants.RolePolecat, Rig: "gastown", HookBead: workID,
				})
				bd.db.Seed(work)
			}
			bd.db.Seed(agent)
			h := reapHost(t, enabledCleanup("gastown"), bd, nil)
			if !tc.parkedAt.IsZero() {
				seat := intent.Seat{Rig: "gastown", Role: constants.RolePolecat, Name: "onyx"}
				writeJSONFile(t, seat.Path(h.d.config.TownRoot), intent.Record{
					Desired: intent.DesiredPark, PausedAt: tc.parkedAt,
				})
			}
			got, err := h.IdleSince("gastown", "onyx")
			if err != nil {
				t.Fatalf("IdleSince: %v", err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("IdleSince = %v, want %v", got, tc.want)
			}
		})
	}
}

// An unreadable input is an error, never a guess: the tick then leaves the
// seat alone rather than reaping on a bad clock.
func TestReapIdleSinceUnreadableIsAnError(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{ID: beads.PolecatBeadIDWithPrefix("gt", "gastown", "onyx"),
		UpdatedAt: time.Now().Format(time.RFC3339)})
	bd.showErr = errors.New("bd show: connection refused")
	h := reapHost(t, enabledCleanup("gastown"), bd, nil)
	if got, err := h.IdleSince("gastown", "onyx"); err == nil {
		t.Fatalf("IdleSince answered %v on a failed read, want an error", got)
	}

	// A seat with no agent bead at all fails closed too: a missing record is
	// not "idle since forever".
	plain := newWorkBD(t)
	h = reapHost(t, enabledCleanup("gastown"), plain, nil)
	if _, err := h.IdleSince("gastown", "onyx"); err == nil {
		t.Fatal("IdleSince answered for a seat with no agent bead, want an error")
	}
}

// A non-terminal bead naming the branch — in its notes or by its assignee —
// is the claim that keeps the seat.
func TestReapBranchClaimed(t *testing.T) {
	t.Parallel()
	const branch = "polecat/onyx/gt-work1+abc123"
	cases := []struct {
		name  string
		issue beads.Issue
		want  string
	}{
		{
			name:  "resume_branch notes",
			issue: beads.Issue{ID: "gt-a", Status: "open", Notes: "resume_branch: " + branch},
			want:  "gt-a",
		},
		{
			name:  "the branch polecat's assignee",
			issue: beads.Issue{ID: "gt-b", Status: "in_progress", Assignee: "gastown/polecats/onyx"},
			want:  "gt-b",
		},
		{
			name:  "another polecat's bead",
			issue: beads.Issue{ID: "gt-c", Status: "open", Assignee: "gastown/polecats/ruby"},
		},
		{
			name:  "an unrelated note",
			issue: beads.Issue{ID: "gt-d", Status: "open", Notes: "resume_branch: polecat/ruby/gt-work9+zzz"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			bd := newWorkBD(t)
			bd.db.Seed(tc.issue)
			h := reapHost(t, enabledCleanup("gastown"), bd, nil)
			got, err := h.BranchClaimed("gastown", branch)
			if err != nil {
				t.Fatalf("BranchClaimed: %v", err)
			}
			if got != tc.want {
				t.Fatalf("BranchClaimed = %q, want %q", got, tc.want)
			}
		})
	}
}

// Every seat with a branch asks whether a bead claims it; one listing per rig
// answers them all, so a pass costs one listing rather than one per seat.
func TestReapBranchClaimedListsOncePerTick(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{ID: "gt-a", Status: "open"})
	h := reapHost(t, enabledCleanup("gastown"), bd, nil)
	for _, branch := range []string{"polecat/onyx/gt-1+a", "polecat/ruby/gt-2+b", "polecat/jade/gt-3+c"} {
		if _, err := h.BranchClaimed("gastown", branch); err != nil {
			t.Fatalf("BranchClaimed: %v", err)
		}
	}
	if len(bd.reads) != 5 {
		t.Fatalf("3 seats made %d bd reads, want 5 (one five-status listing): %v", len(bd.reads), bd.reads)
	}
}

// A failed list is unknown: the claim may be in the status that failed.
func TestReapBranchClaimedFailedListIsAnError(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.listErr = errors.New("bd list: connection refused")
	h := reapHost(t, enabledCleanup("gastown"), bd, nil)
	if got, err := h.BranchClaimed("gastown", "polecat/onyx/gt-work1+abc"); err == nil {
		t.Fatalf("BranchClaimed answered %q on a failed list, want an error", got)
	}
}

// A rig the block does not cover answers every ReapEnv method with an error,
// so a mis-scoped call reads as unknown and never as a removal.
func TestReapHostRefusesUncoveredRig(t *testing.T) {
	t.Parallel()
	gt := newFakeCLI(func([]string) cliReply { return cliReply{stdout: readGolden(t)} })
	h := reapHost(t, enabledCleanup("gastown"), nil, gt)
	for name, err := range map[string]error{
		"Recovery":  func() error { _, e := h.Recovery("beads", "onyx"); return e }(),
		"IdleSince": func() error { _, e := h.IdleSince("beads", "onyx"); return e }(),
		"BranchClaimed": func() error {
			_, e := h.BranchClaimed("beads", "polecat/onyx/gt-work1+abc")
			return e
		}(),
		"Reap": h.Reap("beads", "onyx"),
	} {
		if err == nil {
			t.Errorf("%s answered for an uncovered rig without error", name)
		}
	}
	if len(gt.recorded()) != 0 {
		t.Fatal("an uncovered rig started a gt subprocess")
	}
}

// The options wire the block into the pure pass: absent or disabled leaves
// Reap nil (the tick is unchanged), enabled is dry-run with the fail-safe
// defaults, and a nuke refusal is a refusal.
func TestReapOptionsWiring(t *testing.T) {
	t.Parallel()
	now := time.Now

	off := patrolScanOptions(&DaemonPatrolConfig{Patrols: &PatrolsConfig{
		PatrolScan: &PatrolScanConfig{Enabled: true},
	}}, now)
	if off.Reap != nil {
		t.Fatalf("an absent worktree_cleanup block set Reap = %+v", off.Reap)
	}
	if patrolScanOptions(nil, now).Reap != nil {
		t.Fatal("no config set Reap")
	}

	on := patrolScanOptions(&DaemonPatrolConfig{Patrols: &PatrolsConfig{
		PatrolScan: &PatrolScanConfig{Enabled: true, WorktreeCleanup: &agentconfig.WorktreeCleanupConfig{Enabled: true}},
	}}, now)
	want := &patrolscan.ReapOptions{
		DryRun:      true,
		Grace:       patrolscan.DefaultReapGrace,
		ParkedGrace: patrolscan.DefaultReapParkedGrace,
		MaxPerTick:  patrolscan.DefaultReapMaxPerTick,
	}
	if on.Reap == nil || *on.Reap != *want {
		t.Fatalf("Reap = %+v, want %+v", on.Reap, want)
	}
	if !on.Reap.DryRun {
		t.Fatal("an enabled block without dry_run must be dry-run")
	}
	if !on.IsRefusal(errReapRefused) {
		t.Fatal("a nuke refusal must read as a refusal, so the seat is blocked, not failed")
	}
	if !on.IsRefusal(supervisor.ErrRefused) {
		t.Fatal("a supervisor refusal must still read as a refusal")
	}
	if on.IsRefusal(errors.New("boom")) {
		t.Fatal("an ordinary error must not read as a refusal")
	}
}

// A seat whose agent bead records no timestamp is the zero time, which the
// pure pass reads as not eligible.
func TestReapIdleSinceWithNoTimestampsIsZero(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{ID: beads.PolecatBeadIDWithPrefix("gt", "gastown", "onyx")})
	h := reapHost(t, enabledCleanup("gastown"), bd, nil)
	got, err := h.IdleSince("gastown", "onyx")
	if err != nil {
		t.Fatalf("IdleSince: %v", err)
	}
	if !got.IsZero() {
		t.Fatalf("IdleSince = %v, want the zero time", got)
	}
}

// A malformed timestamp is unreadable input, not an absent one.
func TestReapIdleSinceMalformedTimestampIsAnError(t *testing.T) {
	t.Parallel()
	bd := newWorkBD(t)
	bd.db.Seed(beads.Issue{ID: beads.PolecatBeadIDWithPrefix("gt", "gastown", "onyx"),
		UpdatedAt: "yesterday"})
	h := reapHost(t, enabledCleanup("gastown"), bd, nil)
	if _, err := h.IdleSince("gastown", "onyx"); err == nil {
		t.Fatal("IdleSince accepted a malformed timestamp")
	}
}
