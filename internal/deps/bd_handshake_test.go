package deps

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cmdtree"
)

// Payloads captured from real builds on 2026-09-29, carried as the parsed
// report the store hands the handshake.
var (
	// The town's installed fork build before the D1 machine surface.
	bdInfoPreD1 = beads.VersionInfo{Version: "1.2.2", Build: "537accb", Commit: "537accbea9ca6f2be8d478ac97c5137bce4ac1b2"}
	// The fork build at beads da4983e.
	bdInfoFork = beads.VersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}
	// Upstream steveyegge/beads: a semver and nothing else.
	bdInfoUpstream = beads.VersionInfo{Version: "1.0.5", Build: "release"}
)

// fakeStore answers the handshake's two reads. A nil levelFn answers the level
// the version report names, as a database bd has migrated.
type fakeStore struct {
	info    beads.VersionInfo
	verErr  error
	levelFn func(reads int) (int, error)
	reads   int
}

func (f *fakeStore) Version() (beads.VersionInfo, error) { return f.info, f.verErr }

func (f *fakeStore) SchemaLevel() (int, error) {
	f.reads++
	if f.levelFn != nil {
		return f.levelFn(f.reads)
	}
	return f.info.DBSchemaVersion, nil
}

// storeRetryNow is the retry interval the handshake tests run with: the checks
// are the loop, not the wait, so they drive it as fast as they can.
const storeRetryNow = time.Millisecond

func TestCheckBDHandshake_AcceptsForkBuildAtDBLevel(t *testing.T) {
	t.Parallel()
	hs, err := CheckBDHandshake(context.Background(), "/bin/bd", &fakeStore{info: bdInfoFork})
	if err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
	if hs.DBSchema != 66 || hs.Found.BuildID != "da4983e" || hs.Path != "/bin/bd" {
		t.Fatalf("handshake = %+v", hs)
	}
	if got := hs.String(); !strings.Contains(got, "bd 1.2.2") || !strings.Contains(got, "database at 66") {
		t.Errorf("String() = %q", got)
	}
}

// TestCheckBDHandshake_DBLevelAsString: bd sql may answer the level as a
// number or as text; the store parses both, and the handshake only compares.
func TestCheckBDHandshake_DBLevelAsString(t *testing.T) {
	t.Parallel()
	s := &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) { return 66, nil }}
	if _, err := CheckBDHandshake(context.Background(), "/bin/bd", s); err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
}

func TestCheckBDHandshake_Refusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		store     *fakeStore
		wantParts []string
	}{
		{
			name:      "installed pre-D1 fork build",
			store:     &fakeStore{info: bdInfoPreD1},
			wantParts: []string{"537accb", "no build_id", "no contract_version", "no db_schema_version"},
		},
		{
			name:      "upstream build",
			store:     &fakeStore{info: bdInfoUpstream},
			wantParts: []string{"1.0.5", "no build_id"},
		},
		{
			name:      "unknown contract",
			store:     &fakeStore{info: withContract(bdInfoFork, 2)},
			wantParts: []string{"contract 2", "known: 1"},
		},
		{
			name:      "database behind bd",
			store:     &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) { return 65, nil }},
			wantParts: []string{"schema<=66", "database at 65"},
		},
		{
			name:      "bd and database past the level gastown's SQL reads declare",
			store:     &fakeStore{info: withSchemaCeiling(bdInfoFork, 67), levelFn: func(int) (int, error) { return 67, nil }},
			wantParts: []string{"database is at schema 67", "written for 66", "beadsql.SchemaVersion"},
		},
		{
			name:      "database ahead of bd",
			store:     &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) { return 0, errors.New("the database schema is ahead of this bd (bd exit 26)") }},
			wantParts: []string{"database schema is ahead of this bd"},
		},
		{
			name: "database answer not JSON",
			store: &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) {
				return 0, errors.New(`could not read the database migration level: bd sql output is not a JSON row list: "No rows."`)
			}},
			wantParts: []string{"could not read the database migration level", "No rows."},
		},
		{
			name:      "version command fails",
			store:     &fakeStore{verErr: errors.New("bd version --json: exit status 2")},
			wantParts: []string{"bd version --json: exit status 2"},
		},
		{
			// The seam wraps the sentinel; the handshake must key on the
			// chain, not on the bare value.
			name:      "bd missing",
			store:     &fakeStore{verErr: fmt.Errorf("%w: exec: \"bd\": executable file not found in $PATH", beads.ErrNotInstalled)},
			wantParts: []string{"not found on PATH"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CheckBDHandshake(context.Background(), "/usr/local/bin/bd", tc.store)
			if err == nil {
				t.Fatal("want refusal, got nil")
			}
			if !errors.Is(err, ErrBDHandshake) {
				t.Errorf("refusal does not wrap ErrBDHandshake: %v", err)
			}
			msg := err.Error()
			wants := append(append([]string{}, tc.wantParts...), "/usr/local/bin/bd", "make safe-install", "contract 1")
			for _, part := range wants {
				if !strings.Contains(msg, part) {
					t.Errorf("refusal missing %q:\n%s", part, msg)
				}
			}
			if strings.Contains(msg, "go install") {
				t.Errorf("refusal must never suggest go install:\n%s", msg)
			}
		})
	}
}

