package cmd

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/polecat"
)

// The pool's seat count and `gt polecat list`'s counts_toward_capacity are two
// readings of one seat, and gt-v5n2be found them disagreeing on sessionless
// seats: the list counts a seat whose git check failed, while the pool skipped
// every sessionless seat whose hook was not held. The table below is the
// parity proof. Each row is a disposition the Goal names; the row feeds the
// facts that produce it to the production classifier, asserts the disposition
// it produced is the one the row names, and then asks the pool's own seat
// readers (poolSeatStateOccupied for a live seat, poolDeadHookedSessions for a
// sessionless one, poolLandingSessions for a submission) whether that seat is
// counted.
//
// A row with intended set names one of the two exemptions the Goal allows: a
// seat the pool frees through a named rule (gt-2nft, gt-b9ud0) rather than
// through the disposition's own counts_toward_capacity. Every other row
// asserts the pool's answer equals the list's.
func TestPoolCapacityModelsAgreeWithTheList(t *testing.T) {
	t.Parallel()

	// The facts every clean, measured seat starts from: an agent bead read, a
	// clean recorded status the live probe confirms, and no live hook.
	base := polecat.WorkstateFacts{
		State:          polecat.StateDone,
		HookBeadSafe:   true,
		CleanupStatus:  polecat.CleanupClean,
		GitStateSource: polecat.GitStateSourceLive,
	}

	openMR := base
	openMR.ActiveMR = "gt-mr"
	openMR.ActiveMRBlocker = "active_mr=gt-mr status=open"

	preserved := base
	preserved.UnpushedCommits = 2
	preserved.AssignedBeadTerminal = true

	gitCheckFailed := base
	gitCheckFailed.GitCheckFailed = true
	gitCheckFailed.GitStateSource = polecat.GitStateSourceUnknown

	// The check-recovery facts for the two merge-queue dispositions
	// (internal/polecat/manager.go, the MQ block): a pushed branch with
	// submittable work, no terminal bead and no merge request found. Neither
	// seat read below sets these, so this is the only way to produce them.
	mq := base
	mq.Branch = "polecat/nux/gt-v5n2be"
	mq.MQCheckRequired = true
	mq.HasSubmittableWork = true

	mqLookupFailed := mq
	mqLookupFailed.MQLookupFailed = true

	submitted := base
	submitted.State = polecat.StateSubmitted

	hooked := base
	hooked.HookBead = "gt-work"
	hooked.HookBeadSafe = false

	// A done seat with a clean, measured worktree and no recorded cleanup
	// status: the agent bead was read and the probe answered, so the list
	// frees it (gt-ui2x). Only the sessionless read sets AgentBeadRead for the
	// pool (decision A2 of gt-v5n2be).
	cleanNoCleanup := base
	cleanNoCleanup.CleanupStatus = ""
	cleanNoCleanup.AgentBeadRead = true

	// A sessionless polecat directory with no worktree and no cleanup status:
	// nothing measured the worktree, so the seat fails closed and the list
	// counts it (cleanup-unknown).
	unmeasured := polecat.WorkstateFacts{State: polecat.StateDone, HookBeadSafe: true}

	cases := []struct {
		name string
		// facts produce the disposition; the row fails if they produce another.
		facts       polecat.WorkstateFacts
		wantVerdict string
		wantReason  string
		// wantCounts is `gt polecat list --json`'s counts_toward_capacity for
		// the disposition: disposition.CountsTowardCapacity.
		wantCounts bool
		// live seats a running tmux session; the rest are sessionless.
		live bool
		// landed stages the intent record and work bead of a submission.
		landed bool
		// agentWorking is the agent bead's active-state answer.
		agentWorking bool
		hookHeld     bool
		// noDeadRead stages a read that never reached a disposition (a polecat
		// directory with no agent bead).
		noDeadRead bool
		// intended names the exemption the Goal allows a row to name.
		intended string
	}{
		{
			name:        "working",
			facts:       polecat.WorkstateFacts{State: polecat.StateWorking, HookBeadSafe: true, CleanupStatus: polecat.CleanupClean, GitStateSource: polecat.GitStateSourceLive},
			wantVerdict: polecat.WorkstateVerdictWorking,
			wantReason:  "not-idle",
			wantCounts:  true,
			live:        true,
			hookHeld:    true,
		},
		{
			name:         "working with a running session and an open MR",
			facts:        workingWithOpenMR(),
			wantVerdict:  polecat.WorkstateVerdictWorking,
			wantReason:   "not-idle",
			wantCounts:   true,
			live:         true,
			agentWorking: true,
			hookHeld:     true,
		},
		{
			name:        "done with an open MR",
			facts:       openMR,
			wantVerdict: polecat.WorkstateVerdictPendingMR,
			wantReason:  "active-mr-open",
			wantCounts:  false,
			intended:    "gt-2nft: the pool frees a done seat waiting on the refinery",
		},
		{
			name:        "done with preserved commits on a terminal bead",
			facts:       preserved,
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
			wantReason:  "git-unpushed",
			wantCounts:  false,
			intended:    "gt-b9ud0: the pool frees a seat whose commits are preserved on its branch",
		},
		{
			name:        "needs recovery with a git check that failed",
			facts:       gitCheckFailed,
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
			wantReason:  "git-check-failed",
			wantCounts:  true,
		},
		{
			name:        "needs MQ submit",
			facts:       mq,
			wantVerdict: polecat.WorkstateVerdictNeedsMQSubmit,
			wantReason:  "mq-not-submitted",
			wantCounts:  true,
			live:        true,
		},
		{
			name:        "MQ lookup failed",
			facts:       mqLookupFailed,
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
			wantReason:  "mq-lookup-failed",
			wantCounts:  true,
			live:        true,
		},
		{
			name:        "submitted for landing",
			facts:       submitted,
			wantVerdict: polecat.WorkstateVerdictSubmitted,
			wantReason:  "submitted-for-landing",
			wantCounts:  true,
			landed:      true,
		},
		{
			name:        "crashed with the bead hooked",
			facts:       hooked,
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
			wantReason:  "hook-still-set",
			wantCounts:  true,
			hookHeld:    true,
		},
		{
			name:        "done with a clean probe, no cleanup status and the agent bead read",
			facts:       cleanNoCleanup,
			wantVerdict: polecat.WorkstateVerdictSafeToNuke,
			wantReason:  "reusable",
			wantCounts:  false,
		},
		{
			name:        "no worktree and no cleanup status",
			facts:       unmeasured,
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
			wantReason:  "cleanup-unknown",
			wantCounts:  true,
		},
		{
			// The list reads the same directory as cleanup-unknown and counts
			// it; the pool's read never reached a disposition, and its zero
			// value must count the seat rather than free it (gt-dtx3ht).
			name:        "no agent bead",
			facts:       unmeasured,
			wantVerdict: polecat.WorkstateVerdictNeedsRecovery,
			wantReason:  "cleanup-unknown",
			wantCounts:  true,
			noDeadRead:  true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()

			// The disposition, from the facts through the production
			// classifier. Asserting it first is what keeps a row from naming a
			// disposition its facts do not produce.
			d := polecat.DecideWorkstate(polecat.NewWorkstateInput(c.facts))
			if d.Verdict != c.wantVerdict || d.Reason != c.wantReason {
				t.Fatalf("the facts produce %s/%s, the row names %s/%s (%+v)",
					d.Verdict, d.Reason, c.wantVerdict, c.wantReason, d)
			}
			if d.CountsTowardCapacity != c.wantCounts {
				t.Fatalf("counts_toward_capacity = %v, the row names %v", d.CountsTowardCapacity, c.wantCounts)
			}
			// The sessionless read decides the same facts with the agent bead
			// read (decision A2).
			deadFacts := c.facts
			deadFacts.AgentBeadRead = true
			dead := polecat.DecideWorkstate(polecat.NewWorkstateInput(deadFacts))

			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "ember")
			lister := &fakeLister{}
			if c.live {
				lister.sessions = map[string]map[string]string{
					"gt-ember": {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
				}
				recordSlungSeats(t, town, lister)
			}
			work := submittedWork()
			if c.landed {
				writeLandingSeat(t, town, "gastown", "ember", "gt-ember-work")
				work = submittedWork("gt-ember-work")
			}

			state := poolSeatState{
				Disposition:     d,
				DeadDisposition: dead,
				DeadRead:        !c.noDeadRead,
				AgentWorking:    c.agentWorking,
				HookHeld:        c.hookHeld,
			}
			disposition := func(rigName, polecatName string) (poolSeatState, error) {
				if rigName != "gastown" || polecatName != "ember" {
					t.Fatalf("unexpected seat read %s/%s", rigName, polecatName)
				}
				return state, nil
			}
			got, err := poolSeatSessionsWith(lister, town, disposition, hookHeldOf(disposition), work, seatPool("deepseek-flash", 3))
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			count := 0
			for _, s := range got {
				if s.rig == "gastown" && s.polecat == "ember" {
					count++
				}
			}

			if c.intended != "" {
				// The exemption rows: the pool reaches the free answer through
				// the named rule, and the row says which one.
				if count != 0 {
					t.Fatalf("the pool counted the seat %d time(s); %s", count, c.intended)
				}
				return
			}
			if count > 1 {
				t.Fatalf("the seat was counted %d times, want at most once", count)
			}
			if got, want := count == 1, c.wantCounts; got != want {
				t.Fatalf("the pool counts the seat %v, the list %v", got, want)
			}
		})
	}
}

