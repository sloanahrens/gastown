package daemon

import (
	"bufio"
	"context"
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

	// gt-zvqn8: the stale bound can let a sweep start while the restart has
	// already waited out the cap (that is the point: the sweep must not starve).
	// Its hold runs from its own start, so the restart waits for it rather than
	// killing it on the heartbeat that follows.
	t.Run("a sweep that starts late in the wait gets its own bound", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})

		// A cycle from before the marker is what starts the wait.
		d.tierSweepRunning.Store(true)
		now := time.Now()
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running tier sweep")
		}
		// The marker has waited well past the cap when a stale sweep starts.
		started := now.Add(3 * tierSweepRestartCap)
		d.tierSweepStartedAt.Store(started.UnixNano())
		d.tierSweepRunning.Store(true)
		if d.checkUpgradeRestart(started) {
			t.Fatal("a sweep that just started was cut on its first heartbeat")
		}
		if d.checkUpgradeRestart(started.Add(tierSweepRestartCap - time.Minute)) {
			t.Fatal("the sweep was cut before its own bound, on the marker's clock")
		}
		if !strings.Contains(logs.String(), "waiting for the tier sweep that is running") {
			t.Fatalf("no wait line for the late sweep:\n%s", logs.String())
		}

		// The cycle closes before its bound, and the restart goes.
		d.tierSweepRunning.Store(false)
		if !d.checkUpgradeRestart(started.Add(tierSweepRestartCap)) {
			t.Fatal("a closed tier sweep must not hold the restart")
		}
	})
}

