package testutil

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/workspace"
)

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
	t.Parallel()
	town := makeFakeTown(t)
	snap := snapshotTown(town)
	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("clean town reported leaks: %v", leaks)
	}
}

func TestTripwire_DetectsNewFilesAndDatabases(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	// A write-temp-then-rename sibling of a live target (the daemon's
	// .events.jsonl prune rotation): same class as bd's .~ prefix and .lock
	// churn.
	writeFile(t, filepath.Join(town, events.EventsFile+events.PruneTempSuffix), "")
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
	if strings.Contains(joined, events.PruneTempSuffix) {
		t.Errorf("atomic-write temp flagged as leak: %v", leaks)
	}
}

// gt-lqri: the .tmp suffix exemption is fails-open — a test that writes a
// .tmp to a watched directory and never renames it looks identical to a live
// atomic write under the suffix test, and the suffix alone forgives both. A
// .tmp whose base is not a known atomic-write sibling must be reported.
func TestTripwire_FailsClosedOnUnclaimedTmp(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

func TestTripwire_FlagsFixtureActorEvents(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// gt-d9423: the daemon's crash detection names a live polecat's tmux session
// ("gt-opal") as the actor, a prefix the town does not know, so its own two
// session_death lines red four packages whose tests all passed.
func TestTripwire_ToleratesDaemonAuthoredSessionDeath(t *testing.T) {
	t.Parallel()
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	appendEvents(t, town,
		`{"ts":"2026-09-30T06:37:08Z","source":"gt","type":"session_death","actor":"gt-opal","payload":{"agent":"gastown/polecats/opal","caller":"daemon","reason":"crash detected by daemon health check","session":"gt-opal"},"visibility":"feed"}`+"\n"+
			`{"ts":"2026-09-30T06:37:09Z","source":"gt","type":"session_death","actor":"gt-amber","payload":{"agent":"gastown/polecats/amber","caller":"daemon","reason":"crash detected by daemon health check","session":"gt-amber"},"visibility":"feed"}`+"\n")

	if leaks := snap.diff(); len(leaks) != 0 {
		t.Errorf("daemon-authored session_death flagged as test leakage: %v", leaks)
	}
}

// The daemon tolerance keys on the caller value, not on an event merely having
// a payload, so the gt-x9o fixture-actor catch is not widened for free.
func TestTripwire_FlagsFixtureActorWithNonDaemonCaller(t *testing.T) {
	t.Parallel()
	town := makeFakeTown(t)
	snap := snapshotTown(town)

	appendEvents(t, town,
		`{"ts":"2026-09-30T06:37:08Z","source":"gt","type":"spawn","actor":"myr/mycat","payload":{"caller":"myr/mycat","session":"myr-mycat"},"visibility":"feed"}`+"\n")

	leaks := snap.diff()
	if len(leaks) != 1 {
		t.Fatalf("expected exactly 1 leak (fixture actor), got %d: %v", len(leaks), leaks)
	}
	if !strings.Contains(leaks[0], "myr/mycat") {
		t.Errorf("fixture actor not flagged: %v", leaks)
	}
}

func TestTripwire_ToleratesDogActor(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	town := makeFakeTown(t)
	// The snapshot lands 7 bytes into a line an agent is still appending.
	appendEvents(t, town, `{"ts":"`)
	snap := snapshotTown(town)
	appendEvents(t, town, `2026-09-18T21:29:48Z","source":"gt","type":"session_start","actor":"gastown/polecats/garnet","visibility":"feed"}`+"\n")

	leaks := snap.diff()
	if len(leaks) != 0 {
		t.Errorf("mid-line snapshot offset flagged a concurrent live-town event: %v", leaks)
	}
}

func TestTripwire_DetectsFixtureActorAfterMidLineSnapshotOffset(t *testing.T) {
	t.Parallel()
	town := makeFakeTown(t)
	// Concurrent legitimate traffic first (the line the snapshot lands in),
	// then the leak that must still be caught.
	appendEvents(t, town, `{"ts":"`)
	snap := snapshotTown(town)
	appendEvents(t, town,
		`2026-09-18T21:29:48Z","source":"gt","type":"nudge","actor":"gastown/witness","visibility":"feed"}`+"\n"+
			`{"ts":"2026-09-18T21:29:49Z","source":"gt","type":"spawn","actor":"myr/mycat","visibility":"feed"}`+"\n")

	leaks := snap.diff()
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
	t.Parallel()
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
	t.Parallel()
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

// gt-yirj6: the daemon's events_prune replaces .events.jsonl with its newest
// lines while tests run. The pruned file is smaller than the snapshot's end
// and starts inside what was history, so a saved byte offset scans nothing
// (missing the leak appended after the prune) or replays retained history
// (flagging the old fixture line). The tripwire re-syncs after the last line
// it had passed and judges exactly the lines appended since the snapshot.
func TestTripwire_FollowsPruneBetweenSnapshotAndScan(t *testing.T) {
	t.Parallel()
	town := makeFakeTown(t)
	path := filepath.Join(town, ".events.jsonl")
	var history strings.Builder
	for range 40 {
		history.WriteString(`{"ts":"2026-09-18T20:00:00Z","source":"gt","type":"nudge","actor":"gastown/witness","visibility":"feed"}` + "\n")
	}
	// Pre-snapshot history the tripwire must never report, kept by the prune.
	history.WriteString(`{"ts":"2026-09-18T20:00:01Z","source":"gt","type":"spawn","actor":"old/fixture","visibility":"feed"}` + "\n")
	appendEvents(t, town, history.String())

	snap := snapshotTown(town)
	appendEvents(t, town, `{"ts":"2026-09-18T21:00:00Z","source":"gt","type":"nudge","actor":"gastown/refinery","visibility":"feed"}`+"\n")
	res, err := events.Prune(path, events.PruneOptions{MaxBytes: 1024}, time.Now())
	if err != nil || !res.Pruned() {
		t.Fatalf("Prune = %+v, %v; want a rewrite", res, err)
	}
	appendEvents(t, town, `{"ts":"2026-09-18T21:00:01Z","source":"gt","type":"spawn","actor":"myr/mycat","visibility":"feed"}`+"\n")

	leaks := snap.diff()
	if len(leaks) != 1 || !strings.Contains(leaks[0], "myr/mycat") {
		t.Fatalf("leaks = %v, want only the fixture actor appended after the prune", leaks)
	}
}

// An events file that appears during the run is judged from its start.
func TestTripwire_ScansEventsFileCreatedAfterSnapshot(t *testing.T) {
	t.Parallel()
	town := makeFakeTown(t)
	if err := os.Remove(filepath.Join(town, ".events.jsonl")); err != nil {
		t.Fatal(err)
	}
	snap := snapshotTown(town)
	writeFile(t, filepath.Join(town, ".events.jsonl"),
		`{"ts":"2026-09-18T21:00:01Z","source":"gt","type":"spawn","actor":"myr/mycat","visibility":"feed"}`+"\n")

	leaks := snap.diff()
	var eventLeaks []string
	for _, l := range leaks {
		if strings.Contains(l, "actor") {
			eventLeaks = append(eventLeaks, l)
		}
	}
	if len(eventLeaks) != 1 || !strings.Contains(eventLeaks[0], "myr/mycat") {
		t.Fatalf("leaks = %v, want the fixture actor in the new events file", leaks)
	}
}

// goEnvSet makes preserveGoEnv a no-op on a fake harness: everything it
// would ask the go tool for is already set.
var goEnvSet = []string{"GOENV=/dev/go/env", "GOPATH=/dev/go", "GOCACHE=/dev/cache", "GOMODCACHE=/dev/go/pkg/mod"}

func TestHermeticTest_ScrubsAndRedirects(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{"GT_ROLE=gastown/polecats/flint", "BD_ACTOR=someone", "BEADS_DB=gt", "HOME=/home/dev"}, goEnvSet...)...)

	town := f.hermeticTest(t)

	for _, v := range []string{"GT_ROLE", "BD_ACTOR", "BEADS_DB"} {
		if got, ok := f.env.LookupEnv(v); ok {
			t.Errorf("%s survived the scrub: %q", v, got)
		}
	}
	if home := f.env.get("HOME"); home == "/home/dev" || home == "" {
		t.Errorf("HOME not redirected: %q", home)
	}
	for k, want := range map[string]string{
		"GT_DOLT_PORT":          poisonDoltPort,
		"BEADS_DOLT_PORT":       poisonDoltPort,
		HermeticEnvVar:          "1",
		"GT_TOWN_ROOT":          town,
		"BEADS_DOLT_AUTO_START": "0",
	} {
		if got := f.env.get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if ok, _ := workspace.IsWorkspace(town); !ok {
		t.Errorf("sandbox town %q is not a valid workspace", town)
	}
	if _, ok := f.env.LookupEnv(workspace.EnvForbiddenTownRoot); ok {
		t.Error("a forbidden root was set with no live town around")
	}
}

// With an outer runner's Dolt (GT_TEST_EXTERNAL_DOLT=1) the per-test scrub
// keeps its routing instead of poisoning it.
func TestHermeticTest_KeepsExternalDolt(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{"GT_TEST_EXTERNAL_DOLT=1", "GT_DOLT_PORT=4400", "BEADS_DOLT_PORT=4400"}, goEnvSet...)...)
	f.hermeticTest(t)
	if got := f.env.get("GT_DOLT_PORT"); got != "4400" {
		t.Errorf("GT_DOLT_PORT = %q, want the external server's 4400", got)
	}
}

