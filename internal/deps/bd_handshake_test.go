package deps

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/cmdtree"
)

// fakeExit is a bd exit status as a BDRunner reports it.
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

// Payloads captured from real builds on 2026-09-29.
const (
	// The town's installed fork build before the D1 machine surface.
	bdVersion537accb = `{"build":"537accb","commit":"537accbea9ca6f2be8d478ac97c5137bce4ac1b2","schema_version":1,"version":"1.2.2"}`
	// The fork build at beads da4983e.
	bdVersionDa4983e = `{"build":"da4983e","build_id":"da4983e","commit":"da4983e","contract_version":1,"db_schema_version":66,"schema_ceiling":{"ignored":25,"main":66},"schema_version":1,"version":"1.2.2"}`
	// The same payload under BD_MACHINE=1.
	bdVersionDa4983eEnvelope = `{"schema_version":1,"contract_version":1,"data":` + bdVersionDa4983e + `,"pagination":null,"error":null}`
	// Upstream steveyegge/beads: a semver and nothing else.
	bdVersionUpstream = `{"version":"1.0.5","build":"release"}`
)

type fakeBD struct {
	version    string
	versionErr error
	sqlOut     string
	sqlErr     error
	sqlStderr  string
	calls      [][]string
	envs       [][]string
}

func (f *fakeBD) run(_ context.Context, env []string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	switch args[0] {
	case "version":
		return []byte(f.version), nil, f.versionErr
	case "sql":
		return []byte(f.sqlOut), []byte(f.sqlStderr), f.sqlErr
	}
	return nil, nil, fmt.Errorf("unexpected bd call %v", args)
}

func dbLevel(n int) string { return fmt.Sprintf(`[{"version": %d}]`, n) }

func TestParseBDVersionJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want BDVersionInfo
	}{
		{"fork da4983e", bdVersionDa4983e, BDVersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}},
		{"machine envelope", bdVersionDa4983eEnvelope, BDVersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}},
		{"installed 537accb", bdVersion537accb, BDVersionInfo{Version: "1.2.2", Build: "537accb", Commit: "537accbea9ca6f2be8d478ac97c5137bce4ac1b2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBDVersionJSON([]byte(tc.out))
			if err != nil {
				t.Fatalf("ParseBDVersionJSON: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "bd version 1.2.2 (537accb)", "[]", `{"version":""}`} {
		if _, err := ParseBDVersionJSON([]byte(bad)); err == nil {
			t.Errorf("ParseBDVersionJSON(%q): want error", bad)
		}
	}
}

func TestCheckBDHandshake_AcceptsForkBuildAtDBLevel(t *testing.T) {
	t.Parallel()
	f := &fakeBD{version: bdVersionDa4983e, sqlOut: dbLevel(66)}
	hs, err := CheckBDHandshake(context.Background(), "/bin/bd", f.run)
	if err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
	if hs.Found.BuildID != "da4983e" || hs.DBSchema != 66 || hs.Found.ContractVersion != 1 {
		t.Fatalf("handshake = %+v", hs)
	}
	sql := f.calls[1]
	if sql[0] != "sql" || !strings.Contains(strings.Join(sql, " "), "schema_migrations") {
		t.Fatalf("DB level not read through bd sql: %v", f.calls)
	}
	if !containsEnv(f.envs[1], "BD_MACHINE=1") {
		t.Fatalf("bd sql must run in machine mode so failures carry a typed exit: env %v", f.envs[1])
	}
}

func TestCheckBDHandshake_DBLevelAsString(t *testing.T) {
	t.Parallel()
	f := &fakeBD{version: bdVersionDa4983e, sqlOut: `{"schema_version":1,"contract_version":1,"data":[{"version":"66"}],"pagination":null,"error":null}`}
	if _, err := CheckBDHandshake(context.Background(), "/bin/bd", f.run); err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
}

func TestCheckBDHandshake_Refusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		bd        *fakeBD
		wantParts []string
		noDBRead  bool
	}{
		{
			name:      "installed pre-D1 fork build",
			bd:        &fakeBD{version: bdVersion537accb},
			wantParts: []string{"537accb", "no contract_version", "no db_schema_version"},
			noDBRead:  true,
		},
		{
			name:      "upstream build",
			bd:        &fakeBD{version: bdVersionUpstream},
			wantParts: []string{"1.0.5", "no build_id"},
			noDBRead:  true,
		},
		{
			name:      "unknown contract",
			bd:        &fakeBD{version: strings.Replace(bdVersionDa4983e, `"contract_version":1`, `"contract_version":2`, 1)},
			wantParts: []string{"contract 2", "known: 1"},
			noDBRead:  true,
		},
		{
			name:      "database behind bd",
			bd:        &fakeBD{version: bdVersionDa4983e, sqlOut: dbLevel(65)},
			wantParts: []string{"schema<=66", "database at 65"},
		},
		{
			name:      "bd and database past the level gastown's SQL reads declare",
			bd:        &fakeBD{version: strings.Replace(bdVersionDa4983e, `"db_schema_version":66`, `"db_schema_version":67`, 1), sqlOut: dbLevel(67)},
			wantParts: []string{"database is at schema 67", "written for 66", "beadsql.SchemaVersion"},
		},
		{
			name:      "database ahead of bd",
			bd:        &fakeBD{version: bdVersionDa4983e, sqlErr: fakeExit(26), sqlStderr: "schema skew"},
			wantParts: []string{"database schema is ahead of this bd"},
		},
		{
			name:      "database answer not JSON",
			bd:        &fakeBD{version: bdVersionDa4983e, sqlOut: "No rows."},
			wantParts: []string{"could not read the database migration level", "No rows."},
		},
		{
			name:      "version command fails",
			bd:        &fakeBD{versionErr: fakeExit(2)},
			wantParts: []string{"bd version --json failed"},
			noDBRead:  true,
		},
		{
			name:      "bd missing",
			bd:        &fakeBD{versionErr: &exec.Error{Name: "bd", Err: exec.ErrNotFound}},
			wantParts: []string{"not found on PATH"},
			noDBRead:  true,
		},
		{
			name:      "version text only",
			bd:        &fakeBD{version: "bd version 0.57.0"},
			wantParts: []string{"not JSON"},
			noDBRead:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CheckBDHandshake(context.Background(), "/usr/local/bin/bd", tc.bd.run)
			if err == nil {
				t.Fatal("want refusal, got nil")
			}
			if !errors.Is(err, ErrBDHandshake) {
				t.Errorf("refusal does not wrap ErrBDHandshake: %v", err)
			}
			msg := err.Error()
			for _, part := range append(tc.wantParts, "/usr/local/bin/bd", "make safe-install", "contract 1") {
				if !strings.Contains(msg, part) {
					t.Errorf("refusal missing %q:\n%s", part, msg)
				}
			}
			if strings.Contains(msg, "go install") {
				t.Errorf("refusal must never suggest go install:\n%s", msg)
			}
			if tc.noDBRead && len(tc.bd.calls) > 1 {
				t.Errorf("database was read after the version was refused: %v", tc.bd.calls)
			}
		})
	}
}

