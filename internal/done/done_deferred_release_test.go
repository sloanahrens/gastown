package done

import (
	"errors"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
)

const (
	releaseBeadID = "gt-work"
	releaseSeat   = "gastown/polecats/nux"
	releaseOther  = "gastown/polecats/agate"
)

// releaseBead is the work bead a run holds, with the given status, assignee
// and labels.
func releaseBead(status, assignee string, labels ...string) beads.Issue {
	return beads.Issue{ID: releaseBeadID, Title: "the work", Type: "task", Status: status, Assignee: assignee, Labels: labels}
}

// deferredReleaseRun is a run that has just ended with exit, holding the work
// bead as releaseSeat. bd is the store the run's release opens; env carries
// GT_DONE_FROM_HANDOFF for a handoff. The run is a crew run, which keeps
// reportDone from retiring a tmux session: the release does not depend on the
// role.
func deferredReleaseRun(t *testing.T, seen *seenDoneEvent, bd beads.Client, env map[string]string) *doneRun {
	t.Helper()
	r := recordDoneTestRun(seen)
	r.townRoot = t.TempDir()
	r.rigName = "gastown"
	r.issueID = releaseBeadID
	r.sender = releaseSeat
	r.env = func(key string) string { return env[key] }
	r.openSource = func(string, string) beads.Client { return bd }
	return r
}

// releaseFailClient fails the read or the write of the work bead, so a test
// can drive the release's refusal branches.
type releaseFailClient struct {
	beads.Client
	showErr   error
	updateErr error
}

func (c releaseFailClient) Show(id string) (*beads.Issue, error) {
	if c.showErr != nil {
		return nil, c.showErr
	}
	return c.Client.Show(id)
}

func (c releaseFailClient) Update(id string, opts beads.UpdateOptions) error {
	if c.updateErr != nil {
		return c.updateErr
	}
	return c.Client.Update(id, opts)
}

// TestDeferredExitReleasesTheBeadItPaused: a DEFERRED exit returns the bead
// the seat held to open and unassigned, with the DEFERRED note on it. Every
// bead it may not touch is left as it was: a closed bead stays closed, a bead
// in landing keeps its label and assignee, a rig identity bead and a bead
// another actor claimed are not reopened, and an ESCALATED or COMPLETED exit
// releases nothing. A handoff's DEFERRED is not an exit at all.
func TestDeferredExitReleasesTheBeadItPaused(t *testing.T) {
	t.Parallel()
	inProgress := string(beads.StatusInProgress)
	closed := string(beads.StatusClosed)
	handoff := map[string]string{EnvFromHandoff: "1"}
	cases := []struct {
		name         string
		exit         string
		env          map[string]string
		seed         beads.Issue
		absent       bool
		wantStatus   string
		wantAssignee string
		wantNote     bool
	}{
		{
			name:         "deferred releases the bead the seat held",
			exit:         ExitDeferred,
			seed:         releaseBead(inProgress, releaseSeat),
			wantStatus:   string(beads.StatusOpen),
			wantAssignee: "",
			wantNote:     true,
		},
		{
			name:         "deferred leaves a closed bead closed",
			exit:         ExitDeferred,
			seed:         releaseBead(closed, releaseSeat),
			wantStatus:   closed,
			wantAssignee: releaseSeat,
		},
		{
			name:         "deferred leaves a bead in landing with its label and assignee",
			exit:         ExitDeferred,
			seed:         releaseBead(inProgress, releaseSeat, land.LabelReadyToLand),
			wantStatus:   inProgress,
			wantAssignee: releaseSeat,
		},
		{
			name:         "deferred leaves a rig identity bead alone",
			exit:         ExitDeferred,
			seed:         releaseBead(inProgress, releaseSeat, "gt:rig"),
			wantStatus:   inProgress,
			wantAssignee: releaseSeat,
		},
		{
			name:         "deferred leaves a bead another actor claimed",
			exit:         ExitDeferred,
			seed:         releaseBead(inProgress, releaseOther),
			wantStatus:   inProgress,
			wantAssignee: releaseOther,
		},
		{
			name:         "deferred with no bead left to release is not an error",
			exit:         ExitDeferred,
			absent:       true,
			wantAssignee: "",
		},
		{
			name:         "a handoff's deferred is not an exit and keeps the bead",
			exit:         ExitDeferred,
			env:          handoff,
			seed:         releaseBead(inProgress, releaseSeat),
			wantStatus:   inProgress,
			wantAssignee: releaseSeat,
		},
		{
			name:         "escalated keeps the bead for the operator",
			exit:         ExitEscalated,
			seed:         releaseBead(inProgress, releaseSeat),
			wantStatus:   inProgress,
			wantAssignee: releaseSeat,
		},
		{
			name:         "completed releases nothing",
			exit:         ExitCompleted,
			seed:         releaseBead(inProgress, releaseSeat),
			wantStatus:   inProgress,
			wantAssignee: releaseSeat,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New(beadsfake.WithPrefix("gt"))
			if !tc.absent {
				db.Seed(tc.seed)
			}
			var seen seenDoneEvent
			r := deferredReleaseRun(t, &seen, db, tc.env)
			if err := reportDone(r, tc.exit); err != nil {
				t.Fatalf("reportDone(%s): %v", tc.exit, err)
			}
			if tc.absent {
				if _, err := db.Show(releaseBeadID); !errors.Is(err, beads.ErrNotFound) {
					t.Fatalf("show %s: err = %v, want the bead still absent", releaseBeadID, err)
				}
				return
			}
			got, err := db.Show(releaseBeadID)
			if err != nil {
				t.Fatalf("show %s: %v", releaseBeadID, err)
			}
			if got.Status != tc.wantStatus {
				t.Errorf("status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.Assignee != tc.wantAssignee {
				t.Errorf("assignee = %q, want %q", got.Assignee, tc.wantAssignee)
			}
			hasNote := strings.Contains(got.Notes, "DEFERRED by "+releaseSeat)
			if hasNote != tc.wantNote {
				t.Errorf("DEFERRED note present = %v, want %v; notes %q", hasNote, tc.wantNote, got.Notes)
			}
		})
	}
}

