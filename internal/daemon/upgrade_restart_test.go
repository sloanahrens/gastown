package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// upgradeRepo is where fakeHistory's commits live, as a marker's repo.
const upgradeRepo = "/repo"

// fakeHistory gives d a gitfake repository at upgradeRepo holding a linear
// history, one commit per name in ancestry order (a..b..c), each name a
// branch on its commit. A name outside the history resolves to nothing, so
// git reports it unknown. With no names there is no repository at all.
func fakeHistory(t *testing.T, d *Daemon, names ...string) {
	t.Helper()
	f := useGitfake(t, d)
	if len(names) == 0 {
		return
	}
	f.InitBare(t, upgradeRepo)
	for _, n := range names {
		id := f.Commit(t, upgradeRepo, "main", n, map[string]string{"n": n})
		f.SetRef(t, upgradeRepo, "refs/heads/"+n, id)
	}
}

func withOwnCommit(d *Daemon, c string) {
	d.buildCommitFn = func() string { return c }
}

func captureEscalations(d *Daemon) *[]string {
	var keys []string
	d.seams.upgradeEscalate = func(_ *Daemon, key, msg string) { keys = append(keys, key) }
	return &keys
}

func upgradeTestDaemon(t *testing.T) *Daemon {
	t.Helper()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Daemon{config: &Config{TownRoot: town}, logger: log.New(io.Discard, "", 0)}
}

func writeMarker(t *testing.T, d *Daemon, m restartPendingMarker) {
	t.Helper()
	data, _ := json.Marshal(m)
	if err := os.WriteFile(restartMarkerPath(d.config.TownRoot), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func markerExists(t *testing.T, d *Daemon) bool {
	t.Helper()
	_, err := os.Stat(restartMarkerPath(d.config.TownRoot))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}

func readReceipts(t *testing.T, d *Daemon) []installReceipt {
	t.Helper()
	f, err := os.Open(installReceiptsPath(d.config.TownRoot))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []installReceipt
	s := bufio.NewScanner(f)
	for s.Scan() {
		var r installReceipt
		if err := json.Unmarshal(s.Bytes(), &r); err != nil {
			t.Fatalf("bad receipt line %q: %v", s.Text(), err)
		}
		out = append(out, r)
	}
	return out
}

func TestUpgradeCoveredMarkerClearedWithReceipt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, marker string }{
		{"equal", "bbb"},
		{"ancestor", "aaa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := upgradeTestDaemon(t)
			captureEscalations(d)
			withOwnCommit(d, "bbb")
			fakeHistory(t, d, "aaa", "bbb", "ccc")
			writeMarker(t, d, restartPendingMarker{Commit: tc.marker, Source: "post-merge",
				RequestedAt: time.Now().Add(-90 * time.Second).UTC().Format(time.RFC3339), Repo: "/repo"})

			if d.checkUpgradeRestart(time.Now()) {
				t.Fatal("covered marker must not request a restart")
			}
			if markerExists(t, d) {
				t.Fatal("covered marker not deleted")
			}
			rs := readReceipts(t, d)
			if len(rs) != 1 || rs[0].Event != "daemon_restarted" || rs[0].Commit != tc.marker || rs[0].Source != "post-merge" {
				t.Fatalf("receipts = %+v, want one daemon_restarted for %s", rs, tc.marker)
			}
			if rs[0].DurationS < 89 || rs[0].DurationS > 120 {
				t.Fatalf("duration_s = %v, want ~90", rs[0].DurationS)
			}
		})
	}
}

// gt-ccyw0: a tier sweep cycle in flight holds the restart until it closes,
// and only up to tierSweepRestartCap.
func TestUpgradeWaitsForARunningTierSweepUpToTheCap(t *testing.T) {
	t.Parallel()

	t.Run("no sweep running restarts now", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
		if !d.checkUpgradeRestart(time.Now()) {
			t.Fatal("idle daemon with no sweep running must restart")
		}
	})

	t.Run("a sweep that closes releases the restart", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

		now := time.Now()
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		// gt-rtbbr: one line per heartbeat that finds the daemon not idle, so
		// a wait is never silent. Two checks, two lines, same hold.
		if got := strings.Count(logs.String(), "waiting for the tier sweep that is running"); got != 2 {
			t.Fatalf("sweep wait line logged %d times, want once per heartbeat:\n%s", got, logs.String())
		}

		// The cycle closes; the next check restarts without waiting out the cap.
		d.tierSweepRunning.Store(false)
		if !d.checkUpgradeRestart(now.Add(2 * time.Minute)) {
			t.Fatal("a closed tier sweep must not hold the restart")
		}
		if !strings.Contains(logs.String(), "the tier sweep closed; restarting") {
			t.Fatalf("no release line for the closed sweep:\n%s", logs.String())
		}
	})

	t.Run("a sweep still running at the cap is cut short", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		keys := captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

		now := time.Now()
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(tierSweepRestartCap-time.Minute)) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		if !d.checkUpgradeRestart(now.Add(tierSweepRestartCap)) {
			t.Fatal("a sweep still running at the cap must not hold the restart further")
		}
		if !strings.Contains(logs.String(), "cutting the sweep short") {
			t.Fatalf("no cut-short line at the cap:\n%s", logs.String())
		}
		if len(*keys) != 0 {
			t.Fatalf("escalated: %v", *keys)
		}
	})
}