// gt-du6xl: a running tier sweep that is the only hold does not drain the
// landing workers, so landings keep passing for the sweep's length. The drain
// starts when the sweep closes or hits its cap, and a hold by a landing pass or
// a post-land run drains as it always has.
func TestUpgradeSweepAloneDoesNotDrainLandings(t *testing.T) {
	t.Parallel()

	t.Run("a sweep from before the marker holds without draining", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		// The sweep began before the marker's wait clock starts at now.
		d.tierSweepStartedAt.Store(now.Add(-time.Minute).UnixNano())
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		if d.upgradeRestartDrains() {
			t.Fatal("landing workers were drained while only the tier sweep held the restart")
		}
		if got := strings.Count(logs.String(), "landing passes continue while"); got != 1 {
			t.Fatalf("deferral line logged %d times, want once per hold:\n%s", got, logs.String())
		}
		if got := strings.Count(logs.String(), "waiting for the tier sweep that is running"); got != 2 {
			t.Fatalf("sweep wait line logged %d times, want once per heartbeat:\n%s", got, logs.String())
		}
		if strings.Contains(logs.String(), "draining: no new landing pass") {
			t.Fatalf("drain line logged while the drain was deferred:\n%s", logs.String())
		}

		// The sweep closes: the restart goes, and the drain was on for it.
		d.tierSweepRunning.Store(false)
		if !d.checkUpgradeRestart(now.Add(2 * time.Minute)) {
			t.Fatal("a closed tier sweep must not hold the restart")
		}
		if !strings.Contains(logs.String(), "the tier sweep closed; restarting") {
			t.Fatalf("no release line for the closed sweep:\n%s", logs.String())
		}
	})

	// gt-9vjw8: a dispatch tick, a scan or a paid pass is in flight at most
	// heartbeats, so counting it as a hold defeated the deferral in practice.
	// Soft work neither needs nor keeps the drain.
	t.Run("soft daemon work in flight does not stop the deferral", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		d.specDispatchRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		d.tierSweepStartedAt.Store(now.Add(-time.Minute).UnixNano())
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		if d.upgradeRestartDrains() {
			t.Fatal("a soft dispatch tick drained the landing workers beside the sweep")
		}
		if got := strings.Count(logs.String(), "landing passes continue while"); got != 1 {
			t.Fatalf("deferral line logged %d times, want once per hold:\n%s", got, logs.String())
		}
		if strings.Contains(logs.String(), "draining: no new landing pass") {
			t.Fatalf("drain line logged with only soft work and the sweep in flight:\n%s", logs.String())
		}

		// The sweep's cap still bounds the wait with soft work in flight: at
		// tierSweepRestartCap the sweep is cut short and the restart goes.
		if !d.checkUpgradeRestart(now.Add(tierSweepRestartCap)) {
			t.Fatal("soft work in flight must not hold the restart past the sweep's cap")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("the drain did not start at the cap with soft work in flight")
		}
		if !strings.Contains(logs.String(), "cutting the sweep short") {
			t.Fatalf("no cut-short line at the cap:\n%s", logs.String())
		}
	})

	// gt-9vjw8: the drain end rests with one line naming what it cost and what
	// it was waiting for, so the restart's log no longer needs the whole wait.
	t.Run("a drain that ends names how long landings were held", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		d.tierSweepStartedAt.Store(now.Add(-time.Minute).UnixNano())
		// A landing pass holds the restart first, so the drain goes on.
		d.landingPasses.Add(1)
		d.landingStates.setBead("gastown", "gt-x", now)
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a landing pass")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("a landing pass hold must drain the landing workers")
		}

		// Three minutes later the pass is over and only the sweep holds: the
		// drain ends with the time it held landings and the last hold it named.
		d.landingStates.endPass("gastown")
		d.landingPasses.Add(-1)
		if d.checkUpgradeRestart(now.Add(3 * time.Minute)) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		if d.upgradeRestartDrains() {
			t.Fatal("the drain stayed on with only the sweep holding")
		}
		want := "upgrade-restart: the drain held landing passes for 3m0s, last waiting for landing pass gt-x"
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("drain end line missing or wrong, want %q:\n%s", want, logs.String())
		}
	})

	t.Run("the restart names how long the drain held landings", func(t *testing.T) {
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
		d.landingStates.endPass("gastown")
		d.landingPasses.Add(-1)
		if !d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("must restart once no pass is in flight")
		}
		state := &State{Running: true}
		if err := d.exitForUpgradeIfRequested(state, now.Add(time.Minute)); !errors.Is(err, ErrRestartForUpgrade) {
			t.Fatalf("err = %v, want ErrRestartForUpgrade", err)
		}
		restart := strings.Index(logs.String(), "upgrade-restart: idle; restarting")
		held := strings.Index(logs.String(), "upgrade-restart: the drain held landing passes for ")
		if held < 0 {
			t.Fatalf("no drain end line before the restart:\n%s", logs.String())
		}
		if restart < 0 || held > restart {
			t.Fatalf("the drain end line must precede the restart line:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "the drain held landing passes for 1m0s") {
			t.Fatalf("the drain end line does not measure the wait on the decision's clock:\n%s", logs.String())
		}
		if !strings.Contains(logs.String(), "last waiting for landing pass gt-x") {
			t.Fatalf("the drain end line does not name the hold:\n%s", logs.String())
		}
	})

	t.Run("the drain starts at the cap while the sweep still runs", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		// The sweep began before the marker's wait clock starts at now.
		d.tierSweepStartedAt.Store(now.Add(-time.Minute).UnixNano())
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		if d.upgradeRestartDrains() {
			t.Fatal("landing workers were drained before the cap while only the tier sweep held the restart")
		}
		if !d.checkUpgradeRestart(now.Add(tierSweepRestartCap)) {
			t.Fatal("a sweep still running at the cap must not hold the restart further")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("the drain did not start at the cap while the sweep still ran")
		}
		if !strings.Contains(logs.String(), "cutting the sweep short") {
			t.Fatalf("no cut-short line at the cap:\n%s", logs.String())
		}
	})

	t.Run("a landing pass that starts during the sweep drains", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		// The sweep began before the marker's wait clock starts at now.
		d.tierSweepStartedAt.Store(now.Add(-time.Minute).UnixNano())
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		// A landing pass starts while the sweep runs, as the deferral allows.
		d.landingPasses.Add(1)
		d.landingStates.setBead("gastown", "gt-x", now)
		if d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("restarted under a landing pass")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("a landing pass hold must drain the landing workers")
		}
		if got := strings.Count(logs.String(), "upgrade-restart: draining: no new landing pass until restart"); got != 1 {
			t.Fatalf("drain start line logged %d times, want once:\n%s", got, logs.String())
		}
		if !strings.Contains(logs.String(), "waiting for landing pass gt-x") {
			t.Fatalf("landing pass not named:\n%s", logs.String())
		}
	})

	t.Run("a sweep that began after the marker keeps the drain", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		// A cycle from before the marker starts the wait, then closes.
		d.tierSweepRunning.Store(true)
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running tier sweep before the cap")
		}
		d.tierSweepRunning.Store(false)

		// A new cycle starts after the marker: it is not the one the wait
		// deferred for, so the drain goes on.
		later := now.Add(time.Minute)
		d.tierSweepStartedAt.Store(later.UnixNano())
		d.tierSweepRunning.Store(true)
		if d.checkUpgradeRestart(later) {
			t.Fatal("restarted under a sweep that began after the marker")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("a sweep that began after the marker must drain the landing workers")
		}
	})

	t.Run("a post-land run still drains beside a sweep", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		d.postLandRuns.Add(1)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		if d.checkUpgradeRestart(time.Now()) {
			t.Fatal("restarted under a post-land run inside its cap")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("a post-land run hold must drain the landing workers")
		}
	})
}