// TestCheckBDHandshake_NoDatabaseReadAfterAVersionRefusal: the database is
// read only once the report says this bd may serve it.
func TestCheckBDHandshake_NoDatabaseReadAfterAVersionRefusal(t *testing.T) {
	t.Parallel()
	for _, info := range []beads.VersionInfo{bdInfoPreD1, bdInfoUpstream} {
		s := &fakeStore{info: info}
		if _, err := CheckBDHandshake(context.Background(), "/bin/bd", s); err == nil {
			t.Fatalf("CheckBDHandshake(%+v): want refusal", info)
		}
		if s.reads != 0 {
			t.Errorf("the database was read %d times after a version refusal", s.reads)
		}
	}
}

func withContract(info beads.VersionInfo, contract int) beads.VersionInfo {
	info.ContractVersion = contract
	return info
}

func withSchemaCeiling(info beads.VersionInfo, ceiling int) beads.VersionInfo {
	info.DBSchemaVersion = ceiling
	return info
}

// TestCheckBDHandshake_WaitsOutAColdStore is the gt-2gctr regression for the
// cold boot: while the store is unavailable the handshake waits and tries
// again, and passes once Dolt answers.
func TestCheckBDHandshake_WaitsOutAColdStore(t *testing.T) {
	t.Parallel()
	s := &fakeStore{info: bdInfoFork, levelFn: func(reads int) (int, error) {
		if reads <= 3 {
			return 0, fmt.Errorf("%w: bd exit 25: store unavailable", beads.ErrStoreUnavailable)
		}
		return 66, nil
	}}
	hs, err := checkBDHandshake(context.Background(), "/bin/bd", s, storeRetryNow)
	if err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
	if hs.DBSchema != 66 || hs.Found.BuildID != "da4983e" {
		t.Fatalf("handshake = %+v", hs)
	}
	if s.reads != 4 {
		t.Errorf("schema reads = %d, want 4: three store_unavailable failures, then the pass", s.reads)
	}
}

// forkReportJSON is a fork build's `bd version --json` report, so a store over
// an injected runner parses what bd would print.
const forkReportJSON = `{"build":"da4983e","build_id":"da4983e","commit":"da4983e","contract_version":1,"db_schema_version":66,"version":"1.2.2"}`

// exitStatus is a bd failure carrying bd's typed exit status, as
// *exec.ExitError does.
type exitStatus int

func (e exitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

func (e exitStatus) ExitCode() int { return int(e) }

// storeAnswer is a bd the store runs through beads.WithRunner. It answers the
// --allow-stale probe as a bd that lacks the flag, the version read with the
// fork report, and any other call with sqlOut, sqlStderr and sqlErr.
func storeAnswer(sqlOut string, sqlStderr string, sqlErr error) func(context.Context, []string) ([]byte, []byte, error) {
	return func(_ context.Context, args []string) ([]byte, []byte, error) {
		switch {
		case len(args) == 2 && args[0] == "--allow-stale":
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		case args[0] == "version":
			return []byte(forkReportJSON), nil, nil
		case args[0] == "sql":
			return []byte(sqlOut), []byte(sqlStderr), sqlErr
		}
		return nil, nil, fmt.Errorf("unexpected bd call %v", args)
	}
}

// TestCheckBDHandshake_JudgesWhatTheSeamProduces drives the handshake over a
// real store (beads.New over beads.WithRunner), so the seam parses bd's
// answers and classifies its failures and the handshake decides on those: a
// missing bd, a store that did not answer, a bd ahead of its database, a
// version read that failed, and a read the handshake's own budget ended are
// judged from bd's reports, not from a fake returning a sentinel.
func TestCheckBDHandshake_JudgesWhatTheSeamProduces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		answer   func(context.Context, []string) ([]byte, []byte, error)
		budget   time.Duration // 0 is the case's default
		want     []string
		wantDolt bool
	}{
		{
			name: "bd missing",
			answer: func(context.Context, []string) ([]byte, []byte, error) {
				return nil, nil, &exec.Error{Name: "bd", Err: exec.ErrNotFound}
			},
			want: []string{"bd not found on PATH"},
		},
		{
			name:     "store unavailable",
			answer:   storeAnswer("", "store unavailable", exitStatus(25)),
			want:     []string{"Dolt is not reachable", "gt dolt start", "store unavailable"},
			wantDolt: true,
		},
		{
			name:   "database ahead of bd",
			answer: storeAnswer("", "schema skew", exitStatus(26)),
			want:   []string{"database schema is ahead of this bd", "make safe-install"},
		},
		{
			name:   "version read fails",
			answer: storeAnswer("", "bd: boom", exitStatus(2)),
			want:   []string{"boom", "make safe-install"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			budget := tc.budget
			if budget == 0 {
				// A store that stays unavailable is retried until the
				// budget ends, so every case carries one.
				budget = 50 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			// The store carries the handshake's context, as the production
			// stores do, so its budget bounds the bd calls themselves.
			store := beads.New(t.TempDir(), beads.WithRunner(tc.answer), beads.WithCallContext(ctx))
			_, err := checkBDHandshake(ctx, "/usr/local/bin/bd", store, storeRetryNow)
			if err == nil {
				t.Fatal("want refusal, got nil")
			}
			if !errors.Is(err, ErrBDHandshake) {
				t.Errorf("refusal does not wrap ErrBDHandshake: %v", err)
			}
			if got := errors.Is(err, ErrDoltUnreachable); got != tc.wantDolt {
				t.Errorf("errors.Is(err, ErrDoltUnreachable) = %v, want %v: %v", got, tc.wantDolt, err)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal lacks %q:\n%s", want, err)
				}
			}
		})
	}
}