// The sweep hold is a quarter hour: long enough for a normal cycle, short
// enough that an install is not stuck behind a hung one (gt-ccyw0).
func TestTierSweepRestartCapIsAQuarterHour(t *testing.T) {
	t.Parallel()
	if tierSweepRestartCap != 15*time.Minute {
		t.Fatalf("tierSweepRestartCap = %s, want 15m", tierSweepRestartCap)
	}
}

// The receipt's duration_s is an integer, matching the shell writer.
func TestUpgradeReceiptDurationIsInteger(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa")
	writeMarker(t, d, restartPendingMarker{Commit: "aaa", Repo: "/repo",
		RequestedAt: time.Now().Add(-5 * time.Second).UTC().Format(time.RFC3339)})
	d.checkUpgradeRestart(time.Now())
	data, err := os.ReadFile(installReceiptsPath(d.config.TownRoot))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	f, ok := raw["duration_s"].(float64)
	if !ok || f != float64(int64(f)) {
		t.Fatalf("duration_s = %#v, want an integer", raw["duration_s"])
	}
}

func TestUpgradeNewerMarkerBusyDoesNotRestart(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	keys := captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	d.scheduledSlingsRunning.Store(true)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

	now := time.Now()
	if d.checkUpgradeRestart(now) {
		t.Fatal("busy daemon must not restart")
	}
	if d.checkUpgradeRestart(now.Add(29 * time.Minute)) {
		t.Fatal("busy daemon must not restart")
	}
	if len(*keys) != 0 {
		t.Fatalf("escalated before 30m: %v", *keys)
	}
	d.checkUpgradeRestart(now.Add(31 * time.Minute))
	d.checkUpgradeRestart(now.Add(40 * time.Minute))
	if len(*keys) != 1 || (*keys)[0] != "daemon:restart-pending-stuck" {
		t.Fatalf("escalations = %v, want exactly one daemon:restart-pending-stuck", *keys)
	}
	if d.upgradeRestartRequested.Load() || !markerExists(t, d) {
		t.Fatal("busy daemon must keep waiting with the marker in place")
	}

	// The idle predicate is read live: once the work finishes, the next
	// heartbeat restarts.
	d.scheduledSlingsRunning.Store(false)
	if !d.checkUpgradeRestart(now.Add(41 * time.Minute)) {
		t.Fatal("daemon that became idle must restart on the next check")
	}
}

// gt-rtbbr: with the drain on, the restart follows the last landing pass at the
// first heartbeat after it ends — housekeeping still in flight does not add a
// heartbeat to the wait.
func TestUpgradeRestartFollowsTheLastLandingPass(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	d.landingPasses.Add(1)
	d.landingStates.setBead("gastown", "gt-x", now)
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a landing pass")
	}

	// The pass ends; a plugin run and a dog cycle are still in flight, past
	// the cap that bounds them.
	d.landingStates.endPass("gastown")
	d.landingPasses.Add(-1)
	d.scripts = newScriptRunner()
	d.scripts.tryStart("hm-sync")
	d.compactorDogRunning = true
	if !d.checkUpgradeRestart(now.Add(3 * time.Minute)) {
		t.Fatal("housekeeping still in flight held the restart past the first heartbeat after the landing pass")
	}
	if !strings.Contains(logs.String(), "a plugin run hm-sync") {
		t.Fatalf("the cut-short line did not name the plugin run:\n%s", logs.String())
	}
}