// gt-u1fzb: a spec dispatch tick in flight is soft work with no landing pass to
// protect, so a tick that began before the marker holds the restart without
// draining landings for its whole bound, as a tier sweep does (gt-du6xl). A tick
// that began after the marker keeps the drain, as a late sweep does.
func TestUpgradeDispatchTickAloneDoesNotDrainLandings(t *testing.T) {
	t.Parallel()

	t.Run("a tick from before the marker holds without draining", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		// The tick began before the marker's wait clock starts at now.
		tick := &specDispatchTickRun{started: now.Add(-time.Minute)}
		d.specDispatchRunning.Store(true)
		d.specDispatchTick.Store(tick)
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("restarted under a running spec dispatch tick before its bound")
		}
		if d.upgradeRestartDrains() {
			t.Fatal("landing workers were drained while only the dispatch tick held the restart")
		}
		if !d.upgradeTickWaiting.Load() {
			t.Fatal("the restart did not report waiting on the tick in flight")
		}
		if got := strings.Count(logs.String(), "landing passes continue while a spec dispatch tick holds the restart"); got != 1 {
			t.Fatalf("deferral line logged %d times, want once per hold:\n%s", got, logs.String())
		}
		if got := strings.Count(logs.String(), "waiting for a spec dispatch tick"); got != 2 {
			t.Fatalf("tick wait line logged %d times, want once per heartbeat:\n%s", got, logs.String())
		}
		if strings.Contains(logs.String(), "tier sweep") {
			t.Fatalf("a wait on the dispatch tick named the tier sweep:\n%s", logs.String())
		}
		if strings.Contains(logs.String(), "draining: no new landing pass") {
			t.Fatalf("drain line logged while the drain was deferred:\n%s", logs.String())
		}

		// The drain is off, so the restart's landing hold cannot keep the
		// ticker from starting a tick behind the one it waits on: the tick wait
		// does.
		d.logger = log.New(io.Discard, "", 0)
		if d.triggerSpecDispatch() {
			t.Fatal("a tick started behind the one the restart waits on")
		}
		if got := d.specDispatchTick.Load(); got != tick {
			t.Fatalf("the ticker replaced the running tick's start: %+v", got)
		}

		// The tick closes: the restart goes, and the drain was on for it.
		d.specDispatchRunning.Store(false)
		d.specDispatchTick.Store(nil)
		if !d.checkUpgradeRestart(now.Add(2 * time.Minute)) {
			t.Fatal("a closed spec dispatch tick must not hold the restart")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("the restart did not drain the landing workers for its own moment")
		}
	})

	t.Run("a tick that began after the marker keeps the drain", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		// A tick from before the marker starts the wait, then closes.
		d.specDispatchRunning.Store(true)
		d.specDispatchTick.Store(&specDispatchTickRun{started: now.Add(-time.Minute)})
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running spec dispatch tick before its bound")
		}
		d.specDispatchRunning.Store(false)
		d.specDispatchTick.Store(nil)

		// A new tick starts after the marker: it is not the one the wait deferred
		// for, so the drain goes on.
		later := now.Add(time.Minute)
		d.specDispatchRunning.Store(true)
		d.specDispatchTick.Store(&specDispatchTickRun{started: later})
		if d.checkUpgradeRestart(later) {
			t.Fatal("restarted under a tick that began after the marker")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("a tick that began after the marker must drain the landing workers")
		}
		if d.upgradeTickWaiting.Load() {
			t.Fatal("the restart waits on a tick with the drain on, where the drain is what parks the ticker")
		}
	})

	t.Run("the restart goes past the tick's bound with the drain off", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		// The bound the tick recorded is the one the restart enforces, and the
		// deferral line must name that bound, not a second read of the config.
		bound := 7 * time.Minute
		d.specDispatchRunning.Store(true)
		d.specDispatchTick.Store(&specDispatchTickRun{started: now.Add(-time.Minute), bound: bound})
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running spec dispatch tick before its bound")
		}
		if d.upgradeRestartDrains() {
			t.Fatal("landing workers were drained while only the dispatch tick held the restart")
		}
		if !strings.Contains(logs.String(), "after its 7m0s bound") {
			t.Fatalf("the deferral line does not name the bound the restart enforces:\n%s", logs.String())
		}
		if !d.checkUpgradeRestart(now.Add(bound)) {
			t.Fatal("a tick still running at its bound must not hold the restart further")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("the drain did not start at the tick's bound")
		}
		if !strings.Contains(logs.String(), "a spec dispatch tick is still in flight after its") {
			t.Fatalf("no cut-short line at the tick's bound:\n%s", logs.String())
		}
	})

	// gt-u1fzb: the tick wait parks the ticker, so every way the wait ends must
	// clear it: the drain taking the ticker over, and the marker going away.
	t.Run("the tick wait ends with the wait it parks the ticker for", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		d.logger = log.New(io.Discard, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		d.specDispatchRunning.Store(true)
		d.specDispatchTick.Store(&specDispatchTickRun{started: now.Add(-time.Minute)})
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a tick from before the marker")
		}
		if !d.upgradeTickWaiting.Load() {
			t.Fatal("the restart did not park the ticker on the tick in flight")
		}

		// A landing pass starts while the drain is off: the drain takes the
		// ticker over, and the tick wait does not outlive it.
		d.landingPasses.Add(1)
		d.landingStates.setBead("gastown", "gt-x", now)
		if d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("restarted under a landing pass")
		}
		if !d.upgradeRestartDrains() {
			t.Fatal("a landing pass hold must drain the landing workers")
		}
		if d.upgradeTickWaiting.Load() {
			t.Fatal("the tick wait outlived the drain taking the ticker over")
		}

		// The marker goes away with the tick still in flight: both notices end.
		d.landingPasses.Store(0)
		d.landingStates.endPass("gastown")
		if err := os.Remove(restartMarkerPath(d.config.TownRoot)); err != nil {
			t.Fatal(err)
		}
		if d.checkUpgradeRestart(now.Add(2 * time.Minute)) {
			t.Fatal("restarted with no marker")
		}
		if d.specDispatchParked() {
			t.Fatal("the ticker stayed parked after the wait ended")
		}
	})
}