// flakyStoreBD is a bd whose store is unavailable for the first downReads
// schema reads and answers after that: the shape of a cold boot, where the
// daemon starts before Dolt accepts connections (gt-2gctr).
type flakyStoreBD struct {
	*fakeBD
	downReads int
	reads     int
}

func (f *flakyStoreBD) run(ctx context.Context, env []string, args ...string) ([]byte, []byte, error) {
	if args[0] == "sql" {
		f.reads++
		if f.reads <= f.downReads {
			f.calls = append(f.calls, args)
			return nil, []byte("store unavailable"), fakeExit(bdStoreUnavailableExit)
		}
	}
	return f.fakeBD.run(ctx, env, args...)
}

// storeRetryNow is the retry interval the handshake tests run with: the
// checks are the loop, not the wait, so they drive it as fast as they can.
const storeRetryNow = time.Millisecond

// TestCheckBDHandshake_WaitsOutAColdStore is the gt-2gctr regression for the
// cold boot: while bd reports the store unavailable the handshake waits and
// tries again, and passes once Dolt answers.
func TestCheckBDHandshake_WaitsOutAColdStore(t *testing.T) {
	t.Parallel()
	f := &flakyStoreBD{fakeBD: &fakeBD{version: bdVersionDa4983e, sqlOut: dbLevel(66)}, downReads: 3}
	hs, err := checkBDHandshake(context.Background(), "/bin/bd", f.run, storeRetryNow)
	if err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
	if hs.DBSchema != 66 || hs.Found.BuildID != "da4983e" {
		t.Fatalf("handshake = %+v", hs)
	}
	if f.reads != 4 {
		t.Errorf("schema reads = %d, want 4: three store_unavailable failures, then the pass", f.reads)
	}
}

