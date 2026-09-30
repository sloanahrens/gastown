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

func captureEscalations(t *testing.T) *[]string {
	t.Helper()
	var keys []string
	orig := upgradeEscalateFn
	upgradeEscalateFn = func(d *Daemon, key, msg string) { keys = append(keys, key) }
	t.Cleanup(func() { upgradeEscalateFn = orig })
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
	for _, tc := range []struct{ name, marker string }{
		{"equal", "bbb"},
		{"ancestor", "aaa"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captureEscalations(t)
			d := upgradeTestDaemon(t)
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

// The receipt's duration_s is an integer, matching the shell writer.
func TestUpgradeReceiptDurationIsInteger(t *testing.T) {
	captureEscalations(t)
	d := upgradeTestDaemon(t)
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
	keys := captureEscalations(t)
	d := upgradeTestDaemon(t)
	withOwnCommit(d, "aaa")
	fakeHistory(t, d, "aaa", "bbb")
	d.mayorDispatchRunning.Store(true)
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
	d.mayorDispatchRunning.Store(false)
	if !d.checkUpgradeRestart(now.Add(41 * time.Minute)) {
		t.Fatal("daemon that became idle must restart on the next check")
	}
}

func TestUpgradeNewerMarkerIdleRequestsRestartAndStampsAttempt(t *testing.T) {
	captureEscalations(t)
	d := upgradeTestDaemon(t)
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
	m, err := readRestartMarker(d.config.TownRoot)
	if err != nil || m == nil || m.AttemptedFrom != "aaa" {
		t.Fatalf("marker after request = %+v (err %v), want attempted_from=aaa", m, err)
	}
	data, _ := os.ReadFile(restartMarkerPath(d.config.TownRoot))
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil || raw["future_field"] != float64(42) {
		t.Fatalf("unknown field lost on rewrite: %s", data)
	}
}

func TestUpgradeNoEffectRestartDoesNotLoop(t *testing.T) {
	keys := captureEscalations(t)
	d := upgradeTestDaemon(t)
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
	keys := captureEscalations(t)
	d := upgradeTestDaemon(t)
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
	keys := captureEscalations(t)
	d := upgradeTestDaemon(t)
	withOwnCommit(d, "abc1234")
	fakeHistory(t, d) // every lookup reports "unknown"
	writeMarker(t, d, restartPendingMarker{Commit: "abc1234def5678"})

	if !d.checkUpgradeRestart(time.Now()) {
		t.Fatal("unknown ancestry must not count as covered; idle daemon should restart")
	}
	if !markerExists(t, d) {
		t.Fatal("marker must not be cleared when ancestry is unknown")
	}
	if len(readReceipts(t, d)) != 0 {
		t.Fatal("no daemon_restarted receipt without a proven cover")
	}

	// The next daemon (same fake binary) sees attempted_from and cannot prove
	// it advanced: escalate once, never exit again.
	d2 := &Daemon{config: d.config, logger: d.logger, openGitFn: d.openGitFn, buildCommitFn: d.buildCommitFn}
	if d2.checkUpgradeRestart(time.Now()) || d2.checkUpgradeRestart(time.Now()) {
		t.Fatal("unprovable progress after an attempted restart must not loop")
	}
	if len(*keys) != 1 || (*keys)[0] != "daemon:restart-pending-no-effect" {
		t.Fatalf("escalations = %v, want exactly one daemon:restart-pending-no-effect", *keys)
	}
}

func TestUpgradeUnknownOwnCommitIgnoresMarker(t *testing.T) {
	for _, own := range []string{"", "unknown"} {
		t.Run("own="+own, func(t *testing.T) {
			keys := captureEscalations(t)
			d := upgradeTestDaemon(t)
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
	t.Run("covered clears", func(t *testing.T) {
		captureEscalations(t)
		d := upgradeTestDaemon(t)
		withOwnCommit(d, "bbb")
		fakeHistory(t, d, "aaa", "bbb")
		writeMarker(t, d, restartPendingMarker{Commit: "bbb", Repo: "/repo"})
		d.clearCoveredRestartMarker(time.Now())
		if markerExists(t, d) || len(readReceipts(t, d)) != 1 {
			t.Fatal("startup must clear a covered marker with a receipt")
		}
	})
	t.Run("newer and idle does not restart", func(t *testing.T) {
		keys := captureEscalations(t)
		d := upgradeTestDaemon(t)
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
	captureEscalations(t)
	calls := 0
	orig := heartbeatWorkFn
	heartbeatWorkFn = func(d *Daemon, s *State) { calls++ }
	t.Cleanup(func() { heartbeatWorkFn = orig })

	d := upgradeTestDaemon(t)
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