// gt-u1fzb: with the drain off the deferral clears the restart's landing hold,
// the flag that parks the ticker. The tick wait parks it in its place, so no
// tick starts behind the one the restart waits on and extends the wait.
func TestUpgradeTickWaitParksTheTicker(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.upgradeTickWaiting.Store(true)
	if !d.specDispatchParked() {
		t.Fatal("the tick wait did not park the ticker")
	}
	if d.triggerSpecDispatch() {
		t.Fatal("a tick started while the restart waited on the tick in flight")
	}
	if d.specDispatchRunning.Load() {
		t.Fatal("a deferred tick left its single-flight guard set")
	}
	if d.specDispatchTick.Load() != nil {
		t.Fatal("a deferred tick published a start")
	}
	d.upgradeTickWaiting.Store(false)
	if d.specDispatchParked() {
		t.Fatal("the ticker stayed parked after the wait ended")
	}
}

// gt-oucgn: a maintenance gc cycle in flight holds a pending upgrade restart —
// a shutdown cancels the gc call's context, so the restart aborts the call with
// nothing recorded and the database stays due — but only up to the gc's own
// bound, so a stuck gc cannot hold an install forever.
func TestUpgradeWaitsForAMaintenanceGCUpToItsOwnBound(t *testing.T) {
	t.Parallel()

	t.Run("a gc in flight holds, and the wait names it", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.maintenanceGCRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		d.maintenanceGCCallStartedAt.Store(now.UnixNano())
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted over a maintenance gc call")
		}
		if d.checkUpgradeRestart(now.Add(maintenanceGCRestartCap - time.Minute)) {
			t.Fatal("the gc was cut before its own bound")
		}
		if !strings.Contains(logs.String(), "waiting for the maintenance gc cycle") {
			t.Fatalf("the gc hold was not named in the wait line:\n%s", logs.String())
		}
		if strings.Contains(logs.String(), "the maintenance gc call has run for") {
			t.Fatalf("the release line claimed a restart that kept waiting:\n%s", logs.String())
		}
	})

	t.Run("the gc finishing lets the restart go", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.maintenanceGCRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted over a maintenance gc call")
		}
		d.maintenanceGCRunning.Store(false)
		if !d.checkUpgradeRestart(now.Add(time.Minute)) {
			t.Fatal("the restart must go once the gc finishes")
		}
	})

	t.Run("a stuck gc stops holding at the bound, and the log says so", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.maintenanceGCRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		d.maintenanceGCCallStartedAt.Store(now.UnixNano())
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(maintenanceGCRestartCap-time.Minute)) {
			t.Fatal("restarted under a running gc before the bound")
		}
		if !d.checkUpgradeRestart(now.Add(maintenanceGCRestartCap)) {
			t.Fatal("a gc stuck past its own bound must not hold the restart further")
		}
		if !strings.Contains(logs.String(), "the maintenance gc call has run for") ||
			!strings.Contains(logs.String(), "restarting anyway") {
			t.Fatalf("no release line for the stuck gc:\n%s", logs.String())
		}
	})

	// The bound is the gc hold's own clock, not the marker's: a marker that has
	// already waited out a tier sweep must not cut a gc that just started. With
	// the marker's clock the check below would restart a gc that is a minute in.
	t.Run("a gc that starts late in the wait still gets its own bound", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.tierSweepRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running tier sweep")
		}
		gcStarted := now.Add(tierSweepRestartCap - time.Minute)
		d.maintenanceGCRunning.Store(true)
		d.maintenanceGCCallStartedAt.Store(gcStarted.UnixNano())
		if d.checkUpgradeRestart(gcStarted) {
			t.Fatal("restarted on the heartbeat the gc started")
		}
		if d.checkUpgradeRestart(gcStarted.Add(maintenanceGCRestartCap - time.Minute)) {
			t.Fatal("the gc was cut before its own bound although the marker had waited past it")
		}
		if !d.checkUpgradeRestart(gcStarted.Add(maintenanceGCRestartCap)) {
			t.Fatal("the gc's own bound elapsed and the restart still did not go")
		}
	})

	// gt-763ux: the bound is one dolt_gc call's, not the cycle's. The cycle sets
	// its flag before the backup phase, which can run ten minutes per database;
	// counting it cut a healthy gc a minute in.
	t.Run("a long backup phase before the gc call does not count against the bound", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		var logs strings.Builder
		d.logger = log.New(&logs, "", 0)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.maintenanceGCRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		// The cycle is in its backup phase: flag set, no call yet.
		now := time.Now()
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a cycle in its backup phase")
		}
		if d.checkUpgradeRestart(now.Add(maintenanceGCRestartCap - time.Minute)) {
			t.Fatal("the backup phase was counted against the gc bound")
		}

		// The backup ends at minute 14 and the first call starts.
		callStarted := now.Add(maintenanceGCRestartCap - time.Minute)
		d.maintenanceGCCallStartedAt.Store(callStarted.UnixNano())
		if d.checkUpgradeRestart(callStarted.Add(time.Minute)) {
			t.Fatal("a healthy gc call was cut a minute in by the time the cycle had spent on backup")
		}
		if d.checkUpgradeRestart(callStarted.Add(maintenanceGCRestartCap - time.Minute)) {
			t.Fatal("the gc call was cut before it had run for the bound")
		}
		if strings.Contains(logs.String(), "restarting anyway") {
			t.Fatalf("a release line was logged for a restart that kept waiting:\n%s", logs.String())
		}
		if !d.checkUpgradeRestart(callStarted.Add(maintenanceGCRestartCap)) {
			t.Fatal("a call that has run for the bound must not hold the restart further")
		}
	})

	t.Run("each database's call gets its own bound", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.maintenanceGCRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		d.maintenanceGCCallStartedAt.Store(now.UnixNano())
		if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(10*time.Minute)) {
			t.Fatal("restarted under a running gc call")
		}
		// The first call ends; the second database's starts ten minutes later.
		d.maintenanceGCCallStartedAt.Store(0)
		second := now.Add(10 * time.Minute)
		d.maintenanceGCCallStartedAt.Store(second.UnixNano())
		if d.checkUpgradeRestart(second.Add(maintenanceGCRestartCap - time.Minute)) {
			t.Fatal("the second call was cut at the first call's bound")
		}
	})

	// gt-763ux: a hard hold defers the restart whatever the gc is doing, so the
	// time it holds is not time the gc was waited on. The gc runs through it.
	t.Run("a hard hold in force does not advance the gc wait clock", func(t *testing.T) {
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "aaa")
		fakeHistory(t, d, "aaa", "bbb")
		d.maintenanceGCRunning.Store(true)
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

		now := time.Now()
		d.maintenanceGCCallStartedAt.Store(now.UnixNano())
		if d.checkUpgradeRestart(now) {
			t.Fatal("restarted under a running gc call")
		}

		// A landing pass holds the restart for twenty minutes, well past the cap.
		d.landingPasses.Add(1)
		d.landingStates.setBead("gastown", "gt-x", now)
		for m := 1; m <= 20; m++ {
			if d.checkUpgradeRestart(now.Add(time.Duration(m) * time.Minute)) {
				t.Fatal("restarted under a landing pass")
			}
		}
		if got, _ := d.maintenanceGCUpgradeHold(now.Add(20*time.Minute), true); got != 0 {
			t.Fatalf("the gc wait clock ran %s while a hard hold deferred the restart", got)
		}
		d.landingStates.endPass("gastown")
		d.landingPasses.Add(-1)

		// The pass ends: the gc still gets its bound, counted from here.
		end := now.Add(20 * time.Minute)
		if d.checkUpgradeRestart(end.Add(maintenanceGCRestartCap - time.Minute)) {
			t.Fatal("the time under the hard hold was counted against the gc bound")
		}
		if !d.checkUpgradeRestart(end.Add(maintenanceGCRestartCap)) {
			t.Fatal("the gc has waited its bound outside the hard hold and the restart still did not go")
		}
	})
}

