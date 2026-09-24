package beads

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The two lines bd wrote when TestEngineerCloseMRWithReasonRecordsMergeCommit-
// AndClearsActiveMR died on a saturated Docker VM (gt-6uhq). Kept verbatim:
// the retry keys off exactly this text, so a paraphrase here would test a
// string bd never emits.
const (
	observedIoTimeoutStderr   = `read tcp 127.0.0.1:65473->127.0.0.1:55107: i/o timeout`
	observedOpenFailureStderr = `Error: failed to open database: schema skew check: ` +
		`probing schema_migrations existence: invalid connection`
)

// observedCatalogRaceStderr is the line the refinery gates went red on
// (gt-unq4l), verbatim. The database it names is not the one the command line
// named — bd was told to open testdb_21eb6271a1b36e34 and the store open died
// on another test's testdb_e7165de82d57b7dd — which is the signature of the
// server's catalog changing under the open rather than any fault of the
// caller's own database.
const observedCatalogRaceStderr = `Error: failed to open Dolt store: failed to initialize schema: ` +
	`schema migration: reading pre-migration schema version: probing ` +
	`schema_migrations existence: Error 1105 (HY000): could not resolve ` +
	`initial root for database testdb_e7165de82d57b7dd/`

// observedLegacyWorkspaceStderr and observedAutoApplyRefusalStderr are the two
// refusals the catalog-race retry missed (gt-w4sxk), verbatim from the gate log
// that produced the bead. Both name a schema era for a testdb_ database Init
// had minted microseconds earlier.
const (
	observedLegacyWorkspaceStderr = `Error: legacy Dolt workspace detected; explicit migration ` +
		`is required before this bd version can open or modify the workspace. Preserve ` +
		`.beads unchanged and follow docs/getting-started/upgrading.md#cross-era-upgrades`
	observedAutoApplyRefusalStderr = `refusing to auto-apply 11 pending schema migrations to a ` +
		`server-mode database (v55 -> v66): other bd clients may depend on its current schema ` +
		`even though no Dolt remote is configured`
)

// initOnTestDatabaseArgs is the command shape every class above is retried on:
// the argv is part of the predicate, so a test that passed the stderr without it
// would not exercise the scoping (gt-w4sxk).
var initOnTestDatabaseArgs = []string{
	"init", "--prefix", "pt13dd3b6a", "--quiet",
	"--database", "testdb_21eb6271a1b36e34", "--server", "--server-port", "55069",
}

// TestBdContainerRetryRecoversFromConnectionFailure is gt-6uhq: a bd command
// that loses its Dolt connection to the test container must be retried, and
// the retry must return the command's result rather than the failure.
func TestBdContainerRetryRecoversFromConnectionFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 2)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	out, err := b.run("show", "gt-rqn", "--json")
	if err != nil {
		t.Fatalf("run() after a transient connection failure: %v", err)
	}
	if !strings.Contains(string(out), "gt-rqn") {
		t.Fatalf("run() output = %q, want the command's own result", out)
	}
	if got := stub.calls(t); got != 3 {
		t.Errorf("bd invocations = %d, want 3 (two failures then the success)", got)
	}
}

// TestBdInitRetriesTheCatalogRace is gt-unq4l: an init that loses its store
// open to the test server's catalog changing under it must be attempted again,
// because the name that could not be resolved is gone from the catalog by then.
func TestBdInitRetriesTheCatalogRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyCatalogRaceBDStub(t, 2)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 55069)
	if err := b.Init("pt13dd3b6a"); err != nil {
		t.Fatalf("Init after a transient catalog race: %v", err)
	}
	if got := stub.calls(t); got != 3 {
		t.Errorf("bd invocations = %d, want 3 (two races then the success)", got)
	}
}