// TestCheckBDHandshake_StoreDownNamesDolt is the other half of gt-2gctr: with
// the store still down when the budget runs out, the refusal names Dolt and
// the Dolt commands. The bd is fine, so nothing in it may point at the bd
// version or at reinstalling bd.
func TestCheckBDHandshake_StoreDownNamesDolt(t *testing.T) {
	t.Parallel()
	f := &flakyStoreBD{fakeBD: &fakeBD{version: bdVersionDa4983e}, downReads: 1 << 30}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := checkBDHandshake(ctx, "/usr/local/bin/bd", f.run, storeRetryNow)
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
	if f.reads < 1 {
		t.Error("the handshake refused without reading the database")
	}
}

// TestCheckBDHandshake_DeadlineKillNamesDolt is the gt-3lvv0 regression: a bd
// sql still waiting on Dolt when the budget ends is killed by the context, and
// exec reports that as exit -1. The refusal must name Dolt, not send the
// operator to reinstall a bd whose version check passed.
func TestCheckBDHandshake_DeadlineKillNamesDolt(t *testing.T) {
	t.Parallel()
	f := &fakeBD{version: bdVersionDa4983e}
	hung := func(ctx context.Context, env []string, args ...string) ([]byte, []byte, error) {
		if args[0] != "sql" {
			return f.run(ctx, env, args...)
		}
		<-ctx.Done()
		return nil, []byte("signal: killed"), fakeExit(-1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := checkBDHandshake(ctx, "/usr/local/bin/bd", hung, storeRetryNow)
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

// TestCheckBDHandshake_BDFailureBeforeDeadlineBlamesBD: the deadline reading
// of an exit applies only once the context has ended. A bd that exits non-zero
// with budget left is bd's own failure and keeps the bd refusal.
func TestCheckBDHandshake_BDFailureBeforeDeadlineBlamesBD(t *testing.T) {
	t.Parallel()
	f := &fakeBD{version: bdVersionDa4983e, sqlErr: fakeExit(1), sqlStderr: "boom"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	_, err := checkBDHandshake(ctx, "/usr/local/bin/bd", f.run, storeRetryNow)
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
	if len(f.calls) != 2 {
		t.Errorf("bd calls = %d, want 2 (version, one sql read): a bd failure is not retried", len(f.calls))
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
		name      string
		sqlErr    error
		sqlStderr string
	}{
		{"schema skew", fakeExit(bdSchemaSkewExit), "schema skew"},
		{"other bd failure", fakeExit(2), "boom"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeBD{version: bdVersionDa4983e, sqlErr: tc.sqlErr, sqlStderr: tc.sqlStderr}
			if _, err := checkBDHandshake(context.Background(), "/bin/bd", f.run, storeRetryNow); err == nil {
				t.Fatal("want refusal, got nil")
			}
			if len(f.calls) != 2 {
				t.Errorf("bd calls = %v, want one version read and one schema read", f.calls)
			}
		})
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
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

// TestBDRunnerEnvKeepsMachineModeForSQL is the regression for the `bd sql`
// exemption in the subprocess policy: beads.CommandContextWithEnv's
// machineEnvForCall strips BD_MACHINE from a sql call, so bdRunnerEnv's
// environment has to be applied over it or readDBSchemaLevel runs outside
// machine mode and loses bd's typed schema_skew exit.
func TestBDRunnerEnvKeepsMachineModeForSQL(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		extraEnv []string
	}{
		{"no extra env", nil},
		{"the schema read's own request", []string{"BD_MACHINE=1"}},
		{"routing the caller pinned", []string{"BEADS_DIR=/some/store", "BD_MACHINE=1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bdRunnerEnv(tc.extraEnv)
			if n := countEnvKey(env, "BD_MACHINE"); n != 1 {
				t.Errorf("bdRunnerEnv(%q) has %d BD_MACHINE entries, want exactly 1: %q", tc.extraEnv, n, env)
			}
			if !slices.Contains(env, "BD_MACHINE=1") {
				t.Errorf("bdRunnerEnv(%q) = %q, want BD_MACHINE=1 in it", tc.extraEnv, env)
			}
			for _, want := range tc.extraEnv {
				if !slices.Contains(env, want) {
					t.Errorf("bdRunnerEnv(%q) dropped %q", tc.extraEnv, want)
				}
			}
		})
	}
}

// countEnvKey counts the entries setting name, the way a child process sees
// duplicates.
func countEnvKey(env []string, name string) int {
	n := 0
	for _, e := range env {
		if key, _, _ := strings.Cut(e, "="); key == name {
			n++
		}
	}
	return n
}