// The gc hold's bound is the gc's own: one call is cut off at
// maintenanceGCTimeout and its pause marker outlives it by maintenancePauseSlack
// (gt-oucgn).
func TestMaintenanceGCRestartCapMatchesTheGCsOwnBound(t *testing.T) {
	t.Parallel()
	if want := maintenanceGCTimeout + maintenancePauseSlack; maintenanceGCRestartCap != want {
		t.Fatalf("maintenanceGCRestartCap = %s, want %s", maintenanceGCRestartCap, want)
	}
	if maintenanceGCRestartCap != 15*time.Minute {
		t.Fatalf("maintenanceGCRestartCap = %s, want 15m", maintenanceGCRestartCap)
	}
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
// housekeepingRestartCap over a landing pass, a scheduled slings
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

// gt-cbaoy: a spec dispatch tick in flight holds the restart for its own bound,
// measured from when it started, not for the 2m housekeeping cap. The 2m cap
// restarted the daemon mid-sling and left a seat hooked with no session.
func TestUpgradeWaitsForASpecDispatchTickToItsBound(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	bound := d.specDispatchTickBound()
	if bound <= housekeepingRestartCap {
		t.Fatalf("specDispatchTickBound = %s, want it past the housekeeping cap %s", bound, housekeepingRestartCap)
	}
	d.specDispatchRunning.Store(true)
	d.specDispatchTick.Store(&specDispatchTickRun{started: now})

	if d.checkUpgradeRestart(now) || d.checkUpgradeRestart(now.Add(housekeepingRestartCap)) {
		t.Fatal("restarted under a spec dispatch tick inside its bound")
	}
	if !strings.Contains(logs.String(), "waiting for a spec dispatch tick") {
		t.Fatalf("the wait did not name the spec dispatch tick:\n%s", logs.String())
	}
	if !d.checkUpgradeRestart(now.Add(bound)) {
		t.Fatal("the restart must go once the tick is past its own bound")
	}
	if !strings.Contains(logs.String(), "a spec dispatch tick is still in flight after its") {
		t.Fatalf("no cut-short line naming the spec dispatch tick:\n%s", logs.String())
	}
}

// A max_per_tick cut while a tick runs, or a patrol block that stops reading,
// must not shorten the wait for that tick: the restart waits on the bound the
// tick was started under, not on a fresh read of the config (gt-cbaoy).
func TestUpgradeWaitsForTheSpecDispatchBoundTheTickStartedWith(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.logger = log.New(io.Discard, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})
	if err := SavePatrolConfig(d.config.TownRoot, &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{MaxPerTick: 5}}}); err != nil {
		t.Fatal(err)
	}
	started := specDispatchBound(specDispatchTimeout, 5)

	now := time.Now()
	d.specDispatchRunning.Store(true)
	d.specDispatchTick.Store(&specDispatchTickRun{started: now, bound: started})
	if err := SavePatrolConfig(d.config.TownRoot, &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{MaxPerTick: 1}}}); err != nil {
		t.Fatal(err)
	}

	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a spec dispatch tick inside its bound")
	}
	if d.checkUpgradeRestart(now.Add(started - time.Minute)) {
		t.Fatal("a max_per_tick cut mid-tick shortened the wait for the running tick")
	}
	if !d.checkUpgradeRestart(now.Add(started)) {
		t.Fatal("the restart must go once the tick is past the bound it started under")
	}
}

