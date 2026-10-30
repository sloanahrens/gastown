package deps

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/cmdtree"
	"github.com/steveyegge/gastown/tracker/api"
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

// fakeClock is the handshake's clock in a test: a budget that lasts the retry
// waits the case passes and then ends, so a case that runs out of budget runs
// out on the retry the clock ends and never on how fast the machine answered
// (gt-3hcp9).
type fakeClock struct {
	done   chan struct{}
	passes int   // retry waits that pass before the budget ends
	waits  int   // retry waits asked for so far
	err    error // what Err reports once the budget ended
}

// newFakeClock returns a clock whose budget ends on the wait after passes waits
// have passed.
func newFakeClock(passes int) *fakeClock {
	return &fakeClock{done: make(chan struct{}), passes: passes, err: context.DeadlineExceeded}
}

func (c *fakeClock) Done() <-chan struct{} { return c.done }

func (c *fakeClock) Err() error {
	select {
	case <-c.done:
		return c.err
	default:
		return nil
	}
}

// After passes the waits the clock has budget for and ends the budget on the
// first one it does not: the channel it returns then never receives, so the
// retry loop takes its Done arm instead of waiting it out.
func (c *fakeClock) After(time.Duration) <-chan time.Time {
	c.waits++
	if c.waits > c.passes {
		c.end()
		return make(chan time.Time)
	}
	return elapsed
}

// end ends the budget at once, for a case that fires it while a read waits.
func (c *fakeClock) end() {
	select {
	case <-c.done:
	default:
		close(c.done)
	}
}