// TestStartHermetic_IsolatesTmuxSocketByDefault guards the isolation half of
// gt-yav3: without an explicit opt-out, StartHermetic must bind a throwaway
// per-process tmux socket rather than leaving the default (town) socket in
// force, so tests that construct tmux.Tmux land on a private server. Finish
// kills that server. With no tmux installed there is nothing to bind.
func TestStartHermetic_IsolatesTmuxSocketByDefault(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, goEnvSet...)
	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	if h.TmuxSocket != "gt-test-4242" || f.socket != "gt-test-4242" {
		t.Errorf("TmuxSocket = %q, bound %q; want the per-process gt-test-4242", h.TmuxSocket, f.socket)
	}
	h.Finish(0)
	if !slices.Contains(f.commands(), "tmux -L gt-test-4242 kill-server") {
		t.Errorf("Finish did not kill the isolated server: %q", f.commands())
	}

	f = newFakeHarness(t, goEnvSet...).noTool("tmux")
	if h, err := f.startHermetic(); err != nil || h.TmuxSocket != "" || f.socket != "" {
		t.Errorf("without tmux: %v, socket %q, bound %q; want none", err, h.TmuxSocket, f.socket)
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
	t.Parallel()
	f := newFakeHarness(t, append([]string{AllowLiveTmuxEnv + "=1"}, goEnvSet...)...)

	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	if got := f.env.get(AllowLiveTmuxEnv); got != "1" {
		t.Errorf("%s = %q after StartHermetic, want \"1\" (the opt-out did not survive the scrub)", AllowLiveTmuxEnv, got)
	}
	if h.TmuxSocket != "" || f.socket != "" {
		t.Errorf("TmuxSocket = %q, bound %q; want none — AllowLiveTmuxEnv=1 must bypass tmux socket isolation", h.TmuxSocket, f.socket)
	}
}