// gt-rtbbr: a plugin run or dog cycle is work the new daemon reruns, so it
// holds a restart only up to housekeepingRestartCap — and the wait says which
// one it is. The 2026-10-05 waits named nothing.
func TestUpgradeWaitsForHousekeepingUpToTheCap(t *testing.T) {
	t.Parallel()

	t.Run("inside the cap it holds, and the wait names it", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.scripts = newScriptRunner()
		d.scripts.tryStart("hm-sync")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		if d.checkUpgradeRestart(time.Now()) {
			t.Fatal("restarted under a plugin run inside the cap")
		}
		if !strings.Contains(logs.String(), "waiting for a plugin run hm-sync (0m)") {
			t.Fatalf("the plugin run was not named in the wait line:\n%s", logs.String())
		}
	})

	t.Run("a dog cycle at the cap is cut short", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.compactorDogRunning = true
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a dog cycle inside the cap")
		}
		if !d.checkUpgradeRestart(now.Add(housekeepingRestartCap)) {
			t.Fatal("a dog cycle at its cap must not hold the restart further")
		}
		if !strings.Contains(logs.String(), "the compactor dog cycle is still in flight after 2m0s; restarting anyway") {
			t.Fatalf("the cut-short line did not name the dog cycle:\n%s", logs.String())
		}
	})
}

// gt-rtbbr review (om score 0.45): daemonWorkHold is hard-first, so soft work in
// flight with hard work can never mask it and let the restart cut at
// housekeepingRestartCap over a landing pass, a steward job, a scheduled slings
// cycle or a held install lock.
func TestUpgradeHardHoldIsNotMaskedBySoftWork(t *testing.T) {
	t.Parallel()

	t.Run("a plugin run does not hide a scheduled slings cycle", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.scripts = newScriptRunner()
		d.scripts.tryStart("hm-sync")
		d.scheduledSlingsRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a scheduled slings cycle")
		}
		// Past the housekeeping cap the plugin run alone would be cut; the
		// slings cycle alongside it must keep holding.
		if d.checkUpgradeRestart(now.Add(housekeepingRestartCap)) {
			t.Fatal("the restart cut over a scheduled slings cycle at the housekeeping cap")
		}
		if !strings.Contains(logs.String(), "waiting for a scheduled slings cycle") {
			t.Fatalf("the wait named the soft hold instead of the hard one:\n%s", logs.String())
		}
		if strings.Contains(logs.String(), "a scheduled slings cycle is still in flight") {
			t.Fatalf("a hard hold was cut:\n%s", logs.String())
		}

		// The cycle ends; the plugin run alone is past the cap by now, so the
		// restart goes rather than waiting out a fresh cap.
		d.scheduledSlingsRunning.Store(false)
		if !d.checkUpgradeRestart(now.Add(housekeepingRestartCap + time.Minute)) {
			t.Fatal("the restart must go once the hard hold clears")
		}
	})

	t.Run("a dog cycle does not hide a held install lock", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.compactorDogRunning = true
		holdInstallLock(t, writeInstallLock(t, d))
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(housekeepingRestartCap)) {
			t.Fatal("the restart ran over a held install lock")
		}
		if !strings.Contains(logs.String(), "waiting for the install lock") {
			t.Fatalf("the wait did not name the install lock:\n%s", logs.String())
		}
	})
}

// gt-rtbbr review (om, minor): the tier-sweep release line must not claim a
// restart that a soft hold then keeps waiting for. Every "restarting" line is
// written on the heartbeat that actually restarts.
func TestUpgradeSweepReleaseDoesNotClaimAWaitingRestart(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	d.tierSweepRunning.Store(true)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a running tier sweep")
	}

	// The sweep closes, but a plugin run starts in the same breath, inside its
	// cap: the heartbeat names the plugin run and claims nothing.
	d.tierSweepRunning.Store(false)
	d.scripts = newScriptRunner()
	d.scripts.tryStart("hm-sync")
	if d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("restarted under a plugin run inside the cap")
	}
	if strings.Contains(logs.String(), "the tier sweep closed; restarting") {
		t.Fatalf("the release line claimed a restart that then waited:\n%s", logs.String())
	}
	if !strings.Contains(logs.String(), "waiting for a plugin run hm-sync") {
		t.Fatalf("the plugin run was not named after the sweep closed:\n%s", logs.String())
	}

	// Once the plugin run is past its cap, the restart goes and says so.
	if !d.checkUpgradeRestart(now.Add(housekeepingRestartCap + time.Minute)) {
		t.Fatal("the restart must go once the plugin run is past the cap")
	}
	if !strings.Contains(logs.String(), "a plugin run hm-sync is still in flight after 2m0s; restarting anyway") {
		t.Fatalf("no cut-short line naming the plugin run:\n%s", logs.String())
	}
}

// gt-rtbbr: every heartbeat that finds the daemon not idle names the hold, not
// just the first one — a repeated wait used to log nothing at all.
func TestUpgradeWaitIsNamedEachHeartbeat(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	d.patrolScanRunning.Store(true)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("restarted under a patrol scan inside the cap")
	}
	if got := strings.Count(logs.String(), "waiting for a patrol scan"); got != 2 {
		t.Fatalf("patrol scan wait line logged %d times, want one per heartbeat:\n%s", got, logs.String())
	}
}

