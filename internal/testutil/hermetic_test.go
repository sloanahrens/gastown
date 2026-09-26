package testutil

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/feed"
	"github.com/steveyegge/gastown/internal/krc"
	"github.com/steveyegge/gastown/internal/workspace"
)

// withSavedEnv snapshots the full process environment and restores it via
// t.Cleanup. StartHermetic mutates process-wide env and does not restore it
// itself (unlike HermeticTest), so any test calling it directly needs this.
func withSavedEnv(t *testing.T) {
	t.Helper()
	saved := os.Environ()
	t.Cleanup(func() {
		os.Clearenv()
		for _, kv := range saved {
			if name, val, ok := strings.Cut(kv, "="); ok {
				_ = os.Setenv(name, val)
			}
		}
	})
}

// makeFakeTown builds a minimal "live town" fixture: marker file, rigs.json
// with one known rig, watched subdirectories, and an events log.
func makeFakeTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(root, "mayor", "town.json"), `{"type":"town","version":2,"name":"fake"}`)
	writeFile(t, filepath.Join(root, "mayor", "rigs.json"), `{"version":1,"rigs":{"gastown":{}}}`)
	for _, sub := range watchedSubdirs {
		if err := os.MkdirAll(filepath.Join(root, sub), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFile(t, filepath.Join(root, ".events.jsonl"),
		`{"ts":"2026-09-08T00:00:00Z","source":"gt","type":"boot","actor":"mayor","visibility":"feed"}`+"\n")
	return root
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestTripwire_CleanTownReportsNoLeaks(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)
	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("clean town reported leaks: %v", leaks)
	}
}