// workingWithOpenMR is the seat the list reads as WORKING while the pool used
// to free it: an agent bead that is working with a merge request open.
func workingWithOpenMR() polecat.WorkstateFacts {
	f := polecat.WorkstateFacts{
		State:          polecat.StateWorking,
		HookBeadSafe:   true,
		CleanupStatus:  polecat.CleanupClean,
		GitStateSource: polecat.GitStateSourceLive,
	}
	f.ActiveMR = "gt-mr"
	f.ActiveMRBlocker = "active_mr=gt-mr status=open"
	return f
}

// TestPoolCapacityModelIntendedRows pins the two exemptions the table names:
// the pool's freeing rule is the one the Goal allows, it is reached through the
// named predicate, and the rows that name it are exactly those two.
func TestPoolCapacityModelIntendedRows(t *testing.T) {
	t.Parallel()

	// gt-2nft: a done seat waiting on the refinery, with nothing in flight.
	mr := workingWithOpenMR()
	mr.State = polecat.StateDone
	if poolSeatStateOccupied(poolSeatState{Disposition: polecat.DecideWorkstate(polecat.NewWorkstateInput(mr))}, nil) {
		t.Fatal("a done seat with an open MR holds a seat; gt-2nft frees it")
	}

	// gt-b9ud0: a terminal bead whose commits are preserved on the branch.
	preserved := polecat.WorkstateFacts{
		State:                polecat.StateDone,
		HookBeadSafe:         true,
		CleanupStatus:        polecat.CleanupClean,
		GitStateSource:       polecat.GitStateSourceLive,
		UnpushedCommits:      2,
		AssignedBeadTerminal: true,
	}
	d := polecat.DecideWorkstate(polecat.NewWorkstateInput(preserved))
	if !d.CommitsPreservedOnBranch {
		t.Fatalf("the fixture does not carry preserved commits: %+v", d)
	}
	if poolSeatStateOccupied(poolSeatState{Disposition: d}, nil) {
		t.Fatal("a seat with its commits preserved on a terminal bead holds a seat; gt-b9ud0 frees it")
	}

	// A seat that still holds live work is not idle, so neither exemption
	// frees it: the list reads it as working (live) or stalled (sessionless),
	// both of which count (gt-v5n2be).
	if !poolSeatStateOccupied(poolSeatState{Disposition: d, HookHeld: true}, nil) {
		t.Fatal("a seat whose only exemption is preserved commits was freed while still holding live work")
	}
	if !poolSeatStateOccupied(poolSeatState{Disposition: polecat.DecideWorkstate(polecat.NewWorkstateInput(mr)), HookHeld: true}, nil) {
		t.Fatal("a done seat with an open MR was freed while still holding live work")
	}
}