// The scrub removes the invoking agent's GT_*/BD_*/BEADS_* context and its
// tmux identity, poisons the Dolt ports, and redirects HOME and the config
// dirs into the sandbox; the live tmux server it was launched from is
// remembered for Finish's tripwire.
func TestStartHermetic_ScrubsAndRedirects(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{
		"GT_ROLE=gastown/polecats/topaz", "BD_ACTOR=someone", "BEADS_DIR=/live/.beads",
		"TMUX=/private/tmp/tmux-501/gt-town,123,0", "TMUX_PANE=%3", "HOME=/home/dev", "PATH=/usr/bin",
	}, goEnvSet...)...)

	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	for _, k := range []string{"GT_ROLE", "BD_ACTOR", "BEADS_DIR", "TMUX", "TMUX_PANE"} {
		if v, ok := f.env.LookupEnv(k); ok {
			t.Errorf("%s survived the scrub: %q", k, v)
		}
	}
	if h.LiveTmuxSocket != "gt-town" {
		t.Errorf("LiveTmuxSocket = %q, want gt-town from the inherited $TMUX", h.LiveTmuxSocket)
	}
	for k, want := range map[string]string{
		"HOME":              h.HomeDir,
		"CLAUDE_CONFIG_DIR": filepath.Join(h.SandboxDir, "claude"),
		"XDG_CONFIG_HOME":   filepath.Join(h.HomeDir, ".config"),
		"GT_TOWN_ROOT":      h.TownRoot,
		"GT_DOLT_PORT":      poisonDoltPort,
		"BEADS_DOLT_PORT":   poisonDoltPort,
		HermeticEnvVar:      "1",
		"PATH":              "/usr/bin",
	} {
		if got := f.env.get(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if data, err := os.ReadFile(filepath.Join(h.HomeDir, ".gitconfig")); err != nil || !strings.Contains(string(data), "name = Hermetic Test") {
		t.Errorf("sandbox gitconfig: %v", err)
	}
	h.Finish(0)
	if _, err := os.Stat(h.SandboxDir); !os.IsNotExist(err) {
		t.Errorf("Finish left the sandbox: %v", err)
	}
}

// WithoutGit puts a refusing git first on PATH.
func TestStartHermetic_WithoutGitPutsRefusingGitFirst(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{"PATH=/usr/bin"}, goEnvSet...)...)
	h, err := f.startHermetic(WithoutGit())
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)
	dir := filepath.Join(h.SandboxDir, "nogit")
	if got := f.env.get("PATH"); got != dir+string(os.PathListSeparator)+"/usr/bin" {
		t.Errorf("PATH = %q, want the refusing git's dir first", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "git")); err != nil {
		t.Errorf("no refusing git written: %v", err)
	}
}