// elapsed is a channel already past: a fake clock's retry wait that passed.
var elapsed = func() <-chan time.Time {
	c := make(chan time.Time)
	close(c)
	return c
}()

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
// judged from bd's reports, not from a fake returning a sentinel. Each case
// carries the clock its budget comes from, so no case waits on a wall-clock
// deadline (gt-3hcp9).
func TestCheckBDHandshake_JudgesWhatTheSeamProduces(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		answer   func(context.Context, []string) ([]byte, []byte, error)
		clock    *fakeClock
		minReads int // database reads the case must have made; 0 is no bound
		want     []string
		wantDolt bool
	}{
		{
			name: "bd missing",
			answer: func(context.Context, []string) ([]byte, []byte, error) {
				return nil, nil, &exec.Error{Name: "bd", Err: exec.ErrNotFound}
			},
			clock: newFakeClock(0),
			want:  []string{"bd not found on PATH"},
		},
		{
			name:   "store unavailable",
			answer: storeAnswer("", "store unavailable", exitStatus(25)),
			// One retry wait passes, so the store is read again and the
			// clock ends the handshake on the wait after that.
			clock:    newFakeClock(1),
			minReads: 2,
			want:     []string{"Dolt is not reachable", "gt dolt start", "store unavailable"},
			wantDolt: true,
		},
		{
			name:   "database ahead of bd",
			answer: storeAnswer("", "schema skew", exitStatus(26)),
			clock:  newFakeClock(0),
			want:   []string{"database schema is ahead of this bd", "make safe-install"},
		},
		{
			// The version read itself fails, so no database read follows.
			name: "version read fails",
			answer: func(_ context.Context, args []string) ([]byte, []byte, error) {
				if len(args) == 2 && args[0] == "--allow-stale" {
					return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
				}
				return nil, []byte("bd: boom"), exitStatus(2)
			},
			clock: newFakeClock(0),
			want:  []string{"bd: boom", "make safe-install"},
		},
		{
			name:   "database read fails",
			answer: storeAnswer("", "bd sql: boom", exitStatus(2)),
			clock:  newFakeClock(0),
			want:   []string{"bd sql: boom", "make safe-install"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reads := 0
			answer := func(ctx context.Context, args []string) ([]byte, []byte, error) {
				if len(args) > 0 && args[0] == "sql" {
					reads++
				}
				return tc.answer(ctx, args)
			}
			store := beads.New(t.TempDir(), beads.WithRunner(answer))
			_, err := checkBDHandshakeClock("/usr/local/bin/bd", store, storeRetryNow, tc.clock)
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
			if reads < tc.minReads {
				t.Errorf("the database was read %d times, want at least %d: the budget ends the retry, not the first read", reads, tc.minReads)
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
	record := func(ctx context.Context) {
		if deadline, ok := ctx.Deadline(); ok {
			select {
			case deadlines <- deadline:
			default:
			}
		}
	}
	answer := func(ctx context.Context, args []string) ([]byte, []byte, error) {
		switch {
		case len(args) == 2 && args[0] == "--allow-stale":
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		case args[0] == "version":
			record(ctx)
			return []byte(forkReportJSON), nil, nil
		}
		record(ctx)
		return nil, []byte("boom"), exitStatus(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	store := NewHandshakeStore(ctx, t.TempDir(), beads.WithRunner(answer))
	if _, err := checkBDHandshake(ctx, "/usr/local/bin/bd", store, storeRetryNow); err == nil {
		t.Fatal("want refusal, got nil")
	}
	for _, read := range []string{"version", "database"} {
		select {
		case deadline := <-deadlines:
			if until := time.Until(deadline); until > 2*budget {
				t.Errorf("the %s read's deadline is %s away, want it under the handshakes %s budget", read, until, budget)
			}
		default:
			t.Fatalf("the %s read ran without the handshake context's deadline", read)
		}
	}
}

// TestNewHandshakeStoreReadsATownLayout: the store the startup gate and the
// doctor check build answers the handshake from a town root, asking bd for the
// version report and the database level in that order. Only the bd process is
// swapped, so the construction those two production callers share is what the
// case drives.
func TestNewHandshakeStoreReadsATownLayout(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls []string
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		// The seam probes --allow-stale before each read; the reads are what
		// this case is about.
		if len(args) > 1 && args[0] == "--allow-stale" {
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		}
		calls = append(calls, strings.Join(args, " "))
		switch args[0] {
		case "version":
			return []byte(forkReportJSON), nil, nil
		case "sql":
			return []byte(`[{"version": 66}]`), nil, nil
		}
		return nil, []byte("boom"), exitStatus(2)
	}
	store := NewHandshakeStore(context.Background(), townRoot, beads.WithRunner(answer))
	hs, err := CheckBDHandshake(context.Background(), "/usr/local/bin/bd", store)
	if err != nil {
		t.Fatalf("CheckBDHandshake over the handshake store: %v", err)
	}
	if hs.DBSchema != 66 {
		t.Errorf("DBSchema = %d, want 66", hs.DBSchema)
	}
	if len(calls) != 2 || !strings.HasPrefix(calls[0], "version --json") || !strings.HasPrefix(calls[1], "sql --json") {
		t.Errorf("bd calls = %q, want one version read then one sql read", calls)
	}
}

// TestCheckBDHandshake_VersionReadKilledByTheBudget: a version read the
// caller's budget ended is the budget expiring, not a bd the operator should
// reinstall, so the refusal names neither the install hint nor Dolt.
func TestCheckBDHandshake_VersionReadKilledByTheBudget(t *testing.T) {
	t.Parallel()
	clock := newFakeClock(0)
	reading := make(chan struct{})
	var once sync.Once
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		if len(args) == 2 && args[0] == "--allow-stale" {
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		}
		once.Do(func() { close(reading) })
		<-clock.Done()
		return nil, nil, fmt.Errorf("the version read did not finish: %w", clock.Err())
	}
	store := beads.New(t.TempDir(), beads.WithRunner(answer))
	var err error
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		_, err = checkBDHandshakeClock("/usr/local/bin/bd", store, storeRetryNow, clock)
	}()
	// The budget ends while the version read is still waiting on it.
	waitFor(reading)
	clock.end()
	waitFor(finished)
	if err == nil {
		t.Fatal("want refusal, got nil")
	}
	if !errors.Is(err, ErrBDHandshake) {
		t.Errorf("refusal does not wrap ErrBDHandshake: %v", err)
	}
	if errors.Is(err, ErrDoltUnreachable) {
		t.Errorf("a version read consults no database, so Dolt is not what failed: %v", err)
	}
	for _, want := range []string{"handshake budget ended before bd reported its version", "did not answer"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal lacks %q:\n%s", want, err)
		}
	}
	for _, unwanted := range []string{"make safe-install", "required:"} {
		if strings.Contains(err.Error(), unwanted) {
			t.Errorf("refusal must not blame the installed bd with %q:\n%s", unwanted, err)
		}
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
	_, err := checkBDHandshakeClock("/usr/local/bin/bd", s, storeRetryNow, newFakeClock(1))
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
	if s.reads < 2 {
		t.Errorf("schema reads = %d, want at least 2: the clock ends the retry, not the first read", s.reads)
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
	// The store never stops being unavailable, so the handshake's clock is
	// what ends the retries.
	_, err := checkBDHandshakeClock("/usr/local/bin/bd", s, storeRetryNow, newFakeClock(1))
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

// TestWaitForStoreRetryEndsWithTheBudget pins the bound on the retry loop: a
// clock whose budget has ended stops it, and one with budget left lets the next
// attempt run.
func TestWaitForStoreRetryEndsWithTheBudget(t *testing.T) {
	t.Parallel()
	ended := newFakeClock(0)
	ended.end()
	if waitForStoreRetry(ended, bdStoreRetryWait) {
		t.Error("waitForStoreRetry let the loop continue after the budget ended")
	}
	if !waitForStoreRetry(newFakeClock(1), bdStoreRetryWait) {
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

// waitFor waits for ch to close. The fakes close every channel deterministically,
// and a test that never closes one is ended by the gate's per-package -timeout
// (Makefile gate-test: the hang detector for the unit tier), not by a timer
// here: the no-sleep rule forbids time.After and the other timers in a unit test.
func waitFor(ch <-chan struct{}) {
	<-ch
}

// A real, already-expired deadline reaches the store through its call context.
// The deadline is in the past, so the outcome does not depend on how fast the
// machine is, and the refusal must still name the budget (gt-3hcp9 review round 2).
func TestCheckBDHandshake_DeadlineThroughTheStore(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	answer := func(c context.Context, args []string) ([]byte, []byte, error) {
		if len(args) == 2 && args[0] == "--allow-stale" {
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		}
		return nil, nil, fmt.Errorf("the version read did not finish: %w", c.Err())
	}
	store := beads.New(t.TempDir(), beads.WithRunner(answer), beads.WithCallContext(ctx))
	_, err := checkBDHandshake(ctx, "/usr/local/bin/bd", store, storeRetryNow)
	if err == nil {
		t.Fatal("want refusal, got nil")
	}
	if !errors.Is(err, ErrBDHandshake) {
		t.Errorf("refusal does not wrap ErrBDHandshake: %v", err)
	}
	if !strings.Contains(err.Error(), "handshake budget ended before bd reported its version") {
		t.Errorf("refusal does not name the budget:\n%s", err)
	}
}

// fakeTracker answers the tracker handshake's one read.
type fakeTracker struct {
	url   string
	level int
	err   error
}

func (f fakeTracker) URL() string { return f.url }

func (f fakeTracker) Readyz(context.Context) (int, error) { return f.level, f.err }

// TestCheckTrackerHandshake: a tracker at the schema level this gt speaks
// passes, and a tracker that did not answer ready or is at another level
// refuses, naming the tracker and its URL (gt-p85pw.110).
func TestCheckTrackerHandshake(t *testing.T) {
	t.Parallel()
	const url = "http://127.0.0.1:7433"
	for _, tc := range []struct {
		name    string
		tracker fakeTracker
		wants   []string
	}{
		{"ready at this gt's schema level", fakeTracker{url: url, level: api.SchemaLevel}, nil},
		{
			name:    "did not answer ready",
			tracker: fakeTracker{url: url, err: errors.New("the tracker did not answer: connection refused")},
			wants:   []string{"connection refused"},
		},
		{
			name:    "at another schema level",
			tracker: fakeTracker{url: url, level: api.SchemaLevel + 1},
			wants:   []string{"schema level", "api.SchemaLevel"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := CheckTrackerHandshake(context.Background(), tc.tracker)
			if tc.wants == nil {
				if err != nil {
					t.Fatalf("CheckTrackerHandshake = %v, want a pass", err)
				}
				return
			}
			if !errors.Is(err, ErrTrackerHandshake) {
				t.Fatalf("CheckTrackerHandshake = %v, want an ErrTrackerHandshake", err)
			}
			if !strings.Contains(err.Error(), url) || !strings.Contains(err.Error(), trackerFixHint) {
				t.Errorf("refusal does not name the tracker and its remedy:\n%s", err)
			}
			for _, want := range tc.wants {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("refusal does not say %q:\n%s", want, err)
				}
			}
		})
	}
}