// gt-32j1y: a tick with no recorded start falls back to the marker's wait clock
// under the config's bound, so a tick whose start went missing still holds the
// restart for its bound rather than for the housekeeping cap.
func TestUpgradeSpecDispatchWaitFallsBackToTheMarkerClock(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	var logs strings.Builder
	d.logger = log.New(&logs, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	bound := d.specDispatchTickBound()
	d.specDispatchRunning.Store(true)

	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a spec dispatch tick with no recorded start")
	}
	if !strings.Contains(logs.String(), "waiting for a spec dispatch tick") {
		t.Fatalf("the wait did not name the spec dispatch tick:\n%s", logs.String())
	}
	if d.checkUpgradeRestart(now.Add(bound - time.Minute)) {
		t.Fatal("the marker's wait ended before the tick's bound")
	}
	if !d.checkUpgradeRestart(now.Add(bound)) {
		t.Fatal("the restart must go once the marker's wait passes the tick's bound")
	}
}

// gt-nosod: a restart waits from the youngest running sling's start. Measured
// from the oldest's, it went as soon as that sling was cut at its bound, while
// a younger sling still had most of its own left.
func TestUpgradeHoldWaitsForYoungestSlingBound(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.logger = log.New(io.Discard, "", 0)

	t0 := time.Now()
	bound := specSlingBound()
	d.specSlings.begin(specSlingFlight{Bead: "gt-old", Rig: "gastown", started: t0})
	d.specSlings.begin(specSlingFlight{Bead: "gt-young", Rig: "gastown", started: t0.Add(4 * time.Minute)})

	// The older sling is at its bound and about to leave the set; the younger
	// one has most of its own bound left.
	hold := d.upgradeHold(t0.Add(bound), nil)
	if hold == nil || hold.name != specSlingHoldLabel {
		t.Fatalf("upgradeHold = %+v with a younger sling inside its bound, want the sling hold", hold)
	}
	if hold := d.upgradeHold(t0.Add(4*time.Minute+bound), nil); hold != nil && hold.name == specSlingHoldLabel {
		t.Fatalf("upgradeHold = %+v with every sling past its bound, want the restart to go", hold)
	}
}