// TestPoolDeadReadFreesACleanProbeWithoutACleanupStatus pins decision A2 of
// gt-v5n2be: a sessionless seat whose cleanup_status is missing but whose
// worktree probe answered clean has no unsaved work, and the list frees it.
// The sessionless read sets AgentBeadRead for exactly that; the live read does
// not, so no seat the pool counts today is freed by the same read.
func TestPoolDeadReadFreesACleanProbeWithoutACleanupStatus(t *testing.T) {
	t.Parallel()

	town := t.TempDir()
	writeTestRigsConfig(t, town, "gastown")
	writeTownPolecatDir(t, town, "gastown", "ember")
	// The rig clone inside the seat directory is what resolvePolecatWorktree
	// probes; without it the read has no worktree to measure.
	if err := os.MkdirAll(filepath.Join(town, "gastown", "polecats", "ember", "gastown"), 0o755); err != nil {
		t.Fatal(err)
	}
	reads := poolSeatReads{
		agentBead: func(beads.Client, string) (*beads.Issue, *beads.AgentFields, error) {
			return &beads.Issue{ID: "gt-gastown-polecat-ember"},
				&beads.AgentFields{AgentState: "done", Branch: "polecat/gt-x", HookBead: ""}, nil
		},
		assigned: func(assignedIssueLookup, string, string) polecatActiveWorkEvidence {
			return polecatActiveWorkEvidence{}
		},
		show: func(beads.Client, string) (*beads.Issue, error) { return nil, beads.ErrNotFound },
		probe: func(string) polecat.LiveGitState {
			return polecat.LiveGitState{Source: polecat.GitStateSourceLive, Branch: "polecat/gt-x"}
		},
	}

	state, err := poolPolecatSeatStateWith(town, "gastown", "ember", reads)
	if err != nil {
		t.Fatalf("poolPolecatSeatStateWith: %v", err)
	}
	if !state.DeadRead {
		t.Fatal("the sessionless read did not report a measured disposition")
	}
	if state.DeadDisposition.CountsTowardCapacity {
		t.Fatalf("the sessionless read counts a dead seat with a clean probe and no cleanup status: %+v", state.DeadDisposition)
	}
	if !state.Disposition.CountsTowardCapacity {
		t.Fatalf("the live read freed a seat whose cleanup status was never verified: %+v", state.Disposition)
	}
}