func TestTripwire_DetectsNewFilesAndDatabases(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	// Orphan test database (the hq-det/hq-4zrq incident shape).
	if err := os.MkdirAll(filepath.Join(town, ".dolt-data", "testdb_abc123"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Stray file dropped at the town root.
	writeFile(t, filepath.Join(town, "scratch.txt"), "oops")
	// Lock churn must be ignored — legitimate concurrent agents create these.
	writeFile(t, filepath.Join(town, ".events.jsonl.lock"), "")

	leaks := snap.diff()
	if len(leaks) != 2 {
		t.Fatalf("expected 2 leaks, got %d: %v", len(leaks), leaks)
	}
	joined := strings.Join(leaks, "\n")
	if !strings.Contains(joined, ".dolt-data/testdb_abc123") {
		t.Errorf("orphan database not detected: %v", leaks)
	}
	if !strings.Contains(joined, "scratch.txt") {
		t.Errorf("stray root file not detected: %v", leaks)
	}
}

// gt-bd79: onboarding a new rig concurrently with a test run creates both a
// rig worktree at the town root and a Dolt data directory under
// .dolt-data/, named after the rig — the same shape the before/after
// snapshot cannot distinguish from test-leaked state. Once the rig is
// registered in rigs.json, both entries must be tolerated.
func TestTripwire_ToleratesConcurrentRigOnboarding(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	// Operator onboards a new rig "hm" while the test is running: rigs.json
	// gains an entry, and its worktree + Dolt data directory appear.
	writeFile(t, filepath.Join(town, "mayor", "rigs.json"),
		`{"version":1,"rigs":{"gastown":{},"hm":{}}}`)
	if err := os.MkdirAll(filepath.Join(town, "hm"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(town, ".dolt-data", "hm"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A real leak must still be caught alongside the tolerated rig entries.
	writeFile(t, filepath.Join(town, "scratch.txt"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected exactly 1 leak (real file only), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.txt") {
		t.Errorf("real leak not detected: %v", leaks)
	}
	joined := strings.Join(leaks, "\n")
	if strings.Contains(joined, "new entry: hm") {
		t.Errorf("new rig worktree flagged as leak: %v", leaks)
	}
	if strings.Contains(joined, ".dolt-data/hm") {
		t.Errorf("new rig Dolt data dir flagged as leak: %v", leaks)
	}
}

// An entry that merely shares a name with something under .dolt-data but is
// not a registered rig (e.g. an orphan test database) must still be caught —
// explainedByRig only tolerates names present in the town's current
// rigs.json.
func TestTripwire_DoesNotTolerateUnregisteredDoltDataDir(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	if err := os.MkdirAll(filepath.Join(town, ".dolt-data", "testdb_abc123"), 0o755); err != nil {
		t.Fatal(err)
	}

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak, got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], ".dolt-data/testdb_abc123") {
		t.Errorf("orphan database not detected: %v", leaks)
	}
}

func TestTripwire_ToleratesBdAtomicWriteTemp(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	// bd's JSONL export atomic-write temp file (gt-wdr): write-temp-then-rename
	// churn from any concurrent bd invocation, same class as .lock churn.
	writeFile(t, filepath.Join(town, ".beads", ".~issues.jsonl.2867150252"), "")
	// A real leak in the same directory must still be caught.
	writeFile(t, filepath.Join(town, ".beads", "scratch.jsonl"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak (real file only), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.jsonl") {
		t.Errorf("real leak not detected: %v", leaks)
	}
	joined := strings.Join(leaks, "\n")
	if strings.Contains(joined, ".~issues.jsonl") {
		t.Errorf("bd atomic-write temp flagged as leak: %v", leaks)
	}
}

func TestTripwire_ToleratesPlainAtomicWriteTemp(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	// krc's prune rewrite of the raw events log (gt-hotx): write-temp-then-
	// rename via <path>.tmp, same class as bd's .~ prefix and .lock churn.
	writeFile(t, filepath.Join(town, ".events.jsonl.tmp"), "")
	// A real leak at the town root must still be caught.
	writeFile(t, filepath.Join(town, "scratch.txt"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak (real file only), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.txt") {
		t.Errorf("real leak not detected: %v", leaks)
	}
	joined := strings.Join(leaks, "\n")
	if strings.Contains(joined, ".events.jsonl.tmp") {
		t.Errorf("atomic-write temp flagged as leak: %v", leaks)
	}
}

// gt-lqri: the .tmp suffix exemption is fails-open — a test that writes a
// .tmp to a watched directory and never renames it looks identical to a live
// atomic write under the suffix test, and the suffix alone forgives both. A
// .tmp whose base is not a known atomic-write sibling must be reported.
func TestTripwire_FailsClosedOnUnclaimedTmp(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	writeFile(t, filepath.Join(town, "scratch.tmp"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak (unclaimed .tmp), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.tmp") {
		t.Errorf("unclaimed .tmp not detected: %v", leaks)
	}
}

// beads.WriteRoutes creates .routes-<random>.tmp in .beads via
// os.CreateTemp (a .tmp-suffix name whose middle is a random number, so it is
// not derivable from the target routes.jsonl), and the daemon may hard-crash
// before the rename. The suffix must not carry it: .beads is a watched
// surface, and a .routes-<random>.tmp left there is exactly the residue the
// cross-check tolerates only because it names a real atomic-write target.
func TestTripwire_ToleratesRoutesCreateTempSiblings(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	writeFile(t, filepath.Join(town, ".beads", beads.RoutesTempPrefix+"12345.tmp"), "")
	writeFile(t, filepath.Join(town, ".beads", beads.RoutesTempPrefix+"99999.tmp"), "")
	// A real leak in the same directory must still be caught.
	writeFile(t, filepath.Join(town, ".beads", "scratch.jsonl"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak (real file only), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.jsonl") {
		t.Errorf("real leak not detected: %v", leaks)
	}
	joined := strings.Join(leaks, "\n")
	if strings.Contains(joined, beads.RoutesTempPrefix) {
		t.Errorf("CreateTemp %s*.tmp sibling flagged as leak: %v", beads.RoutesTempPrefix, leaks)
	}
}

// Same fails-open shape in a watched subdirectory: an abandoned .tmp under
// .dolt-data must be reported, not forgiven by the suffix.
func TestTripwire_FailsClosedOnUnclaimedTmpInWatchedSubdir(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	writeFile(t, filepath.Join(town, ".dolt-data", "scratch.tmp"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak (unclaimed .tmp), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.tmp") {
		t.Errorf("unclaimed .tmp not detected: %v", leaks)
	}
}

// A hard crash (SIGKILL) mid-prune leaves <path>.tmp in the watched town
// surface — the expected state for krc's own writers (events prune, feed
// prune, and its auto-prune state save), which all go through
// replaceWithLines/SaveAutoPruneState and share krc.ReplaceTempSuffix. The
// next prune removes the residue; the tripwire tolerates it in the meantime,
// and its reporting pass is the intended signal that a crash happened.
func TestTripwire_ToleratesCrashResidueTmp(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	writeFile(t, filepath.Join(town, events.EventsFile+krc.ReplaceTempSuffix), "")
	writeFile(t, filepath.Join(town, feed.FeedFile+krc.ReplaceTempSuffix), "")
	writeFile(t, filepath.Join(town, krc.AutoPruneStateFile+krc.ReplaceTempSuffix), "")
	// A real leak must still be caught alongside the crash residue.
	writeFile(t, filepath.Join(town, "scratch.txt"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak (real file only), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.txt") {
		t.Errorf("real leak not detected: %v", leaks)
	}
	joined := strings.Join(leaks, "\n")
	if strings.Contains(joined, ".tmp") {
		t.Errorf("crash-residue .tmp flagged as leak: %v", leaks)
	}
}

// The feed curator's separate truncate rotation writes a different suffix
// than krc's own prune (feed.TruncateTempSuffix, ".truncate.tmp", not
// krc.ReplaceTempSuffix's plain ".tmp") for the same target file,
// .feed.jsonl. A prior version of this allowlist assumed .feed.jsonl had
// only one writer's suffix and reported this one's crash residue as a leak;
// this test pins the real producer filename so that regression cannot
// recur silently.
func TestTripwire_ToleratesFeedCuratorTruncateResidue(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	writeFile(t, filepath.Join(town, feed.FeedFile+feed.TruncateTempSuffix), "")
	// A real leak must still be caught alongside the crash residue.
	writeFile(t, filepath.Join(town, "scratch.txt"), "oops")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected 1 leak (real file only), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "scratch.txt") {
		t.Errorf("real leak not detected: %v", leaks)
	}
	joined := strings.Join(leaks, "\n")
	if strings.Contains(joined, feed.TruncateTempSuffix) {
		t.Errorf("feed curator truncate residue flagged as leak: %v", leaks)
	}
}

func TestTripwire_FlagsFixtureActorEvents(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	f, err := os.OpenFile(filepath.Join(town, ".events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// One event from a known rig (concurrent legitimate traffic) and one from
	// a fixture rig that does not exist in rigs.json (the gt-x9o incident).
	_, _ = f.WriteString(`{"ts":"2026-09-08T00:01:00Z","source":"gt","type":"done","actor":"gastown/witness","visibility":"feed"}` + "\n")
	_, _ = f.WriteString(`{"ts":"2026-09-08T00:01:01Z","source":"gt","type":"spawn","actor":"myr/mycat","visibility":"feed"}` + "\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected exactly 1 leak (fixture actor), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "myr/mycat") {
		t.Errorf("fixture actor not flagged: %v", leaks)
	}
}

func TestTripwire_ToleratesUnknownActor(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	f, err := os.OpenFile(filepath.Join(town, ".events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// detectActor() (internal/cmd/sling_helpers.go) legitimately logs actor
	// "unknown" when GetRole() can't resolve an identity (e.g. a sling run
	// outside an agent session). That must not trip the leak detector (gt-ro0).
	_, _ = f.WriteString(`{"ts":"2026-09-08T00:01:00Z","source":"gt","type":"sling","actor":"unknown","visibility":"feed"}` + "\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("legitimate unknown-actor sling event flagged as leak: %v", leaks)
	}
}

func TestTripwire_ToleratesDogActor(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	f, err := os.OpenFile(filepath.Join(town, ".events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	// Deacon dogs (internal/cmd/dog.go, RoleDog) are real town-level
	// infrastructure workers that emit events like "nudge" during normal
	// operation (e.g. the deacon patrol's dog-pool-maintenance step). That
	// must not trip the leak detector (gt-kvc).
	_, _ = f.WriteString(`{"ts":"2026-09-08T00:01:00Z","source":"gt","type":"nudge","actor":"dog","visibility":"feed"}` + "\n")
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("legitimate dog-actor nudge event flagged as leak: %v", leaks)
	}
}

func TestTripwire_ToleratesAllBuiltinActorPrefixes(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	f, err := os.OpenFile(filepath.Join(town, ".events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range BuiltinActorPrefixes() {
		line, err := json.Marshal(map[string]string{
			"ts":         "2026-09-08T00:02:00Z",
			"source":     "gt",
			"type":       "test",
			"actor":      actor,
			"visibility": "feed",
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("builtin actor prefixes flagged as leaks: %v", leaks)
	}
}

// appendEvents appends raw content to the town's .events.jsonl, exactly as a
// concurrent live-town writer would.
func appendEvents(t *testing.T, town, content string) {
	t.Helper()
	f, err := os.OpenFile(filepath.Join(town, ".events.jsonl"), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// gt-5few: the tripwire's snapshot offset is a byte position taken by Stat on
// the live file, and concurrent agents append to that file constantly. When
// Stat races an append the offset lands mid-line, so the scan began with the
// line's tail — real live-town JSON missing its leading `{"ts":` (7 bytes) —
// and reported the concurrent agent's own event as leaked state. The refinery
// hit this on most gate cycles; zero tests failed, but both heavy packages
// reported FAIL and the run's exit code could not answer "did the code pass?".
func TestTripwire_ToleratesMidLineSnapshotOffset(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)
	appendEvents(t, town, `{"ts":"2026-09-18T21:29:48Z","source":"gt","type":"session_start","actor":"gastown/polecats/garnet","visibility":"feed"}`+"\n")

	// diff() would use the snapshot's own boundary, which is a line start in
	// this fixture; drive the detector directly at the truncation the town
	// actually produced so the mid-line path is exercised. +7 skips `{"ts":"`.
	leaks := suspiciousAppendedEvents(town, snap.eventsSize+7)
	if len(leaks) != 0 {
		t.Errorf("mid-line snapshot offset flagged a concurrent live-town event: %v", leaks)
	}
}

func TestTripwire_DetectsFixtureActorAfterMidLineSnapshotOffset(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)
	// Concurrent legitimate traffic first (the line the offset lands in), then
	// the leak that must still be caught.
	appendEvents(t, town,
		`{"ts":"2026-09-18T21:29:48Z","source":"gt","type":"nudge","actor":"gastown/witness","visibility":"feed"}`+"\n"+
			`{"ts":"2026-09-18T21:29:49Z","source":"gt","type":"spawn","actor":"myr/mycat","visibility":"feed"}`+"\n")

	leaks := suspiciousAppendedEvents(town, snap.eventsSize+7)
	if len(leaks) != 1 {
		t.Fatalf("expected exactly 1 leak (fixture actor), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "myr/mycat") {
		t.Errorf("fixture actor not flagged: %v", leaks)
	}
}

// A writer mid-append when the scan runs leaves a last line with no trailing
// newline; that fragment is truncated the same way, and must not be reported.
func TestTripwire_ToleratesPartialTrailingLine(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)
	appendEvents(t, town, `{"ts":"2026-09-18T22:29:19Z","source":"gt","type":"nudge","actor":"dog","payload":{"reason":"DOG_`)

	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("partial trailing line flagged as leak: %v", leaks)
	}
}

// Complete lines must still be judged: a malformed line that ends with a
// newline is not a truncation, so the leak check stays honest.
func TestTripwire_FlagsCompleteMalformedLine(t *testing.T) {
	town := makeFakeTown(t)
	snap := snapshotTown(town)
	appendEvents(t, town, "this is not json\n")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected exactly 1 leak (malformed line), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "unparseable") {
		t.Errorf("malformed line not flagged as unparseable: %v", leaks)
	}
}

func TestHermeticTest_ScrubsAndRedirects(t *testing.T) {
	t.Setenv("GT_ROLE", "gastown/polecats/flint")
	t.Setenv("BD_ACTOR", "someone")
	t.Setenv("BEADS_DB", "gt")
	origHome := os.Getenv("HOME")

	town := HermeticTest(t)

	for _, v := range []string{"GT_ROLE", "BD_ACTOR", "BEADS_DB"} {
		if got := os.Getenv(v); got != "" {
			t.Errorf("%s survived the scrub: %q", v, got)
		}
	}
	if home := os.Getenv("HOME"); home == origHome || home == "" {
		t.Errorf("HOME not redirected: %q", home)
	}
	if got := os.Getenv("GT_DOLT_PORT"); got != poisonDoltPort {
		t.Errorf("GT_DOLT_PORT = %q, want poisoned %q", got, poisonDoltPort)
	}
	if got := os.Getenv(HermeticEnvVar); got != "1" {
		t.Errorf("%s = %q, want 1", HermeticEnvVar, got)
	}
	if got := os.Getenv("GT_TOWN_ROOT"); got != town {
		t.Errorf("GT_TOWN_ROOT = %q, want sandbox town %q", got, town)
	}
	if ok, _ := workspace.IsWorkspace(town); !ok {
		t.Errorf("sandbox town %q is not a valid workspace", town)
	}
}

// TestStartHermetic_IsolatesTmuxSocketByDefault guards the isolation half of
// gt-yav3: without an explicit opt-out, StartHermetic must bind a throwaway
// per-process tmux socket rather than leaving the default (town) socket in
// force, so tests that construct tmux.Tmux land on a private server.
func TestStartHermetic_IsolatesTmuxSocketByDefault(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	withSavedEnv(t)
	_ = os.Unsetenv(AllowLiveTmuxEnv)

	h, err := StartHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)

	if h.TmuxSocket == "" {
		t.Error("TmuxSocket = \"\", want a per-process isolated socket by default")
	}
}

// TestStartHermetic_AllowLiveTmuxSurvivesScrub guards the gt-yav3 MR1 bounce:
// AllowLiveTmuxEnv (BEADS_TEST_ALLOW_LIVE_TMUX) is itself a BEADS_* variable,
// so scrubProcessEnv used to wipe it before anything ever checked it —
// isolateTmuxSocket() here, and tmux.NewTmux()'s own live-socket guard later
// in the process lifetime — making the documented opt-out permanently dead.
// StartHermetic must restore the caller's opt-out immediately after the scrub
// so both call sites see it.
func TestStartHermetic_AllowLiveTmuxSurvivesScrub(t *testing.T) {
	withSavedEnv(t)
	if err := os.Setenv(AllowLiveTmuxEnv, "1"); err != nil {
		t.Fatal(err)
	}

	h, err := StartHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)

	if got := os.Getenv(AllowLiveTmuxEnv); got != "1" {
		t.Errorf("%s = %q after StartHermetic, want \"1\" (the opt-out did not survive scrubProcessEnv)", AllowLiveTmuxEnv, got)
	}
	if h.TmuxSocket != "" {
		t.Errorf("TmuxSocket = %q, want \"\" — AllowLiveTmuxEnv=1 must bypass tmux socket isolation", h.TmuxSocket)
	}
}

func TestScratchTown_CwdResolvesToScratch(t *testing.T) {
	town := ScratchTown(t)

	root, err := workspace.FindFromCwd()
	if err != nil {
		t.Fatalf("FindFromCwd: %v", err)
	}
	// Resolve symlinks on both sides (macOS /var -> /private/var).
	wantReal, _ := filepath.EvalSymlinks(town)
	gotReal, _ := filepath.EvalSymlinks(root)
	if gotReal != wantReal {
		t.Errorf("FindFromCwd = %q, want scratch town %q", root, town)
	}
}

// TestStartHermetic_DockerOptInSurvivesScrub: DockerTestsEnv is a GT_*
// variable, so the scrub would wipe the opt-in before WithDolt or
// RequireDoltContainer could see it. It must survive both the TestMain
// harness and the per-test HermeticTest scrub.
func TestStartHermetic_DockerOptInSurvivesScrub(t *testing.T) {
	withSavedEnv(t)
	if err := os.Setenv(DockerTestsEnv, "1"); err != nil {
		t.Fatal(err)
	}

	h, err := StartHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)

	if !DockerTestsEnabled() {
		t.Errorf("%s did not survive StartHermetic's scrub", DockerTestsEnv)
	}
	HermeticTest(t)
	if !DockerTestsEnabled() {
		t.Errorf("%s did not survive HermeticTest's scrub", DockerTestsEnv)
	}
	// And the scrub still removes ordinary GT_* context.
	_ = os.Setenv("GT_ROLE", "gastown/polecats/topaz")
	scrubProcessEnv(false)
	if os.Getenv("GT_ROLE") != "" {
		t.Error("scrubProcessEnv kept GT_ROLE")
	}
	if !DockerTestsEnabled() {
		t.Errorf("scrubProcessEnv removed %s", DockerTestsEnv)
	}
}

// TestStartHermetic_WithDoltWithoutOptIn: with the container opt-in unset,
// a TestMain that asks for Dolt must still start (no error), with the port
// left empty so container-dependent tests skip — the contract daemon's and
// convoy's TestMains rely on, and the reason a bare `go test` of those
// packages passes in seconds without Docker.
func TestStartHermetic_WithDoltWithoutOptIn(t *testing.T) {
	withSavedEnv(t)
	_ = os.Unsetenv(DockerTestsEnv)

	h, err := StartHermetic(WithDolt())
	if err != nil {
		t.Fatalf("StartHermetic(WithDolt) without the opt-in must not fail: %v", err)
	}
	defer h.Finish(0)

	if DoltContainerPort() != "" {
		t.Errorf("DoltContainerPort = %q, want empty (no container may start without %s=1)", DoltContainerPort(), DockerTestsEnv)
	}
	if got := os.Getenv("GT_DOLT_PORT"); got != poisonDoltPort {
		t.Errorf("GT_DOLT_PORT = %q, want the poison port %q", got, poisonDoltPort)
	}
}

// TestFinish_DoltTerminationFailureFailsLoud pins the fix for gt-p98h/gt-n5g6:
// a Dolt container that fails to terminate must fail the run, not vanish
// silently and keep holding memory on the shared Docker VM until it's
// noticed hours later. terminateDoltContainer is swapped for a fake so this
// forces the failure branch without starting a real container.
func TestFinish_DoltTerminationFailureFailsLoud(t *testing.T) {
	orig := terminateDoltContainer
	terminateDoltContainer = func() error {
		return errors.New("simulated: container still running")
	}
	t.Cleanup(func() { terminateDoltContainer = orig })

	stderrR, stderrW, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStderr := os.Stderr
	os.Stderr = stderrW
	h := &Hermetic{}
	code := h.Finish(0)
	os.Stderr = origStderr
	stderrW.Close()
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(stderrR); err != nil {
		t.Fatalf("reading captured stderr: %v", err)
	}

	if code != 1 {
		t.Errorf("Finish(0) with a termination error = %d, want 1 (forced failure)", code)
	}
	if !strings.Contains(buf.String(), "HERMETIC TRIPWIRE: shared Dolt container failed to terminate") {
		t.Errorf("Finish stderr = %q, want it to name the termination tripwire", buf.String())
	}
}