// TestBdInitRetriesTheSchemaEraRace is gt-w4sxk: the catalog race met one check
// later, where bd got a root, read a schema era off it, and refused. That era
// cannot belong to the testdb_ database Init minted microseconds before, so the
// refusal is about the database the session landed on rather than the one it
// asked for, and another attempt resolves again.
//
// The bead's own sequence was attempt 1 lost the catalog race and attempt 2 met
// this refusal; both gate the same loop, so covering the second covers the pair.
func TestBdInitRetriesTheSchemaEraRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyBdStub(t, 2, observedLegacyWorkspaceStderr)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 55352)
	if err := b.Init("pt42ed0697"); err != nil {
		t.Fatalf("Init after a transient schema-era refusal: %v", err)
	}
	if got := stub.calls(t); got != 3 {
		t.Errorf("bd invocations = %d, want 3 (two refusals then the success)", got)
	}
}

// TestBdInitRetriesTheAutoApplyRefusal is the same class's other face: bd
// refusing to migrate a server-mode database to the current schema, on a
// database that has no schema of its own yet.
func TestBdInitRetriesTheAutoApplyRefusal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyBdStub(t, 2, observedAutoApplyRefusalStderr)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 55352)
	if err := b.Init("pkcf3e21b8"); err != nil {
		t.Fatalf("Init after a transient auto-apply refusal: %v", err)
	}
	if got := stub.calls(t); got != 3 {
		t.Errorf("bd invocations = %d, want 3 (two refusals then the success)", got)
	}
}

// TestBdInitRetryClearsTheWorkspaceTheFailedAttemptLeft is gt-o8i9f: attempt 1
// dies on the catalog race after writing .beads/dolt, and bd reads that
// half-written workspace as a legacy one on every attempt after it. Without a
// reset one transient race spends the whole attempt budget on a refusal no
// attempt can clear.
func TestBdInitRetryClearsTheWorkspaceTheFailedAttemptLeft(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installHalfWrittenBdInitStub(t)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 55069)
	if err := b.Init("pt13dd3b6a"); err != nil {
		t.Fatalf("Init after a failed attempt left a half-written .beads: %v", err)
	}
	if got := stub.calls(t); got != 2 {
		t.Errorf("bd invocations = %d, want 2 (the race, then the attempt the reset made possible)", got)
	}
	invocations := stub.invocations(t)
	if len(invocations) != 2 {
		t.Fatalf("recorded argv = %d, want 2", len(invocations))
	}
	if first, second := databaseFlag(t, invocations[0]), databaseFlag(t, invocations[1]); first == second {
		t.Errorf("both attempts ran against --database %s, want a fresh name for the one after the failure", first)
	}
}

// TestBdInitRetryLeavesAPreexistingWorkspaceAlone is that reset's safety
// property: only a .beads the loop watched appear is ever cleared. A workspace
// that was already there is its owner's, so Init keeps the behavior of retrying
// a directory no retry can fix rather than deleting it (gt-o8i9f).
func TestBdInitRetryLeavesAPreexistingWorkspaceAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyBdStub(t, 99, observedLegacyWorkspaceStderr)
	zeroRetryBackoff(t)

	workDir := t.TempDir()
	marker := filepath.Join(workDir, ".beads", "keep-me")
	if err := os.MkdirAll(filepath.Dir(marker), 0755); err != nil {
		t.Fatalf("create the caller's workspace: %v", err)
	}
	if err := os.WriteFile(marker, []byte("here first\n"), 0644); err != nil {
		t.Fatalf("write into the caller's workspace: %v", err)
	}

	b := NewIsolatedWithPort(workDir, 55352)
	if err := b.Init("pt13dd3b6a"); err == nil {
		t.Fatal("Init should fail: the stub refuses every attempt")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the retry cleared a workspace that was there before the first attempt: %v", err)
	}
	if got := stub.calls(t); got != bdContainerRetryAttempts {
		t.Errorf("bd invocations = %d, want %d (the retry is unchanged when the workspace is not ours)", got, bdContainerRetryAttempts)
	}
}