// The housekeeping cap is short on purpose: it must not cost a heartbeat after
// the landing work it trails (gt-rtbbr).
func TestHousekeepingRestartCapIsTwoMinutes(t *testing.T) {
	t.Parallel()
	if housekeepingRestartCap != 2*time.Minute {
		t.Fatalf("housekeepingRestartCap = %s, want 2m", housekeepingRestartCap)
	}
}

// gt-gb4ij: a post-landing run holds the restart until its verdict, but
// only up to postLandRestartCap; the worker reruns the tip after the start.
func TestUpgradeWaitsForAPostLandRunUpToTheCap(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	keys := captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	d.postLandRuns.Add(1)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

	now := time.Now()
	if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(postLandRestartCap-time.Minute)) {
		t.Fatal("restarted under a post-land run before the cap")
	}
	if !d.checkUpgradeRestart(now.Add(postLandRestartCap)) {
		t.Fatal("still waiting for the post-land run at the cap")
	}
	if len(*keys) != 0 {
		t.Fatalf("escalated: %v", *keys)
	}

	// The run finishing releases the restart at once.
	d2 := upgradeTestDaemon(t)
	captureEscalations(d2)
	withOwnCommit(d2, "aaa")
	fakeHistory(t, d2, "aaa", "bbb")
	d2.postLandRuns.Add(1)
	writeMarker(t, d2, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
	if d2.checkUpgradeRestart(now) {
		t.Fatal("restarted under a post-land run")
	}
	d2.postLandRuns.Add(-1)
	if !d2.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("finished post-land run must not hold the restart")
	}
}

// gt-gb4ij follow-up: every install rewrites the marker with a newer commit.
// The cap clock runs from the first pending marker, not the latest one, or a
// steady stream of installs holds the old binary forever (observed 2026-09-30:
// 20+ minutes of post-land runs, a new marker every ~5 minutes).
func TestUpgradePostLandCapCountsFromTheFirstPendingMarker(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb", "ccc")
	d.postLandRuns.Add(1)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

	now := time.Now()
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a post-land run before the cap")
	}
	// A newer install replaces the marker halfway through the wait.
	writeMarker(t, d, restartPendingMarker{Commit: "ccc", Repo: "/repo"})
	if d.checkUpgradeRestart(now.Add(postLandRestartCap / 2)) {
		t.Fatal("restarted under a post-land run before the cap")
	}
	if !d.checkUpgradeRestart(now.Add(postLandRestartCap)) {
		t.Fatal("a newer marker restarted the cap clock: still waiting at the cap")
	}
}