// TestPoolCountsAWorkingSeatWithARunningSession is acceptance box 2 of
// gt-v5n2be, through the pool's real seat read. A seat whose agent bead says
// working is WORKING on the list, which counts toward capacity; the pool read
// used to flatten the active state to idle, see the open MR alone, and free the
// seat as idle-pr-open. The second case takes the session away from it (the
// patrol recorded the sandbox dead) and leaves an intent record naming another
// agent: the seat still counts.
func TestPoolCountsAWorkingSeatWithARunningSession(t *testing.T) {
	t.Parallel()

	fields := &beads.AgentFields{AgentState: "working", CleanupStatus: "clean", ActiveMR: "gt-mr"}
	reads := poolSeatReads{
		agentBead: func(beads.Client, string) (*beads.Issue, *beads.AgentFields, error) {
			return &beads.Issue{ID: "gt-gastown-polecat-ember"}, fields, nil
		},
		assigned: func(assignedIssueLookup, string, string) polecatActiveWorkEvidence {
			return polecatActiveWorkEvidence{}
		},
		show: func(beads.Client, string) (*beads.Issue, error) { return nil, beads.ErrNotFound },
		probe: func(string) polecat.LiveGitState {
			return polecat.LiveGitState{Source: polecat.GitStateSourceLive, Branch: "polecat/gt-x"}
		},
	}

	cases := []struct {
		name  string
		stage func(t *testing.T, town string)
	}{
		{
			name: "a working seat with a running session",
			stage: func(t *testing.T, town string) {
				recordSlungSeats(t, town, &fakeLister{sessions: map[string]map[string]string{
					"gt-ember": {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
				}})
			},
		},
		{
			name: "a working seat whose sandbox the patrol recorded dead and whose record names another agent",
			stage: func(t *testing.T, town string) {
				writeSandboxSeat(t, town, "gt-ember", "gastown", "ember", "another-agent",
					&intent.Progress{SampledAt: time.Unix(1760000000, 0), DeadSamples: 2})
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			writeTestRigsConfig(t, town, "gastown")
			writeTownPolecatDir(t, town, "gastown", "ember")
			c.stage(t, town)
			// The tmux session is there in both cases: the patrol's dead record
			// is what takes it away from the pool, not the session's absence.
			lister := &fakeLister{sessions: map[string]map[string]string{
				"gt-ember": {"GT_ROLE": "gastown/polecats/ember", "GT_AGENT": "deepseek-flash"},
			}}

			got, err := poolSeatSessionsWith(lister, town,
				poolDispositionAnswering(town, reads), poolHookHeldAnswering(town, reads), nil,
				seatPool("deepseek-flash", 3))
			if err != nil {
				t.Fatalf("poolSeatSessionsWith: %v", err)
			}
			seats := 0
			for _, s := range got {
				if s.rig == "gastown" && s.polecat == "ember" {
					seats++
				}
			}
			if seats != 1 {
				t.Fatalf("the pool counted the working seat %d time(s), want once (%+v)", seats, got)
			}

			// The list reads the same agent bead as a working seat, which is
			// the count the pool must not fall below.
			item := buildPolecatInventoryItem("gastown", "ember", fields, nil, polecatSessionSet{}, polecatInventoryEnv{Agent: "deepseek-flash"})
			if !item.Disposition.CountsTowardCapacity {
				t.Fatalf("the list does not count the seat the pool must count: %+v", item.Disposition)
			}
		})
	}
}