// TestCheckBDHandshake_TheReadsCarryTheHandshakeBudget: the store built with
// the handshake's context runs its bd calls under that deadline, so the
// handshake budget bounds the calls themselves and not only the retry wait
// (gt-3lvv0).
func TestCheckBDHandshake_TheReadsCarryTheHandshakeBudget(t *testing.T) {
	t.Parallel()
	const budget = 5 * time.Second
	deadlines := make(chan time.Time, 2)
	answer := func(ctx context.Context, args []string) ([]byte, []byte, error) {
		switch {
		case len(args) == 2 && args[0] == "--allow-stale":
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		case args[0] == "version":
			return []byte(forkReportJSON), nil, nil
		}
		if deadline, ok := ctx.Deadline(); ok {
			select {
			case deadlines <- deadline:
			default:
			}
		}
		return nil, []byte("boom"), exitStatus(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	store := beads.New(t.TempDir(), beads.WithRunner(answer), beads.WithCallContext(ctx))
	if _, err := checkBDHandshake(ctx, "/usr/local/bin/bd", store, storeRetryNow); err == nil {
		t.Fatal("want refusal, got nil")
	}
	select {
	case deadline := <-deadlines:
		if until := time.Until(deadline); until > 2*budget {
			t.Errorf("the bd read's deadline is %s away, want it under the handshakes %s budget", until, budget)
		}
	default:
		t.Fatal("the bd read ran without the handshake context's deadline")
	}
}

// TestCheckBDHandshake_StoreDownNamesDolt is the other half of gt-2gctr: with
// the store still down when the budget runs out, the refusal names Dolt and
// the Dolt commands. The bd is fine, so nothing in it may point at the bd
// version or at reinstalling bd.
func TestCheckBDHandshake_StoreDownNamesDolt(t *testing.T) {
	t.Parallel()
	s := &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) {
		return 0, fmt.Errorf("%w: bd exit 25: store unavailable", beads.ErrStoreUnavailable)
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := checkBDHandshake(ctx, "/usr/local/bin/bd", s, storeRetryNow)
	if err == nil {
		t.Fatal("want refusal, got nil")
	}
	if !errors.Is(err, ErrBDHandshake) || !errors.Is(err, ErrDoltUnreachable) {
		t.Errorf("refusal must wrap ErrBDHandshake and ErrDoltUnreachable: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"Dolt is not reachable", "gt dolt status", "gt dolt start", "/usr/local/bin/bd", "store unavailable"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{"make safe-install", "required:", "contract 1", "1.2.2"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("refusal must not blame bd with %q:\n%s", unwanted, msg)
		}
	}
	if s.reads < 1 {
		t.Error("the handshake refused without reading the database")
	}
}

// TestCheckBDHandshake_DeadlineKillNamesDolt is the gt-3lvv0 regression: a
// read still waiting on Dolt when its deadline ends is the store's failure,
// and the refusal must name Dolt rather than send the operator to reinstall a
// bd whose version check passed.
func TestCheckBDHandshake_DeadlineKillNamesDolt(t *testing.T) {
	t.Parallel()
	s := &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) {
		return 0, fmt.Errorf("%w: the read did not finish: %w", beads.ErrStoreUnavailable, context.DeadlineExceeded)
	}}
	// The store never stops being unavailable, so the handshake's own budget
	// is what ends the retries.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := checkBDHandshake(ctx, "/usr/local/bin/bd", s, storeRetryNow)
	if err == nil {
		t.Fatal("want refusal, got nil")
	}
	if !errors.Is(err, ErrBDHandshake) || !errors.Is(err, ErrDoltUnreachable) {
		t.Errorf("refusal must wrap ErrBDHandshake and ErrDoltUnreachable: %v", err)
	}
	msg := err.Error()
	for _, want := range []string{"Dolt is not reachable", "gt dolt status", "gt dolt start", "deadline exceeded"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal lacks %q:\n%s", want, msg)
		}
	}
	for _, unwanted := range []string{"make safe-install", "required:", "could not read the database migration level"} {
		if strings.Contains(msg, unwanted) {
			t.Errorf("refusal must not blame bd with %q:\n%s", unwanted, msg)
		}
	}
}