func TestUpgradeNewerMarkerIdleRequestsRestartAndStampsAttempt(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	// An unknown field from a future writer must survive the daemon's rewrite.
	if err := os.WriteFile(restartMarkerPath(d.config.TownRoot),
		[]byte(`{"commit":"bbb","repo":"/repo","future_field":42}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if !d.checkUpgradeRestart(time.Now()) {
		t.Fatal("idle daemon with newer marker must request restart")
	}
	if !d.upgradeRestartRequested.Load() {
		t.Fatal("upgradeRestartRequested not set")
	}
	if m, _ := readRestartMarker(d.config.TownRoot); m == nil || m.AttemptedFrom != "" {
		t.Fatalf("marker after the request = %+v, want attempted_from unset until the commit", m)
	}
	// The run loop commits the request: that is where the attempt is stamped.
	if err := d.exitForUpgradeIfRequested(&State{Running: true}); !errors.Is(err, ErrRestartForUpgrade) {
		t.Fatalf("commit err = %v, want ErrRestartForUpgrade", err)
	}
	m, err := readRestartMarker(d.config.TownRoot)
	if err != nil || m == nil || m.AttemptedFrom != "aaa" {
		t.Fatalf("marker after the commit = %+v (err %v), want attempted_from=aaa", m, err)
	}
	data, _ := os.ReadFile(restartMarkerPath(d.config.TownRoot))
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil || raw["future_field"] != float64(42) {
		t.Fatalf("unknown field lost on rewrite: %s", data)
	}
}

// TestStampRestartAttemptDoesNotOverwriteANewerMarker pins gt-oyrav (D9):
// install-gt.sh replaces daemon/restart-pending.json with a newer install's
// marker, and this stamp reads, edits and renames the same path. A marker
// swapped between the read and the rename must survive: renaming the older one
// back over it would clear a request for an install the daemon has not run.
func TestStampRestartAttemptDoesNotOverwriteANewerMarker(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	writeMarker(t, d, restartPendingMarker{Commit: "aaa", Repo: upgradeRepo})

	swapped := false
	err := stampRestartAttemptWith(func() {
		swapped = true
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})
	}, d.config.TownRoot, "aaa")

	if !swapped {
		t.Fatal("the test never swapped the marker")
	}
	if !errors.Is(err, errRestartMarkerMoved) {
		t.Fatalf("err = %v, want errRestartMarkerMoved", err)
	}
	m, readErr := readRestartMarker(d.config.TownRoot)
	if readErr != nil || m == nil || m.Commit != "bbb" || m.AttemptedFrom != "" {
		t.Fatalf("marker = %+v (err %v), want the newer marker untouched", m, readErr)
	}
	if _, statErr := os.Stat(restartMarkerPath(d.config.TownRoot) + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("the abandoned temp file was left beside the marker: %v", statErr)
	}
}

// The stamp still lands when nothing replaced the marker, and it keeps the
// fields this daemon does not know about.
func TestStampRestartAttemptStampsTheMarkerItRead(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	if err := os.WriteFile(restartMarkerPath(d.config.TownRoot),
		[]byte(`{"commit":"bbb","repo":"/repo","future_field":42}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := stampRestartAttempt(d.config.TownRoot, "aaa"); err != nil {
		t.Fatalf("stampRestartAttempt: %v", err)
	}
	m, err := readRestartMarker(d.config.TownRoot)
	if err != nil || m == nil || m.Commit != "bbb" || m.AttemptedFrom != "aaa" {
		t.Fatalf("marker = %+v (err %v), want attempted_from=aaa on commit bbb", m, err)
	}
	data, _ := os.ReadFile(restartMarkerPath(d.config.TownRoot))
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil || raw["future_field"] != float64(42) {
		t.Fatalf("unknown field lost on rewrite: %s", data)
	}
}

func TestUpgradeNoEffectRestartDoesNotLoop(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	keys := captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo", AttemptedFrom: "aaa"})

	if d.checkUpgradeRestart(time.Now()) || d.checkUpgradeRestart(time.Now()) {
		t.Fatal("restart that already had no effect must not repeat")
	}
	if len(*keys) != 1 || (*keys)[0] != "daemon:restart-pending-no-effect" {
		t.Fatalf("escalations = %v, want exactly one daemon:restart-pending-no-effect", *keys)
	}
}

// A restart that moved the daemon forward, but not yet to the marker, may
// restart again: the attempted_from guard only stops a restart that changed
// nothing.
func TestUpgradeAdvancedPastAttemptMayRestartAgain(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	keys := captureEscalations(d)
	withOwnCommit(d, "bbb")
	fakeHistory(t, d, "aaa", "bbb", "ccc")
	writeMarker(t, d, restartPendingMarker{Commit: "ccc", Repo: "/repo", AttemptedFrom: "aaa"})

	if !d.checkUpgradeRestart(time.Now()) {
		t.Fatal("daemon that advanced past attempted_from should restart for the newer marker")
	}
	if len(*keys) != 0 {
		t.Fatalf("unexpected escalations %v", *keys)
	}
}

// Ancestry that git cannot answer is not "covered": the daemon restarts
// rather than silently clearing the marker, and the attempted_from guard
// stops it from looping when it comes back.
func TestUpgradeUnknownAncestryIsNotCovered(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	keys := captureEscalations(d)
	withOwnCommit(d, "abc1234")
	fakeHistory(t, d) // every lookup reports "unknown"
	writeMarker(t, d, restartPendingMarker{Commit: "abc1234def5678"})

	if !d.checkUpgradeRestart(time.Now()) {
		t.Fatal("unknown ancestry must not count as covered; idle daemon should restart")
	}
	// The commit writes attempted_from; that is what the next daemon reads.
	if err := d.exitForUpgradeIfRequested(&State{Running: true}); !errors.Is(err, ErrRestartForUpgrade) {
		t.Fatalf("commit err = %v, want ErrRestartForUpgrade", err)
	}
	if !markerExists(t, d) {
		t.Fatal("marker must not be cleared when ancestry is unknown")
	}
	if len(readReceipts(t, d)) != 0 {
		t.Fatal("no daemon_restarted receipt without a proven cover")
	}

	// The next daemon (same fake binary) sees attempted_from and cannot prove
	// it advanced: escalate once, never exit again.
	d2 := &Daemon{config: d.config, logger: d.logger, openGitFn: d.openGitFn, buildCommitFn: d.buildCommitFn, seams: d.seams}
	if d2.checkUpgradeRestart(time.Now()) || d2.checkUpgradeRestart(time.Now()) {
		t.Fatal("unprovable progress after an attempted restart must not loop")
	}
	if len(*keys) != 1 || (*keys)[0] != "daemon:restart-pending-no-effect" {
		t.Fatalf("escalations = %v, want exactly one daemon:restart-pending-no-effect", *keys)
	}
}

func TestUpgradeUnknownOwnCommitIgnoresMarker(t *testing.T) {
	t.Parallel()
	for _, own := range []string{"", "unknown"} {
		t.Run("own="+own, func(t *testing.T) {
			d := upgradeTestDaemon(t)
			keys := captureEscalations(d)
			withOwnCommit(d, own)
			fakeHistory(t, d, "aaa", "bbb")
			writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
			if d.checkUpgradeRestart(time.Now()) {
				t.Fatal("daemon that cannot name its own commit must not restart")
			}
			if !markerExists(t, d) || len(readReceipts(t, d)) != 0 || len(*keys) != 0 {
				t.Fatal("marker must be left alone when own commit is absent")
			}
		})
	}
}

// C7: at startup a covered marker clears, but the daemon never exits.
func TestUpgradeStartupClearsCoveredOnlyNeverRestarts(t *testing.T) {
	t.Parallel()
	t.Run("covered clears", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "bbb")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
		d.clearCoveredRestartMarker(time.Now())
		if markerExists(t, d) || len(readReceipts(t, d)) != 1 {
			t.Fatal("startup must clear a covered marker with a receipt")
		}
	})
	t.Run("newer and idle does not restart", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		keys := captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
		d.clearCoveredRestartMarker(time.Now())
		if d.upgradeRestartRequested.Load() {
			t.Fatal("startup must never request an upgrade restart")
		}
		m, _ := readRestartMarker(d.config.TownRoot)
		if m == nil || m.AttemptedFrom != "" || len(*keys) != 0 {
			t.Fatalf("startup must leave a newer marker untouched; got %+v, escalations %v", m, *keys)
		}
	})
}