// The wait is frozen when the drain begins, so a sling that starts afterwards
// cannot move its end: a dispatch that kept handing back slings would otherwise
// hold the restart for ever (gt-nosod).
func TestUpgradeSlingWaitIsNotExtendedByASlingStartedAfterTheDrain(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	bound := specSlingBound()
	d.specSlings.begin(specSlingFlight{Bead: "gt-a", Rig: "gastown", started: now})
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a running sling inside its bound")
	}

	d.specSlings.begin(specSlingFlight{Bead: "gt-b", Rig: "gastown", started: now.Add(bound - time.Minute)})
	if !d.checkUpgradeRestart(now.Add(bound)) {
		t.Fatal("a sling started after the drain extended the wait; want the restart at the frozen youngest's bound")
	}
}

// gt-cbaoy: a tick that ends inside its bound releases the restart at once; the
// restart does not wait out the rest of the bound.
func TestUpgradeRestartGoesWhenTheSpecDispatchTickEnds(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.logger = log.New(io.Discard, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	d.specDispatchRunning.Store(true)
	d.specDispatchTick.Store(&specDispatchTickRun{started: now})
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a spec dispatch tick inside its bound")
	}

	d.specDispatchRunning.Store(false)
	d.specDispatchTick.Store(nil)
	if !d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("the restart must go as soon as the tick ends")
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
	if err := d.exitForUpgradeIfRequested(&State{Running: true}, time.Now()); !errors.Is(err, ErrRestartForUpgrade) {
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

// gt-21cr0: a restart whose stamp fails must not leave the pending flag set.
// The flag holds spec dispatch and the landing passes, so a flag left behind by
// a failed stamp holds them until a later stamp succeeds.
func TestFailedRestartStampReleasesThePendingFlag(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})
	// A daemon directory the stamp cannot write to makes the stamp fail; the
	// marker is still readable, so the request is still made.
	daemonDir := filepath.Dir(restartMarkerPath(d.config.TownRoot))
	//testpolicy:allow no-exec-files — the target is the fixture directory: a read-only dir needs its search bit
	if err := os.Chmod(daemonDir, 0o555); err != nil {
		t.Fatal(err)
	}
	//testpolicy:allow no-exec-files — the target is the fixture directory: restores its write bit for t.TempDir cleanup
	t.Cleanup(func() { _ = os.Chmod(daemonDir, 0o755) })
	now := time.Now()

	if !d.checkUpgradeRestart(now) {
		t.Fatal("idle daemon with newer marker must request restart")
	}
	state := &State{Running: true}
	if err := d.exitForUpgradeIfRequested(state, now); err != nil || !state.Running {
		t.Fatalf("failed stamp: err = %v, shut down = %v; want no error and no shutdown", err, !state.Running)
	}
	if d.upgradeRestartDrains() {
		t.Fatal("pending flag still set after the stamp failed")
	}
	// Restored so the dispatch tick runs as it does in a daemon. The tick runs
	// against a fake CLI, as the spec dispatch tests do.
	//testpolicy:allow no-exec-files — the target is the fixture directory: traversal needs its search bit back
	if err := os.Chmod(daemonDir, 0o755); err != nil {
		t.Fatal(err)
	}
	d.ctx = context.Background()
	d.execCmd = newFakeCLI(func([]string) cliReply { return cliReply{} }).run
	d.patrolConfig = &DaemonPatrolConfig{Patrols: &PatrolsConfig{SpecDispatch: &SpecDispatchConfig{Enabled: boolPtr(true)}}}
	if !d.triggerSpecDispatch() {
		t.Fatal("spec dispatch held after the stamp failed")
	}
	// Bounded: a dispatch cycle that never ends fails this test by name.
	cycles := make(chan struct{})
	go func() {
		d.specDispatchCycles.Wait()
		close(cycles)
	}()
	waitForSignal(t, cycles, "spec dispatch cycle did not finish after the failed stamp")
}

// TestStampRestartAttemptDoesNotOverwriteANewerMarker pins gt-oyrav (D9):
// the installer replaces daemon/restart-pending.json with a newer install's
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