// Started inside a live town, the harness forbids resolving it, snapshots it
// for the tripwire, and refuses to start when an in-process resolver still
// reaches it (gt-dr664).
func TestStartHermetic_LiveTown(t *testing.T) {
	t.Parallel()
	town, inner := liveTownFixture(t)
	under := func(root string) bool {
		return root == town || strings.HasPrefix(root, town+string(filepath.Separator))
	}

	f := newFakeHarness(t, goEnvSet...)
	f.findTown = func() (string, error) { return town, nil }
	f.getwd = func() (string, error) { return inner, nil }
	f.forbidden = under
	f.resolvers = []liveTownResolver{{"loud", func(string) string { panic(workspace.ErrForbiddenTownRoot) }}}
	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic inside a live town: %v", err)
	}
	if h.RealTownRoot != town || h.snap == nil {
		t.Errorf("RealTownRoot = %q, snapshot %v; want the live town watched", h.RealTownRoot, h.snap != nil)
	}
	if got := f.env.get(workspace.EnvForbiddenTownRoot); got != town {
		t.Errorf("%s = %q, want the live town", workspace.EnvForbiddenTownRoot, got)
	}
	writeFile(t, filepath.Join(town, ".dolt-data", "testdb_leak"), "")
	if code := h.Finish(0); code != 1 || !strings.Contains(f.stderr.String(), "testdb_leak") {
		t.Errorf("Finish after a leak = %d, stderr %q; want the tripwire to fail the run", code, f.stderr.String())
	}

	f = newFakeHarness(t, goEnvSet...)
	f.findTown = func() (string, error) { return town, nil }
	f.forbidden = under
	f.resolvers = []liveTownResolver{{"leaky", func(string) string { return town }}}
	if _, err := f.startHermetic(); err == nil || !strings.Contains(err.Error(), "leaky") {
		t.Errorf("StartHermetic with a leaking resolver = %v, want a refusal naming it", err)
	}
}

// TestStartHermetic_DockerOptInSurvivesScrub: DockerTestsEnv is a GT_*
// variable, so the scrub would wipe the opt-in before WithDolt or
// RequireDoltContainer could see it. It must survive both the TestMain
// harness and the per-test HermeticTest scrub.
func TestStartHermetic_DockerOptInSurvivesScrub(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{DockerTestsEnv + "=1"}, goEnvSet...)...)

	h, err := f.startHermetic()
	if err != nil {
		t.Fatalf("StartHermetic: %v", err)
	}
	defer h.Finish(0)

	if !dockerTestsEnabled(f.env) {
		t.Errorf("%s did not survive StartHermetic's scrub", DockerTestsEnv)
	}
	f.hermeticTest(t)
	if !dockerTestsEnabled(f.env) {
		t.Errorf("%s did not survive HermeticTest's scrub", DockerTestsEnv)
	}
	// And the scrub still removes ordinary GT_* context.
	_ = f.env.Setenv("GT_ROLE", "gastown/polecats/topaz")
	scrubEnv(f.env, false)
	if _, ok := f.env.LookupEnv("GT_ROLE"); ok {
		t.Error("scrubEnv kept GT_ROLE")
	}
	if !dockerTestsEnabled(f.env) {
		t.Errorf("scrubEnv removed %s", DockerTestsEnv)
	}
}

// With an outer runner's Dolt (GT_TEST_EXTERNAL_DOLT=1) the scrub keeps its
// routing variables, and only then.
func TestScrubEnv_KeepsDoltPassthroughOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	entries := []string{"GT_DOLT_PORT=4400", "GT_DOLT_HOST=h", "BEADS_DOLT_PORT=4400", "BEADS_DOLT_SERVER_HOST=h", "GT_TEST_EXTERNAL_DOLT=1", "GT_ROLE=x"}
	keep := newMapEnv(entries...)
	scrubEnv(keep, true)
	drop := newMapEnv(entries...)
	scrubEnv(drop, false)
	for _, k := range doltPassthroughVars {
		if _, ok := keep.LookupEnv(k); !ok {
			t.Errorf("keepDolt dropped %s", k)
		}
		if _, ok := drop.LookupEnv(k); ok {
			t.Errorf("without keepDolt %s survived", k)
		}
	}
	if _, ok := keep.LookupEnv("GT_ROLE"); ok {
		t.Error("keepDolt kept GT_ROLE")
	}
}