func TestUpgradeShutdownLeavesDoltRunning(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		upgrade  bool
		wantStop int
	}{
		{"upgrade restart keeps Dolt", true, 0},
		{"ordinary shutdown stops Dolt", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stops := 0
			d := upgradeTestDaemon(t)
			d.doltServer = &DoltServerManager{
				config:   &DoltServerConfig{Enabled: true},
				townRoot: d.config.TownRoot,
				logger:   func(string, ...interface{}) {},
				stopFn:   func() { stops++ },
			}
			d.upgradeRestartRequested.Store(tc.upgrade)
			_ = d.shutdown(&State{Running: true})
			if stops != tc.wantStop {
				t.Fatalf("Dolt stop calls = %d, want %d", stops, tc.wantStop)
			}
		})
	}
}

func TestHeartbeatSkipsWorkWhenRestartRequested(t *testing.T) {
	t.Parallel()
	calls := 0

	d := upgradeTestDaemon(t)
	captureEscalations(d)
	d.seams.heartbeatWork = func(*Daemon, *State, bool) { calls++ }
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	d.heartbeat(&State{})
	if calls != 1 {
		t.Fatalf("no marker: heartbeatWork calls = %d, want 1", calls)
	}
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
	d.heartbeat(&State{})
	if calls != 1 {
		t.Fatalf("restart requested: heartbeatWork (plugin dispatch) ran anyway; calls = %d", calls)
	}
	if !d.upgradeRestartRequested.Load() {
		t.Fatal("heartbeat did not request the restart")
	}
}

// The ancestry check against a repository: a proven yes, a proven no, and
// "unknown" for a missing commit or repo (never a false "covered").
func TestIsAncestorAnswersFromTheRepo(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	fakeHistory(t, d, "a", "b")

	for _, tc := range []struct {
		name, repo, anc, desc string
		ok, known             bool
	}{
		{"ancestor", upgradeRepo, "a", "b", true, true},
		{"equal", upgradeRepo, "b", "b", true, true},
		{"not ancestor", upgradeRepo, "b", "a", false, true},
		{"missing commit", upgradeRepo, "0123456789abcdef0123456789abcdef01234567", "b", false, false},
		{"no repo", "", "a", "b", false, false},
		{"bad repo", "/repo/nope", "a", "b", false, false},
		{"no ancestor", upgradeRepo, "", "b", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if ok, known := d.isAncestor(tc.repo, tc.anc, tc.desc); ok != tc.ok || known != tc.known {
				t.Fatalf("isAncestor = (%v, %v), want (%v, %v)", ok, known, tc.ok, tc.known)
			}
		})
	}
}

