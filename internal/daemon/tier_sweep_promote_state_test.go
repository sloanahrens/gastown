package daemon

import (
	"context"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/beadsfake"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/landworker"
	"github.com/steveyegge/gastown/internal/promote"
)

// midPushVerdictRepo is a promotion target whose push writes what the landing
// worker's verdict writes while the push runs: the record's last_green and
// last_run, at the path the promote path saves (gt-8iq4h).
type midPushVerdictRepo struct {
	*tierSweepPromoteRepo
	path  string
	green string
}

func (r *midPushVerdictRepo) PushWithEnv(_, refspec string, _ bool, _ []string) error {
	r.pushes = append(r.pushes, refspec)
	state := fileMainState{path: r.path}
	st, err := state.Load()
	if err != nil {
		return err
	}
	st.LastGreen, st.LastRun = r.green, r.green
	return state.Save(st)
}

// midPushSweep is a daemon whose rig promotes over repo, whose push writes the
// verdict `green` into the rig's record. It returns the daemon, the record's
// path and the commit the sweep promotes.
func midPushSweep(t *testing.T, green string) (*Daemon, string, string) {
	t.Helper()
	townRoot := t.TempDir()
	d := &Daemon{logger: discardLogger, config: &Config{TownRoot: townRoot}}
	path := RedMainStatePath(townRoot, "gastown")
	// What the record held before the push: a green commit main has since
	// left behind, so a save of the copy loaded before the push is visible.
	if err := (fileMainState{path: path}).Save(landworker.MainState{LastGreen: "cccc0000", LastRun: "cccc0000"}); err != nil {
		t.Fatal(err)
	}
	h := &tierSweepPromoteHarness{repo: &midPushVerdictRepo{
		tierSweepPromoteRepo: &tierSweepPromoteRepo{tip: "bbbb1111", ancestor: true},
		path:                 path,
		green:                green,
	}}
	d.tierSweepSeams.promote = h.build
	return d, path, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
}

// TestTierSweepPromoteKeepsAVerdictWrittenDuringThePush: the promotion push
// runs for seconds against the network while the landing worker writes its
// verdict into the same record, so the sweep re-reads the record after the
// push and replaces only the promotion State — last_green and last_run the
// push ran over are the record's, not the copy's (gt-8iq4h).
func TestTierSweepPromoteKeepsAVerdictWrittenDuringThePush(t *testing.T) {
	t.Parallel()
	d, path, sha := midPushSweep(t, "verdict-during-the-push")

	d.tierSweepPromoteWith("gastown", d.tierSweepSeams.promote("gastown", ""), sha)

	st, err := (fileMainState{path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastGreen != "verdict-during-the-push" || st.LastRun != "verdict-during-the-push" {
		t.Errorf("state = %+v, want the verdict written during the push kept", st)
	}
	if st.LastPromoted != sha {
		t.Errorf("LastPromoted = %q, want the promoted commit %s", st.LastPromoted, sha)
	}
}

// TestTierSweepRetryOwedPromotionKeepsAVerdictWrittenDuringThePush: the
// unchanged-main skip's retry runs the same push over the same record, so a
// verdict written while it runs survives it too (gt-fn9e6.52, gt-8iq4h).
func TestTierSweepRetryOwedPromotionKeepsAVerdictWrittenDuringThePush(t *testing.T) {
	t.Parallel()
	d, path, sha := midPushSweep(t, "verdict-during-the-retry")
	// The promotion the green sweep owed is still owed: the target holds a
	// commit the sweep has since left behind.
	if err := (fileMainState{path: path}).Save(landworker.MainState{
		LastGreen: "cccc0000", LastRun: "cccc0000", State: promote.State{LastPromoted: "dddd0000"},
	}); err != nil {
		t.Fatal(err)
	}

	d.tierSweepRetryOwedPromotion("gastown", "", sha)

	st, err := (fileMainState{path: path}).Load()
	if err != nil {
		t.Fatal(err)
	}
	if st.LastGreen != "verdict-during-the-retry" || st.LastRun != "verdict-during-the-retry" {
		t.Errorf("state = %+v, want the verdict written during the retry kept", st)
	}
	if st.LastPromoted != sha {
		t.Errorf("LastPromoted = %q, want the retried commit %s", st.LastPromoted, sha)
	}
}

// fixedLandings is a rig's landings file holding one record.
type fixedLandings struct{ rec land.LandingRecord }

func (f fixedLandings) LatestForBead(id string) (land.LandingRecord, bool, error) {
	if f.rec.BeadID != id {
		return land.LandingRecord{}, false, nil
	}
	return f.rec, true, nil
}

func (f fixedLandings) Recent(int) ([]land.LandingRecord, error) {
	return []land.LandingRecord{f.rec}, nil
}

// TestTierSweepPromoteLeavesTheLastGreenARevertIsJudgedAgainst: a revert is
// filed only for the one change between the last green and the red commit, so
// it is judged against the last green the record holds at that moment. A save
// of the copy from before the push would put back a commit main has left
// behind, and the culprit's landing — built on the commit a verdict recorded
// during the push called green — would read as one of several changes and
// never be reverted (gt-8iq4h).
func TestTierSweepPromoteLeavesTheLastGreenARevertIsJudgedAgainst(t *testing.T) {
	t.Parallel()
	const green = "1111111111111111111111111111111111111111"
	const red = "2222222222222222222222222222222222222222"
	d, path, sha := midPushSweep(t, green)

	d.tierSweepPromoteWith("gastown", d.tierSweepSeams.promote("gastown", ""), sha)

	bd := beadsfake.New(beadsfake.WithPrefix("gt"))
	bd.Seed(beads.Issue{ID: "gt-cul", Title: "culprit", Status: "closed", Type: "task", Assignee: "gastown/polecats/opal"})
	var status []string
	rm := &landworker.RedMain{
		Rig: "gastown", Beads: bd, Logf: t.Logf, State: fileMainState{path: path},
		Landings: fixedLandings{rec: land.LandingRecord{
			BeadID: "gt-cul", Rig: "gastown", Branch: "polecat/opal/gt-cul", Target: "main",
			Base: green, LandedCommit: red,
		}},
		Revert: func(_ context.Context, rec land.LandingRecord, _ string) (string, error) {
			if rec.LandedCommit != red {
				t.Errorf("reverted %s, want the culprit's landing %s", rec.LandedCommit, red)
			}
			return "8888888888888888888888888888888888888888", nil
		},
		Diff: func(context.Context, land.LandingRecord) ([]string, error) {
			return []string{"internal/a/a.go"}, nil
		},
		Rerun: func(context.Context, string, string, landworker.PostLand) landworker.PostLandResult {
			return landworker.PostLandResult{ExitCode: 1, Tail: "--- FAIL: TestA"}
		},
		Status: func(line string) { status = append(status, line) },
	}
	rm.Red(context.Background(), "make test-slow",
		landworker.PostLand{BeadID: "gt-cul", Commit: red, Target: "main"},
		landworker.PostLandResult{ExitCode: 2, Packages: []land.PackageResult{{Package: "example.com/a"}}})

	if last := status[len(status)-1]; !strings.Contains(last, "reverting gt-cul as ") {
		t.Fatalf("status %q; want the culprit's landing reverted against the last green the record holds", last)
	}
}