// TestStartHermetic_WithDoltWithoutOptIn: with the container opt-in unset,
// a TestMain that asks for Dolt must still start (no error), with the port
// left poisoned so container-dependent tests skip — the contract daemon's and
// convoy's TestMains rely on, and the reason a bare `go test` of those
// packages passes in seconds without Docker.
func TestStartHermetic_WithDoltWithoutOptIn(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, goEnvSet...)

	h, err := f.startHermetic(WithDolt())
	if err != nil {
		t.Fatalf("StartHermetic(WithDolt) without the opt-in must not fail: %v", err)
	}
	defer h.Finish(0)

	if got := f.env.get("GT_DOLT_PORT"); got != poisonDoltPort {
		t.Errorf("GT_DOLT_PORT = %q, want the poison port %q", got, poisonDoltPort)
	}
	if !strings.Contains(f.stderr.String(), "Dolt-dependent tests will skip") {
		t.Errorf("stderr = %q, want the skip warning", f.stderr.String())
	}
}

// TestStartHermetic_WithDoltOptedInFailsWithoutContainer: once GT_TEST_DOCKER=1
// opts in, a container that will not start fails the package's TestMain
// instead of letting every container test skip on the empty port — an opt-in
// run that loses its coverage must not read as green.
func TestStartHermetic_WithDoltOptedInFailsWithoutContainer(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t, append([]string{DockerTestsEnv + "=1"}, goEnvSet...)...)
	f.ensureDolt = func() error { return errors.New("simulated: Docker not available") }

	h, err := f.startHermetic(WithDolt())
	if err == nil {
		h.Finish(0)
		t.Fatal("StartHermetic(WithDolt) with the opt-in set and no container succeeded; want an error")
	}
	if !strings.Contains(err.Error(), "simulated: Docker not available") || !strings.Contains(err.Error(), DockerTestsEnv) {
		t.Errorf("StartHermetic error = %q, want it to carry the cause and name %s", err, DockerTestsEnv)
	}
}

// TestFinish_DoltTerminationFailureFailsLoud pins the fix for gt-p98h/gt-n5g6:
// a Dolt container that fails to terminate must fail the run, not vanish
// silently and keep holding memory on the shared Docker VM until it's
// noticed hours later.
func TestFinish_DoltTerminationFailureFailsLoud(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t)
	f.terminateDolt = func() error { return errors.New("simulated: container still running") }

	if code := (&Hermetic{host: f.harnessHost}).Finish(0); code != 1 {
		t.Errorf("Finish(0) with a termination error = %d, want 1 (forced failure)", code)
	}
	if !strings.Contains(f.stderr.String(), "HERMETIC TRIPWIRE: shared Dolt container failed to terminate") {
		t.Errorf("Finish stderr = %q, want it to name the termination tripwire", f.stderr.String())
	}
}

// A catalog-guard failure at teardown fails the run under its own banner,
// which names the database, rather than the termination tripwire's.
func TestFinish_DoltCatalogGuardFailsLoud(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t)
	f.terminateDolt = func() error {
		return catalogViolations([]string{"gt_test", "information_schema", "mysql", "beads"}, nil, nil)
	}

	if code := (&Hermetic{host: f.harnessHost}).Finish(0); code != 1 {
		t.Errorf("Finish(0) with a catalog-guard error = %d, want 1", code)
	}
	for _, want := range []string{"DOLT CATALOG GUARD", `database "beads" was created`} {
		if !strings.Contains(f.stderr.String(), want) {
			t.Errorf("Finish stderr = %q, want it to contain %q", f.stderr.String(), want)
		}
	}
}

// Sessions the tests left on the live tmux server the process was launched
// from fail the run (gt-2bj).
func TestFinish_LiveTmuxSessionsFailTheRun(t *testing.T) {
	t.Parallel()
	f := newFakeHarness(t)
	f.tmuxSessions = func(socket string) []string {
		if socket == "gt-town" {
			return []string{"gt-test-phantom (cwd=/tmp/x)"}
		}
		return nil
	}
	if code := (&Hermetic{host: f.harnessHost, LiveTmuxSocket: "gt-town"}).Finish(0); code != 1 {
		t.Errorf("Finish(0) with a leaked live session = %d, want 1", code)
	}
	if !strings.Contains(f.stderr.String(), "gt-test-phantom") {
		t.Errorf("Finish stderr = %q, want the session named", f.stderr.String())
	}
}