// mergedAt is the commit's committer time as UTC RFC3339, the shell
// writer's format, and "" when git cannot answer.
func TestMergedAtIsTheCommitterTimeInUTC(t *testing.T) {
	t.Parallel()
	d := &Daemon{}
	fakeHistory(t, d, "a")
	got := d.mergedAt(upgradeRepo, "a")
	if _, err := time.Parse(time.RFC3339, got); err != nil || !strings.HasSuffix(got, "Z") {
		t.Fatalf("mergedAt = %q, want UTC RFC3339", got)
	}
	for _, tc := range []struct{ repo, commit string }{
		{upgradeRepo, "0123456789abcdef0123456789abcdef01234567"},
		{"", "a"},
		{"/repo/nope", "a"},
	} {
		if got := d.mergedAt(tc.repo, tc.commit); got != "" {
			t.Errorf("mergedAt(%q, %q) = %q, want empty", tc.repo, tc.commit, got)
		}
	}
}

// resolveOwnCommit widens the short build commit through the town's gastown
// checkout, and keeps it as built when the checkout cannot resolve it.
func TestResolveOwnCommitWidensThroughTheGastownCheckout(t *testing.T) {
	t.Parallel()
	d := &Daemon{config: &Config{TownRoot: t.TempDir()}}
	f := useGitfake(t, d)
	own := strings.Repeat("ab", 20)
	d.buildCommitFn = func() string { return own[:7] }
	if got := d.resolveOwnCommit(); got != own[:7] {
		t.Fatalf("resolveOwnCommit with no checkout = %q, want the build commit %q", got, own[:7])
	}
	checkout := filepath.Join(d.config.TownRoot, "gastown", "mayor", "rig")
	f.InitBare(t, checkout)
	id := f.Commit(t, checkout, "main", "build", map[string]string{"a": "a"})
	d.buildCommitFn = func() string { return "main" }
	if got := d.resolveOwnCommit(); got != id {
		t.Fatalf("resolveOwnCommit = %q, want the checkout's full id %s", got, id)
	}
}

// The run loop's exit after a heartbeat (both the initial heartbeat and the
// ticker): no request, no shutdown; a request runs shutdown without stopping
// Dolt and yields ErrRestartForUpgrade for Run to return.
func TestExitForUpgradeIfRequested(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		requested    bool
		wantErr      bool
		wantShutdown bool
	}{
		{"not requested", false, false, false},
		{"requested", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stops := 0
			d := upgradeTestDaemon(t)
			d.doltServer = &DoltServerManager{
				config:   &DoltServerConfig{Enabled: true},
				townRoot: d.config.TownRoot,
				logger:   func(string, ...interface{}) {},
				stopFn:   func() { stops++ },
			}
			d.upgradeRestartRequested.Store(tc.requested)
			if tc.requested {
				// The commit re-reads the marker: a request with nothing to
				// restart onto is dropped rather than shut down on.
				withOwnCommit(d, "aaa")
				fakeHistory(t, d, "aaa", "bbb")
				writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})
			}
			state := &State{Running: true}

			err := d.exitForUpgradeIfRequested(state)

			if got := errors.Is(err, ErrRestartForUpgrade); got != tc.wantErr || (!tc.wantErr && err != nil) {
				t.Fatalf("err = %v, want ErrRestartForUpgrade=%v", err, tc.wantErr)
			}
			// shutdown marks the state stopped; it is the observable proof it ran.
			if ranShutdown := !state.Running; ranShutdown != tc.wantShutdown {
				t.Fatalf("shutdown ran = %v, want %v", ranShutdown, tc.wantShutdown)
			}
			// Unrequested: no shutdown at all (an ordinary shutdown would stop
			// Dolt once). Requested: shutdown ran but left Dolt running.
			if stops != 0 {
				t.Fatalf("Dolt stop calls = %d, want 0", stops)
			}
		})
	}
}

// gt-nxvpe: a pending restart drains the landing workers and fires at the
// first moment no pass is in flight.
func TestUpgradeDrainsLandingPassesThenRestarts(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")

	// No marker: passes continue back to back.
	if d.upgradeRestartPending.Load() {
		t.Fatal("drain on with no marker")
	}
	d.checkUpgradeRestart(time.Now())
	if d.upgradeRestartPending.Load() {
		t.Fatal("drain on with no marker")
	}

	d.landingPasses.Add(1)
	d.landingStates.setBead("gastown", "gt-x", time.Now())
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
	now := time.Now()
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a landing pass")
	}
	if !d.upgradeRestartPending.Load() {
		t.Fatal("pending restart must drain the landing workers")
	}
	if d.checkUpgradeRestart(now.Add(3 * time.Minute)) {
		t.Fatal("restarted under a landing pass")
	}
	// gt-rtbbr: the wait line is one per heartbeat that finds the daemon not
	// idle (two checks above), while the drain line is once per marker.
	if got := strings.Count(logs.String(), "waiting for landing pass gt-x"); got != 2 {
		t.Fatalf("wait line logged %d times, want once per heartbeat:\n%s", got, logs.String())
	}
	if got := strings.Count(logs.String(), "upgrade-restart: draining: no new landing pass until restart"); got != 1 {
		t.Fatalf("draining line logged %d times:\n%s", got, logs.String())
	}

	// The pass ends: restart at once.
	d.landingStates.endPass("gastown")
	d.landingPasses.Add(-1)
	if !d.checkUpgradeRestart(now.Add(4 * time.Minute)) {
		t.Fatal("must restart once no pass is in flight")
	}
}