// TestCheckBDHandshake_BDFailureBeforeDeadlineBlamesBD: a store failure that
// is not the store being unavailable is bd's own answer and keeps the bd
// refusal.
func TestCheckBDHandshake_BDFailureBeforeDeadlineBlamesBD(t *testing.T) {
	t.Parallel()
	s := &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) {
		return 0, errors.New("could not read the database migration level: bd sql: boom")
	}}
	_, err := checkBDHandshake(context.Background(), "/usr/local/bin/bd", s, storeRetryNow)
	if err == nil {
		t.Fatal("want refusal, got nil")
	}
	if !errors.Is(err, ErrBDHandshake) || errors.Is(err, ErrDoltUnreachable) {
		t.Errorf("refusal must wrap ErrBDHandshake and not ErrDoltUnreachable: %v", err)
	}
	for _, want := range []string{"could not read the database migration level", "make safe-install"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	if s.reads != 1 {
		t.Errorf("schema reads = %d, want 1: a bd failure is not retried", s.reads)
	}
}

// TestWaitForStoreRetryEndsWithTheBudget pins the bound on the retry loop: an
// ended context stops it, and one with budget left lets the next attempt run.
func TestWaitForStoreRetryEndsWithTheBudget(t *testing.T) {
	t.Parallel()
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if waitForStoreRetry(ended, time.Second) {
		t.Error("waitForStoreRetry let the loop continue after the context ended")
	}
	if !waitForStoreRetry(context.Background(), time.Millisecond) {
		t.Error("waitForStoreRetry stopped a loop with budget left")
	}
}

// TestCheckBDHandshake_OnlyStoreUnavailableIsRetried: bd's other failures are
// bd answering, so they refuse on the first read (gt-2gctr).
func TestCheckBDHandshake_OnlyStoreUnavailableIsRetried(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"schema skew", errors.New("the database schema is ahead of this bd (bd exit 26)")},
		{"other bd failure", errors.New("could not read the database migration level: bd sql: boom")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &fakeStore{info: bdInfoFork, levelFn: func(int) (int, error) { return 0, tc.err }}
			if _, err := checkBDHandshake(context.Background(), "/bin/bd", s, storeRetryNow); err == nil {
				t.Fatal("want refusal, got nil")
			}
			if s.reads != 1 {
				t.Errorf("schema reads = %d, want 1: only store_unavailable is retried", s.reads)
			}
		})
	}
}

// TestBdCommandTreeSnapshotMatchesContract ties the command-tree lint's bd
// snapshot (internal/cmdtree/bd-command-tree.json) to the handshake: the
// snapshot must come from a bd whose contract is the newest one this gt
// accepts. Adding a contract version here without refreshing the snapshot
// (make bd-command-tree), or refreshing it from a bd on a contract the
// handshake would refuse, fails.
func TestBdCommandTreeSnapshotMatchesContract(t *testing.T) {
	t.Parallel()
	_, snap, err := cmdtree.LoadBdTree()
	if err != nil {
		t.Fatalf("LoadBdTree: %v", err)
	}
	newest := 0
	for _, v := range KnownBDContractVersions {
		newest = max(newest, v)
	}
	if snap.ContractVersion != newest {
		t.Fatalf("bd-command-tree.json is from %s at contract_version %d; the handshake's newest known contract is %d (KnownBDContractVersions %v): refresh the snapshot with make bd-command-tree, or fix KnownBDContractVersions",
			snap.Source, snap.ContractVersion, newest, KnownBDContractVersions)
	}
}
