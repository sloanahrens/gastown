package daemon

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

const tierSweepEarlyLine = "tier_sweep: gastown: red bead(s) closed and main moved; re-running now"

// earlyCase is one red-then-fixed scenario: the rig's record, the sweep beads
// still open, where origin/main is, and how long ago the last cycle ran.
type earlyCase struct {
	name         string
	lastSHA      string // the last cycle's tip
	lastGreenSHA string
	openTitles   []string
	mainSHA      string
	sinceLastRun time.Duration
	wantRun      bool
}

// setupEarly builds a daemon whose gastown record is as c says and whose hourly
// patrol clock was last set 20 minutes ago, so the interval is not due.
func setupEarly(t *testing.T, c earlyCase) (*Daemon, *tierSweepRunRecorder, *tierSweepFakeBeads, *bytes.Buffer) {
	t.Helper()
	now := atHour(time.Now(), 15)
	d, rec, bd, logs := newTierSweepDaemon(t, now, "gastown")
	rec.green()
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return c.mainSHA, nil }
	d.tierSweepSeams.worktree = func(context.Context, string, string, string) (func(), error) { return func() {}, nil }
	for _, title := range c.openTitles {
		bd.open = append(bd.open, &beads.Issue{ID: "gt-red", Title: title})
	}
	if err := savePatrolLastRun(d.config.TownRoot, "tier_sweep", now.Add(-20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	st := tierSweepState{LastRun: now.Add(-c.sinceLastRun), LastSHA: c.lastSHA, LastGreenSHA: c.lastGreenSHA}
	if err := writeTierSweepState(d.config.TownRoot, "gastown", st); err != nil {
		t.Fatal(err)
	}
	return d, rec, bd, logs
}

func TestTriggerTierSweep_EarlyReRunAfterRedBeadsLand(t *testing.T) {
	t.Parallel()
	redBead := tierSweepBeadPrefix("gastown", "shell") + "TestFoo"
	otherRigBead := tierSweepBeadPrefix("other", "shell") + "TestFoo"
	cases := []earlyCase{
		{
			name: "open red bead and main moved waits the interval", lastSHA: "red1", lastGreenSHA: "green0",
			openTitles: []string{redBead}, mainSHA: "new2", sinceLastRun: 15 * time.Minute, wantRun: false,
		},
		{
			name: "beads closed, main moved, gap passed runs now", lastSHA: "red1", lastGreenSHA: "green0",
			mainSHA: "new2", sinceLastRun: 15 * time.Minute, wantRun: true,
		},
		{
			name: "another rig's open bead does not hold this rig", lastSHA: "red1", lastGreenSHA: "green0",
			openTitles: []string{otherRigBead}, mainSHA: "new2", sinceLastRun: 15 * time.Minute, wantRun: true,
		},
		{
			name: "beads closed but main unmoved waits", lastSHA: "red1", lastGreenSHA: "green0",
			mainSHA: "red1", sinceLastRun: 15 * time.Minute, wantRun: false,
		},
		{
			name: "beads closed, main moved, only five minutes since the last run waits", lastSHA: "red1", lastGreenSHA: "green0",
			mainSHA: "new2", sinceLastRun: 5 * time.Minute, wantRun: false,
		},
		{
			name: "green record keeps the interval", lastSHA: "green0", lastGreenSHA: "green0",
			mainSHA: "new2", sinceLastRun: 15 * time.Minute, wantRun: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			d, rec, _, logs := setupEarly(t, c)

			d.triggerTierSweep()
			waitTierSweepIdle(t, d)

			ran := len(rec.stages) > 0
			if ran != c.wantRun {
				t.Fatalf("ran = %v, want %v\n%s", ran, c.wantRun, logs.String())
			}
			if logged := strings.Contains(logs.String(), tierSweepEarlyLine); logged != c.wantRun {
				t.Fatalf("early line logged = %v, want %v\n%s", logged, c.wantRun, logs.String())
			}
		})
	}
}

// An early re-run is a cycle like any other: it records the new tip, and a red
// result files a fresh bead. It leaves the patrol's hourly clock where it was.
func TestTriggerTierSweep_EarlyReRunRecordsTheCycleAndKeepsTheHourlyClock(t *testing.T) {
	t.Parallel()
	d, rec, bd, _ := setupEarly(t, earlyCase{
		lastSHA: "red1", lastGreenSHA: "green0", mainSHA: "new2", sinceLastRun: 15 * time.Minute,
	})
	rec.result = nil // no verdict line: RED
	before, _, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep")
	if err != nil {
		t.Fatal(err)
	}

	d.triggerTierSweep()
	waitTierSweepIdle(t, d)

	st := tierSweepTestState(t, d, "gastown")
	if st.LastSHA != "new2" || st.LastGreenSHA != "green0" {
		t.Fatalf("record = %+v, want LastSHA new2 with the old green tip", st)
	}
	if len(bd.created) == 0 {
		t.Fatal("a red early re-run must file its bead through the normal path")
	}
	after, _, err := loadPatrolLastRun(d.config.TownRoot, "tier_sweep")
	if err != nil {
		t.Fatal(err)
	}
	if !after.Equal(before) {
		t.Fatalf("hourly last-run moved from %v to %v; an early re-run must not consume the hour", before, after)
	}

	// The gap now bounds the loop: the record was just rewritten, so the next
	// heartbeat finds only seconds since LastRun.
	rec.stages = nil
	bd.open = nil
	d.tierSweepSeams.mainSHA = func(context.Context, string) (string, error) { return "newer3", nil }
	d.triggerTierSweep()
	waitTierSweepIdle(t, d)
	if len(rec.stages) != 0 {
		t.Fatalf("stages = %+v, want none inside the retry gap", rec.stages)
	}
}

// byStatusBeads answers List only for the statuses it holds a bead under.
type byStatusBeads struct {
	tierSweepFakeBeads
	byStatus map[string][]*beads.Issue
}

func (b *byStatusBeads) List(opts beads.ListOptions) ([]*beads.Issue, error) {
	return b.byStatus[opts.Status], nil
}

// A hooked or in-progress bead is a fix still under way, the same as an open one.
func TestTierSweepRedBeadsOpen_CountsEveryUnlandedStatus(t *testing.T) {
	t.Parallel()
	bead := []*beads.Issue{{ID: "gt-x", Title: tierSweepBeadPrefix("gastown", "race") + "TestBar"}}
	for _, status := range []string{"open", "in_progress", "hooked", "closed"} {
		bd := &byStatusBeads{byStatus: map[string][]*beads.Issue{status: bead}}
		open, err := tierSweepRedBeadsOpen(bd, "gastown")
		if err != nil {
			t.Fatal(err)
		}
		if want := status != "closed"; open != want {
			t.Errorf("bead held as %q: open = %v, want %v", status, open, want)
		}
	}
}