// gt-3cbee: a landing pass that begins after the idle read lost a race with
// the restart decision — the worker checks the drain at the top of its loop and
// only then registers the pass. The pass must not be killed by that restart.
func TestUpgradeRestartDefersForALandingStartedAfterTheIdleRead(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	if !d.checkUpgradeRestart(time.Now()) {
		t.Fatal("an idle daemon with a pending marker must request the restart")
	}

	// The pass starts in the gap between the idle read and the restart.
	d.landingPasses.Add(1)
	d.landingStates.setBead("gastown", "gt-x", time.Now())

	state := &State{Running: true}
	if err := d.exitForUpgradeIfRequested(state); err != nil {
		t.Fatalf("err = %v, want nil: the restart must wait for the pass", err)
	}
	if !state.Running {
		t.Fatal("the daemon shut down on top of a landing pass that started after the idle read")
	}
	if d.upgradeRestartRequested.Load() {
		t.Fatal("the request must be dropped so the next heartbeat re-reads the pass")
	}
	if got := strings.Count(logs.String(), "waiting for landing pass gt-x"); got != 1 {
		t.Fatalf("deferral logged %d times, want once:\n%s", got, logs.String())
	}

	// The pass ends: the next heartbeat restarts.
	d.landingStates.endPass("gastown")
	d.landingPasses.Add(-1)
	if !d.checkUpgradeRestart(time.Now().Add(time.Minute)) {
		t.Fatal("the restart must go ahead once the pass ends")
	}
	if err := d.exitForUpgradeIfRequested(state); !errors.Is(err, ErrRestartForUpgrade) {
		t.Fatalf("err = %v, want ErrRestartForUpgrade", err)
	}
	if state.Running {
		t.Fatal("the restart did not shut the daemon down")
	}
}

// gt-3cbee: with no pass in flight the requested restart commits at once.
func TestUpgradeRestartCommitsWithNoLandingInFlight(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	if !d.checkUpgradeRestart(time.Now()) {
		t.Fatal("an idle daemon with a pending marker must request the restart")
	}
	state := &State{Running: true}
	if err := d.exitForUpgradeIfRequested(state); !errors.Is(err, ErrRestartForUpgrade) {
		t.Fatalf("err = %v, want ErrRestartForUpgrade", err)
	}
	if state.Running {
		t.Fatal("the daemon did not shut down for the restart")
	}
	if !strings.Contains(logs.String(), "Restarting for upgrade") {
		t.Fatalf("the restart was not logged:\n%s", logs.String())
	}
}

// gt-fzwcd: the wake a landing worker sends when its drained pass ends
// restarts the daemon right then, instead of waiting out a heartbeat.
func TestUpgradeRestartsOnTheDrainedLandingWake(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
	// The heartbeat drained the workers while the pass was in flight.
	d.upgradeRestartPending.Store(true)

	state := &State{Running: true}
	if err := d.restartOnDrainedLanding(state); !errors.Is(err, ErrRestartForUpgrade) {
		t.Fatalf("err = %v, want ErrRestartForUpgrade", err)
	}
	if state.Running {
		t.Fatal("the drained-pass wake shut nothing down")
	}
}

func TestUpgradeDrainEndsWhenMarkerIsGone(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	d.landingPasses.Add(1)
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
	d.checkUpgradeRestart(time.Now())
	if err := os.Remove(restartMarkerPath(d.config.TownRoot)); err != nil {
		t.Fatal(err)
	}
	d.checkUpgradeRestart(time.Now())
	if d.upgradeRestartPending.Load() {
		t.Fatal("drain must end with the marker")
	}
}

// TestPostLandRestartCapStaysShort pins gt-8p8h7: the drain holds every landing
// while it waits for post-land, so the cap must stay a couple of minutes.
func TestPostLandRestartCapStaysShort(t *testing.T) {
	t.Parallel()
	if postLandRestartCap > 3*time.Minute {
		t.Fatalf("postLandRestartCap = %s; a pending upgrade restart holds the landing queue that long (gt-8p8h7)", postLandRestartCap)
	}
}