// clearCoveredMarker removes a marker by path after a git ancestry check.
// the installer writing a newer marker in between must survive: removing it
// would leave the daemon on the old binary with no request left to restart for.
func TestClearCoveredMarker(t *testing.T) {
	t.Parallel()
	// loaded is a daemon running bbb with a marker for aaa on disk, and the
	// marker as the heartbeat read it.
	loaded := func(t *testing.T) (*Daemon, *restartPendingMarker, string) {
		t.Helper()
		d := upgradeTestDaemon(t)
		captureEscalations(d)
		withOwnCommit(d, "bbb")
		fakeHistory(t, d, "aaa", "bbb", "ccc")
		writeMarker(t, d, restartPendingMarker{Commit: "aaa", Repo: upgradeRepo})
		m, own, ok := d.loadMarkerForUpgrade()
		if !ok {
			t.Fatal("no marker loaded")
		}
		return d, m, own
	}

	t.Run("keeps a newer marker written during the ancestry check", func(t *testing.T) {
		t.Parallel()
		d, m, own := loaded(t)

		swapped := false
		handled := d.clearCoveredMarkerWith(func() {
			swapped = true
			writeMarker(t, d, restartPendingMarker{Commit: "ccc", Repo: upgradeRepo})
		}, m, own, time.Now())

		if !swapped {
			t.Fatal("the test never swapped the marker")
		}
		if !handled {
			t.Error("the caller was told to go on with the marker it read; it was replaced")
		}
		if got, err := readRestartMarker(d.config.TownRoot); err != nil || got == nil || got.Commit != "ccc" {
			t.Fatalf("marker = %+v (err %v), want the newer marker left in place", got, err)
		}
		if rs := readReceipts(t, d); len(rs) != 0 {
			t.Errorf("receipts = %+v, want none for a marker that was not cleared", rs)
		}
	})

	t.Run("keeps a marker it cannot read back", func(t *testing.T) {
		t.Parallel()
		d, m, own := loaded(t)

		handled := d.clearCoveredMarkerWith(func() {
			if err := os.WriteFile(restartMarkerPath(d.config.TownRoot), []byte("{not json"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, m, own, time.Now())

		if !handled {
			t.Error("the caller was told to go on with a marker that could not be read back")
		}
		if !markerExists(t, d) {
			t.Error("a marker the re-read could not confirm was removed")
		}
		if rs := readReceipts(t, d); len(rs) != 0 {
			t.Errorf("receipts = %+v, want none for a marker that was not cleared", rs)
		}
	})

	t.Run("clears the marker it read, with a receipt", func(t *testing.T) {
		t.Parallel()
		d, m, own := loaded(t)

		if !d.clearCoveredMarker(m, own, time.Now()) {
			t.Fatal("covered marker not cleared")
		}
		if markerExists(t, d) {
			t.Error("covered marker still on disk")
		}
		if rs := readReceipts(t, d); len(rs) != 1 || rs[0].Commit != "aaa" {
			t.Errorf("receipts = %+v, want one for aaa", rs)
		}
	})
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
	if err := d.exitForUpgradeIfRequested(&State{Running: true}, time.Now()); !errors.Is(err, ErrRestartForUpgrade) {
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

			err := d.exitForUpgradeIfRequested(state, time.Now())

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
	if d.upgradeRestartDrains() {
		t.Fatal("drain on with no marker")
	}
	d.checkUpgradeRestart(time.Now())
	if d.upgradeRestartDrains() {
		t.Fatal("drain on with no marker")
	}

	d.landingPasses.Add(1)
	d.landingStates.setBead("gastown", "gt-x", time.Now())
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
	now := time.Now()
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a landing pass")
	}
	if !d.upgradeRestartDrains() {
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
	if err := d.exitForUpgradeIfRequested(state, time.Now()); err != nil {
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
	if err := d.exitForUpgradeIfRequested(state, time.Now().Add(time.Minute)); !errors.Is(err, ErrRestartForUpgrade) {
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
	if err := d.exitForUpgradeIfRequested(state, time.Now()); !errors.Is(err, ErrRestartForUpgrade) {
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
	d.landingHolds.hold(landingHoldUpgradeRestart)

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
	if d.upgradeRestartDrains() {
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

// gt-nosod: a drain that began while no sling was in flight froze the sling wait
// on nothing, so a sling that starts afterwards does not hold the restart. Only
// the slings a drain froze can hold it.
func TestUpgradeSlingHoldIgnoresSlingsStartedAfterAnEmptyDrain(t *testing.T) {
	t.Parallel()
	d := upgradeTestDaemon(t)
	d.logger = log.New(io.Discard, "", 0)
	captureEscalations(d)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: upgradeRepo})

	now := time.Now()
	// A dispatch tick holds the drain open with no sling in flight.
	d.specDispatchRunning.Store(true)
	d.specDispatchTick.Store(&specDispatchTickRun{started: now})
	if d.checkUpgradeRestart(now) {
		t.Fatal("restarted under a spec dispatch tick inside its bound")
	}
	d.specDispatchRunning.Store(false)
	d.specDispatchTick.Store(nil)

	// A sling begun after the drain froze nothing, and must not hold the restart.
	d.specSlings.begin(specSlingFlight{Bead: "gt-late", Rig: "gastown", started: now.Add(time.Minute)})
	if !d.checkUpgradeRestart(now.Add(time.Minute)) {
		t.Fatal("a sling begun after an empty drain held the restart; want it to go")
	}
}