// TestBdInitSchemaRaceRetryLeavesOtherCommandsAlone: the era class is the only
// one scoped to the command, so the same stderr from anything but an init of a
// testdb_ name is an answer and costs one bd process (gt-w4sxk).
func TestBdInitSchemaRaceRetryLeavesOtherCommandsAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyBdStub(t, 99, observedLegacyWorkspaceStderr)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 55352)
	if _, err := b.run("show", "gt-rqn", "--json"); err == nil {
		t.Fatal("run() should fail")
	}
	if got := stub.calls(t); got != 1 {
		t.Errorf("bd invocations = %d, want 1 (the era class is init-only)", got)
	}
}

// TestBdContainerRetryStopsAtAttemptCapForTheCatalogRace: the second retry class
// carries the same bound as the first. A catalog that keeps changing under every
// attempt costs a fixed number of bd processes and reports bd's own stderr.
func TestBdContainerRetryStopsAtAttemptCapForTheCatalogRace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyCatalogRaceBDStub(t, 99)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 55069)
	_, err := b.run("init", "--prefix", "pt13dd3b6a", "--quiet")
	if err == nil {
		t.Fatal("run() should fail when every attempt loses the store open")
	}
	if !strings.Contains(err.Error(), "could not resolve initial root") {
		t.Errorf("exhausted-retry error should keep bd's stderr, got %v", err)
	}
	if got := stub.calls(t); got != bdContainerRetryAttempts {
		t.Errorf("bd invocations = %d, want %d (the cap)", got, bdContainerRetryAttempts)
	}
}

// TestBdContainerRetryStopsAtAttemptCap pins the bound: a container that never
// comes back costs a fixed number of attempts, not an unbounded retry loop, and
// the error the caller sees is still bd's own stderr.
func TestBdContainerRetryStopsAtAttemptCap(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 99)
	zeroRetryBackoff(t)

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	_, err := b.run("show", "gt-rqn", "--json")
	if err == nil {
		t.Fatal("run() should fail when every attempt loses the connection")
	}
	if !strings.Contains(err.Error(), "invalid connection") {
		t.Errorf("exhausted-retry error should keep bd's stderr, got %v", err)
	}
	if got := stub.calls(t); got != bdContainerRetryAttempts {
		t.Errorf("bd invocations = %d, want %d (the cap)", got, bdContainerRetryAttempts)
	}
}

// TestBdContainerRetryLeavesNonContainerWrappersAlone is the blast-radius
// contract: the retry exists for testutil's ephemeral container, and a wrapper
// that is not pointed at one — every production caller — behaves exactly as it
// did before, one invocation per command.
func TestBdContainerRetryLeavesNonContainerWrappersAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 99)

	b := NewIsolated(t.TempDir()) // isolated, but no container port
	if _, err := b.run("show", "gt-rqn", "--json"); err == nil {
		t.Fatal("run() should fail")
	}
	if got := stub.calls(t); got != 1 {
		t.Errorf("bd invocations = %d, want 1 (no retry outside the container)", got)
	}
}

// TestBdContainerRetryLeavesBehavioralFailuresAlone: a failure that is not a
// lost connection is an answer, and retrying it would only delay the report.
func TestBdContainerRetryLeavesBehavioralFailuresAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installBdStub(t, `#!/bin/sh
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
echo $((count + 1)) > __COUNT__
echo "Error: invalid status: done" >&2
exit 1
`)

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	if _, err := b.run("update", "gt-rqn", "--status=done"); err == nil {
		t.Fatal("run() should fail")
	}
	if got := stub.calls(t); got != 1 {
		t.Errorf("bd invocations = %d, want 1 (a behavioral failure is not retried)", got)
	}
}