// TestDeferredExitLeavesAWorkflowStepAlone: a workflow step's DEFERRED exit is a
// completion, so a step the agent-state write could not close is not reopened
// by the release.
func TestDeferredExitLeavesAWorkflowStepAlone(t *testing.T) {
	t.Parallel()
	const step = "gt-work-wfs-1"
	db := beadsfake.New(beadsfake.WithPrefix("gt"))
	db.Seed(beads.Issue{ID: step, Title: "step", Type: "task", Status: string(beads.StatusInProgress), Assignee: releaseSeat})
	var seen seenDoneEvent
	r := deferredReleaseRun(t, &seen, db, nil)
	r.issueID = step
	if err := reportDone(r, ExitDeferred); err != nil {
		t.Fatalf("reportDone(DEFERRED) on a workflow step: %v", err)
	}
	got, err := db.Show(step)
	if err != nil {
		t.Fatalf("show %s: %v", step, err)
	}
	if got.Status != string(beads.StatusInProgress) || got.Assignee != releaseSeat {
		t.Errorf("step = status %q assignee %q, want it left as the seat had it", got.Status, got.Assignee)
	}
}

// TestDeferredExitRefusesAReleaseItCannotMake: a bead the release cannot read
// or write fails the exit, and the bead keeps the seat's claim with no DEFERRED
// note. The exit is never reported clean over a bead the release did not free.
func TestDeferredExitRefusesAReleaseItCannotMake(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		wrap func(beads.Client) beads.Client
	}{
		{
			name: "a read that fails",
			wrap: func(c beads.Client) beads.Client {
				return releaseFailClient{Client: c, showErr: errors.New("bd show: connection refused")}
			},
		},
		{
			name: "a write that fails",
			wrap: func(c beads.Client) beads.Client {
				return releaseFailClient{Client: c, updateErr: errors.New("bd update: store locked")}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := beadsfake.New(beadsfake.WithPrefix("gt"))
			db.Seed(releaseBead(string(beads.StatusInProgress), releaseSeat))
			var seen seenDoneEvent
			r := deferredReleaseRun(t, &seen, tc.wrap(db), nil)
			if err := reportDone(r, ExitDeferred); err == nil {
				t.Fatal("reportDone(DEFERRED) = nil, want the failed release returned")
			}
			got, err := db.Show(releaseBeadID)
			if err != nil {
				t.Fatalf("show %s: %v", releaseBeadID, err)
			}
			if got.Status != string(beads.StatusInProgress) || got.Assignee != releaseSeat {
				t.Errorf("bead = status %q assignee %q, want it still held by the seat", got.Status, got.Assignee)
			}
			if strings.Contains(got.Notes, "DEFERRED by") {
				t.Errorf("notes = %q, want no DEFERRED note from a failed release", got.Notes)
			}
		})
	}
}