// TestRetryableBdConnectionFailure pins the predicate's edges, including the
// one that keeps a wedged bd from being retried five times: a subprocess killed
// at its own deadline has already spent its whole budget.
func TestRetryableBdConnectionFailure(t *testing.T) {
	container := NewIsolatedWithPort(t.TempDir(), 3306)
	noContainer := New(t.TempDir())

	// A stalled container can also time bd out mid-connect, which prints both
	// the raw driver error and bd's open-path wrapper (gt-6uhq's actual stderr).
	observed := fmt.Errorf("bd show gt-rqn --json: %s\n%s",
		observedIoTimeoutStderr, observedOpenFailureStderr)

	tests := []struct {
		name string
		b    *Beads
		err  error
		want bool
	}{
		{name: "observed container failure", b: container, err: observed, want: true},
		{name: "invalid connection alone", b: container, err: fmt.Errorf("bd show x: invalid connection"), want: true},
		{name: "broken pipe", b: container, err: fmt.Errorf("bd show x: write: broken pipe"), want: true},
		{name: "no container", b: noContainer, err: observed, want: false},
		{name: "nil error", b: container, err: nil, want: false},
		{name: "not found is an answer", b: container, err: ErrNotFound, want: false},
		{name: "behavioral failure", b: container, err: fmt.Errorf("bd update x: invalid status"), want: false},
		{
			// The deadline kill is the case that must not retry even though the
			// message can carry a connection marker alongside it.
			name: "subprocess deadline",
			b:    container,
			err:  fmt.Errorf("bd show gt-rqn: %s: %w", observedIoTimeoutStderr, context.DeadlineExceeded),
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.retryableBdConnectionFailure(tt.err); got != tt.want {
				t.Errorf("retryableBdConnectionFailure(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestRetryableBdCatalogRace pins the second class's edges against the first:
// a store open lost to the test server's catalog is retried, the connection
// markers that already had a class stay in that one, and no class reaches a
// wrapper that is not pointed at the container (gt-unq4l).
func TestRetryableBdCatalogRace(t *testing.T) {
	container := NewIsolatedWithPort(t.TempDir(), 55069)
	noContainer := New(t.TempDir())

	observed := fmt.Errorf("bd init --prefix pt13dd3b6a --database testdb_21eb6271a1b36e34: %s",
		observedCatalogRaceStderr)

	tests := []struct {
		name             string
		b                *Beads
		err              error
		wantCatalogRace  bool
		wantRetriedAtAll bool
	}{
		{name: "observed catalog race", b: container, err: observed, wantCatalogRace: true, wantRetriedAtAll: true},
		{name: "production wrapper", b: noContainer, err: observed, wantCatalogRace: false, wantRetriedAtAll: false},
		{name: "nil error", b: container, err: nil, wantCatalogRace: false, wantRetriedAtAll: false},
		{name: "not found is an answer", b: container, err: ErrNotFound, wantCatalogRace: false, wantRetriedAtAll: false},
		{name: "behavioral failure", b: container, err: fmt.Errorf("bd update x: invalid status"), wantCatalogRace: false, wantRetriedAtAll: false},
		{
			// Same deadline rule as the connection class: a subprocess killed at
			// its own budget is wedged, and the catalog it saw is not the reason.
			name:            "subprocess deadline",
			b:               container,
			err:             fmt.Errorf("%s: %w", observedCatalogRaceStderr, context.DeadlineExceeded),
			wantCatalogRace: false, wantRetriedAtAll: false,
		},
		{
			// The first class keeps its own predicate: a lost connection is not
			// a catalog race, and a suite that reports it as one would skip.
			name:            "connection failure stays its own class",
			b:               container,
			err:             fmt.Errorf("bd show x: %s", observedOpenFailureStderr),
			wantCatalogRace: false, wantRetriedAtAll: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.retryableBdCatalogRace(tt.err); got != tt.wantCatalogRace {
				t.Errorf("retryableBdCatalogRace(%v) = %v, want %v", tt.err, got, tt.wantCatalogRace)
			}
			if got := tt.b.retryableBdTransientFailure(initOnTestDatabaseArgs, tt.err); got != tt.wantRetriedAtAll {
				t.Errorf("retryableBdTransientFailure(%v) = %v, want %v", tt.err, got, tt.wantRetriedAtAll)
			}
		})
	}
}

// TestRetryableBdInitSchemaRace pins the third class's edges (gt-w4sxk): the era
// refusals are retried for an init of a testdb_ name and for nothing else, and
// neither wider class absorbs them — a suite that reported one as a lost
// container would skip a run that should stay red.
func TestRetryableBdInitSchemaRace(t *testing.T) {
	container := NewIsolatedWithPort(t.TempDir(), 55069)
	noContainer := New(t.TempDir())

	legacy := fmt.Errorf("bd %s", observedLegacyWorkspaceStderr)
	autoApply := fmt.Errorf("bd %s", observedAutoApplyRefusalStderr)
	rigInit := []string{"init", "--prefix", "gt", "--quiet", "--database", "gastown", "--server", "--server-port", "55352"}

	tests := []struct {
		name             string
		b                *Beads
		args             []string
		err              error
		wantClass        bool
		wantRetriedAtAll bool
	}{
		{name: "observed legacy workspace refusal", b: container, args: initOnTestDatabaseArgs, err: legacy, wantClass: true, wantRetriedAtAll: true},
		{name: "observed auto-apply refusal", b: container, args: initOnTestDatabaseArgs, err: autoApply, wantClass: true, wantRetriedAtAll: true},
		{
			// The same stderr answers a rig database's real migration state, and
			// an operator has to see it rather than have it retried away.
			name: "init against a rig database", b: container, args: rigInit,
			err: legacy, wantClass: false, wantRetriedAtAll: false,
		},
		{
			name: "another command on a testdb_ database", b: container,
			args: []string{"show", "--database", "testdb_1a13842eb92b6edf", "gt-rqn"},
			err:  legacy, wantClass: false, wantRetriedAtAll: false,
		},
		{name: "production wrapper", b: noContainer, args: initOnTestDatabaseArgs, err: legacy, wantClass: false, wantRetriedAtAll: false},
		{name: "nil error", b: container, args: initOnTestDatabaseArgs, err: nil, wantClass: false, wantRetriedAtAll: false},
		{
			// Same deadline rule as the classes above: a subprocess killed at its
			// own budget is wedged, and the era it reported is not the reason.
			name: "subprocess deadline", b: container, args: initOnTestDatabaseArgs,
			err:       fmt.Errorf("%s: %w", observedLegacyWorkspaceStderr, context.DeadlineExceeded),
			wantClass: false, wantRetriedAtAll: false,
		},
		{
			// The earlier classes keep their own verdicts: a lost connection is
			// not an era refusal, and a run that spends its retries on one skips.
			name: "connection failure stays its own class", b: container, args: initOnTestDatabaseArgs,
			err:       fmt.Errorf("bd init: %s", observedOpenFailureStderr),
			wantClass: false, wantRetriedAtAll: true,
		},
		{
			name: "catalog race stays its own class", b: container, args: initOnTestDatabaseArgs,
			err:       fmt.Errorf("bd init: %s", observedCatalogRaceStderr),
			wantClass: false, wantRetriedAtAll: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.b.retryableBdInitSchemaRace(tt.args, tt.err); got != tt.wantClass {
				t.Errorf("retryableBdInitSchemaRace(%q, %v) = %v, want %v", tt.args, tt.err, got, tt.wantClass)
			}
			if got := tt.b.retryableBdTransientFailure(tt.args, tt.err); got != tt.wantRetriedAtAll {
				t.Errorf("retryableBdTransientFailure(%q, %v) = %v, want %v", tt.args, tt.err, got, tt.wantRetriedAtAll)
			}
		})
	}

	// An era refusal says nothing about the container being gone, so a run that
	// exhausts its retries on one fails rather than skipping (gt-cbtl).
	if container.ContainerUnavailable(legacy) {
		t.Error("a schema-era refusal was reported as the test container being gone")
	}
}

// TestBdInitOnTestDatabase pins the argv the class above reads, which is the
// whole of its blast radius: only Init passes a testdb_ --database, and only
// against the container (gt-w4sxk).
func TestBdInitOnTestDatabase(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "Init on the container", args: initOnTestDatabaseArgs, want: true},
		{name: "equals form", args: []string{"init", "--database=testdb_deadbeef"}, want: true},
		{name: "init against a rig database", args: []string{"init", "--database", "gastown", "--server"}, want: false},
		{name: "init that lets bd derive the name", args: []string{"init", "--prefix", "gt", "--quiet"}, want: false},
		{name: "another command on a testdb_ database", args: []string{"show", "--database", "testdb_deadbeef"}, want: false},
		{name: "database flag with no value", args: []string{"init", "--database"}, want: false},
		{name: "no args", args: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := bdInitOnTestDatabase(tt.args); got != tt.want {
				t.Errorf("bdInitOnTestDatabase(%q) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// TestRemintTestDatabase pins the argv rewrite the reset uses: the minted name a
// failed attempt used is replaced in both spellings the predicate accepts, the
// caller's own slice is left alone, and an argv with no minted name comes back
// unchanged (gt-o8i9f).
func TestRemintTestDatabase(t *testing.T) {
	const original = "testdb_1a13842eb92b6edf"

	tests := []struct {
		name string
		args []string
	}{
		{name: "space form", args: []string{"init", "--prefix", "pt13dd3b6a", "--database", original, "--server"}},
		{name: "equals form", args: []string{"init", "--database=" + original, "--quiet"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := remintTestDatabase(tt.args)
			if db := databaseFlag(t, got); db == original || !strings.HasPrefix(db, testDatabasePrefix) {
				t.Errorf("--database = %q, want a fresh %s name", db, testDatabasePrefix)
			}
			// The rewrite must hand back argv of the same shape: one flag with a
			// new value, every other argument where it was.
			if len(got) != len(tt.args) {
				t.Fatalf("rewritten argv = %q, want %d arguments", got, len(tt.args))
			}
			if want := databaseFlag(t, tt.args); want != original {
				t.Fatalf("the caller's argv was rewritten in place: --database = %q, want %q", want, original)
			}
		})
	}

	// No minted name to replace: the reset never reaches this shape, and the
	// rewrite must not invent a database for it.
	untouched := []string{"init", "--prefix", "gt", "--quiet", "--server"}
	if got := remintTestDatabase(untouched); strings.Join(got, " ") != strings.Join(untouched, " ") {
		t.Errorf("remintTestDatabase(%q) = %q, want it unchanged", untouched, got)
	}
}

// TestCatalogRaceIsNotAContainerGoneSkip picks the wider of the two verdicts a
// caller can reach from a spent retry, and pins the narrower one for this class
// (gt-cbtl): a catalog race says nothing about the container being gone, so
// exhausting the retries on one leaves the suite red rather than skipping it
// green.
func TestCatalogRaceIsNotAContainerGoneSkip(t *testing.T) {
	container := NewIsolatedWithPort(t.TempDir(), 55069)
	race := fmt.Errorf("bd init: %s", observedCatalogRaceStderr)

	if container.retryableBdTransientFailure(initOnTestDatabaseArgs, race) != true {
		t.Fatal("the catalog race is not retried at all")
	}
	if container.ContainerUnavailable(race) {
		t.Error("a catalog race was reported as the test container being gone")
	}
}

// TestContainerUnavailableNeverExcusesAProductionFailure pins the guard that
// the container-backed suites' skip rides on (gt-cbtl): the predicate is scoped
// to a wrapper built for a test container, so no production caller can turn a
// real failure into a skip.
func TestContainerUnavailableNeverExcusesAProductionFailure(t *testing.T) {
	lost := fmt.Errorf("bd init: %s\n%s", observedIoTimeoutStderr, observedOpenFailureStderr)

	production := New(t.TempDir())
	if production.ContainerUnavailable(lost) {
		t.Error("a production wrapper reported the test container unavailable")
	}
	container := NewIsolatedWithPort(t.TempDir(), 55107)
	if !container.ContainerUnavailable(lost) {
		t.Error("a wrapper for the test container did not report it unavailable")
	}
}

// TestBdContainerRetryWindowStopsTheLoop pins the bound that the attempt count
// alone does not give: attempts run subprocesses, so a container that stalls
// rather than refuses must not be retried on count alone. The window is aged
// out here rather than waited out.
func TestBdContainerRetryWindowStopsTheLoop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	stub := installFlakyConnectionBDStub(t, 99)
	zeroRetryBackoff(t)

	restore := bdContainerRetryWindow
	bdContainerRetryWindow = -time.Second // already expired when the loop starts
	t.Cleanup(func() { bdContainerRetryWindow = restore })

	b := NewIsolatedWithPort(t.TempDir(), 3306)
	if _, err := b.run("show", "gt-rqn", "--json"); err == nil {
		t.Fatal("run() should fail")
	}
	if got := stub.calls(t); got != 1 {
		t.Errorf("bd invocations = %d, want 1 (a spent retry window stops the loop)", got)
	}
}

// TestBdContainerRetryBackoffBounds pins the budget the retry can spend: it
// must grow, stay under the cap, and stay close to the nominal delay so five
// attempts cannot silently become a multi-minute stall.
func TestBdContainerRetryBackoffBounds(t *testing.T) {
	total := time.Duration(0)
	prev := time.Duration(0)
	for attempt := 1; attempt < bdContainerRetryAttempts; attempt++ {
		got := bdContainerRetryBackoff(attempt)
		if got > bdContainerRetryMaxBackoff {
			t.Fatalf("backoff(attempt=%d) = %v, want <= %v", attempt, got, bdContainerRetryMaxBackoff)
		}
		if got < prev*3/4 {
			t.Errorf("backoff(attempt=%d) = %v, want no less than %v (growth is monotonic)", attempt, got, prev*3/4)
		}
		prev = got
		total += got
	}
	if total > 30*time.Second {
		t.Errorf("total backoff = %v, want a bounded pause well under the per-command budget", total)
	}
}

// flakyBdStub is a fake bd on PATH that counts its invocations and fails the
// first failUntil of them with the connection-failure stderr gt-6uhq recorded.
type flakyBdStub struct {
	countFile string
	argsFile  string
}

func (s *flakyBdStub) calls(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(s.countFile)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatalf("read bd call count: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse bd call count %q: %v", raw, err)
	}
	return n
}

// invocations returns the argv of each bd invocation, in order. Only a stub
// whose script writes to __ARGS__ records them.
func (s *flakyBdStub) invocations(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile(s.argsFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatalf("read bd argv log: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out = append(out, strings.Fields(line))
		}
	}
	return out
}

// databaseFlag returns the --database value in one recorded argv, in either
// spelling, or fails the test: every argv these tests record is an Init, which
// always passes one.
func databaseFlag(t *testing.T, args []string) string {
	t.Helper()
	for i, arg := range args {
		switch {
		case arg == "--database" && i+1 < len(args):
			return args[i+1]
		case strings.HasPrefix(arg, "--database="):
			return strings.TrimPrefix(arg, "--database=")
		}
	}
	t.Fatalf("argv %q has no --database", args)
	return ""
}

// installFlakyBdStub writes a fake bd that fails the first failUntil
// invocations with failStderr and then succeeds. One script serves every retry
// class: what separates them is the stderr bd writes, not the shape of the
// failure, so a class added later is a failStderr argument rather than a fourth
// copy of this shell.
func installFlakyBdStub(t *testing.T, failUntil int, failStderr ...string) *flakyBdStub {
	t.Helper()
	var failures strings.Builder
	for _, line := range failStderr {
		fmt.Fprintf(&failures, "  echo %q >&2\n", line)
	}
	return installBdStub(t, fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
count=$((count + 1))
echo "$count" > __COUNT__
if [ "$count" -le %d ]; then
%s  exit 1
fi
printf '%%s\n' '[{"id":"gt-rqn","title":"MR","status":"open"}]'
exit 0
`, failUntil, failures.String()))
}

func installFlakyConnectionBDStub(t *testing.T, failUntil int) *flakyBdStub {
	t.Helper()
	return installFlakyBdStub(t, failUntil, observedIoTimeoutStderr, observedOpenFailureStderr)
}

// installFlakyCatalogRaceBDStub writes a fake bd that fails the first failUntil
// invocations with gt-unq4l's store-open race and then succeeds.
func installFlakyCatalogRaceBDStub(t *testing.T, failUntil int) *flakyBdStub {
	t.Helper()
	return installFlakyBdStub(t, failUntil, observedCatalogRaceStderr)
}

// installHalfWrittenBdInitStub writes the fake bd gt-o8i9f's sequence needs: an
// attempt that writes .beads/dolt and dies on the catalog race, bd refusing a
// .beads already on disk the way its legacy guard does, and an attempt that
// succeeds against a directory with none. So an init only gets past attempt 1 if
// the workspace that attempt left is gone by the time the next one runs — which
// is the behavior under test, not an artifact of the stub.
func installHalfWrittenBdInitStub(t *testing.T) *flakyBdStub {
	t.Helper()
	return installBdStub(t, fmt.Sprintf(`#!/bin/sh
if [ "${1:-}" = "--allow-stale" ] && [ "${2:-}" = "version" ]; then
  echo "Error: unknown flag: --allow-stale" >&2
  exit 0
fi
count=0
[ -f __COUNT__ ] && count=$(cat __COUNT__)
count=$((count + 1))
echo "$count" > __COUNT__
echo "$*" >> __ARGS__
if [ -d .beads ]; then
  echo %q >&2
  exit 1
fi
if [ "$count" -le 1 ]; then
  mkdir -p .beads/dolt
  echo %q >&2
  exit 1
fi
mkdir -p .beads
exit 0
`, observedLegacyWorkspaceStderr, observedCatalogRaceStderr))
}

// installBdStub writes a fake bd whose body is script, substitutes __COUNT__
// with a per-test counter file and __ARGS__ with a per-test log of the argv it
// was called with, and puts it first on PATH. The counter file is how the retry
// loop's tests observe how many attempts actually happened — the behavior under
// test is the number of bd processes, not a Go-side tally.
func installBdStub(t *testing.T, script string) *flakyBdStub {
	t.Helper()
	// The allow-stale probe is cached per bd path for the whole process, so a
	// stub installed here must not leave a verdict from another test's binary.
	ResetBdAllowStaleCacheForTest()
	t.Cleanup(ResetBdAllowStaleCacheForTest)

	dir := t.TempDir()
	countFile := filepath.Join(dir, "bd-calls")
	argsFile := filepath.Join(dir, "bd-args")
	script = strings.ReplaceAll(script, "__COUNT__", countFile)
	script = strings.ReplaceAll(script, "__ARGS__", argsFile)

	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		t.Fatalf("create stub bin dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "bd"), []byte(script), 0755); err != nil {
		t.Fatalf("write bd stub: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return &flakyBdStub{countFile: countFile, argsFile: argsFile}
}

// zeroRetryBackoff removes the real pause for the duration of one test, through
// the same seam the production loop reads. Without it the attempt-cap test would
// spend the full ~7.5s of backoff to observe that a bounded loop is bounded.
func zeroRetryBackoff(t *testing.T) {
	t.Helper()
	restore := bdContainerRetryBackoffFn
	bdContainerRetryBackoffFn = func(int) time.Duration { return 0 }
	t.Cleanup(func() { bdContainerRetryBackoffFn = restore })
}
