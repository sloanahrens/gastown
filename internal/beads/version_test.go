package beads

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

// bdVersionFork is a fork build's report, captured from a real build.
const bdVersionFork = `{"build":"da4983e","build_id":"da4983e","commit":"da4983e","contract_version":1,"db_schema_version":66,"schema_version":1,"version":"1.2.2"}`

// TestVersionAndSchemaLevelRunBdsOwnReads pins the argv of both reads and what
// the store makes of the answers: the startup handshake refuses a town on
// exactly these two facts.
func TestVersionAndSchemaLevelRunBdsOwnReads(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		switch args[0] {
		case "version":
			return reply{stdout: bdVersionFork}
		case "sql":
			return reply{stdout: `[{"version": 66}]`}
		}
		return reply{err: fmt.Errorf("unexpected bd call %v", args)}
	})
	b := newRecordedBeads(t.TempDir(), r)

	info, err := b.Version()
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if info != (VersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}) {
		t.Errorf("Version = %+v", info)
	}

	level, err := b.SchemaLevel()
	if err != nil {
		t.Fatalf("SchemaLevel: %v", err)
	}
	if level != 66 {
		t.Errorf("SchemaLevel = %d, want 66", level)
	}

	calls := r.calls()
	if len(calls) != 2 {
		t.Fatalf("bd calls = %v, want one version read and one sql read", r.argvs())
	}
	if got := strings.Join(calls[0].args, " "); got != "version --json" {
		t.Errorf("Version argv = %q, want \"version --json\"", got)
	}
	if got := strings.Join(calls[1].args[:2], " "); got != "sql --json" {
		t.Errorf("SchemaLevel argv = %q, want a \"sql --json\" read", strings.Join(calls[1].args, " "))
	}
	if !strings.Contains(calls[1].args[2], "schema_migrations") {
		t.Errorf("SchemaLevel query = %q, want the schema_migrations level", calls[1].args[2])
	}
	// Machine mode is what makes a schema-ahead database fail with bd's typed
	// schema_skew exit rather than prose.
	if _, ok := lastEnvValue(calls[1].env, "BD_MACHINE"); !ok {
		t.Errorf("SchemaLevel ran without BD_MACHINE: %v", calls[1].env)
	}
}

// TestSchemaLevelClassifiesAStoreThatDidNotAnswer is the cold boot's half of
// the handshake (gt-2gctr): bd's store_unavailable exit, and a read its
// deadline ended, are the store's failure and say so; bd's other failures do
// not.
func TestSchemaLevelClassifiesAStoreThatDidNotAnswer(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		rep           reply
		wantStore     bool
		wantSubstring string
	}{
		{"store unavailable", reply{stderr: "store unavailable", err: exitError{25}}, true, "database behind bd did not answer"},
		{"deadline", reply{err: fmt.Errorf("timed out after 60s: %w", context.DeadlineExceeded)}, true, "database behind bd did not answer"},
		{"schema skew", reply{stderr: "schema skew", err: exitError{26}}, false, "database schema is ahead of this bd"},
		{"bd failure", reply{stderr: "boom", err: exitError{2}}, false, "could not read the database migration level"},
		{"not JSON", reply{stdout: "No rows."}, false, "could not read the database migration level"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func(args []string) reply {
				if args[0] == "sql" {
					return tc.rep
				}
				return reply{stdout: bdVersionFork}
			})
			_, err := newRecordedBeads(t.TempDir(), r).SchemaLevel()
			if err == nil {
				t.Fatal("SchemaLevel: want an error")
			}
			if got := errors.Is(err, ErrStoreUnavailable); got != tc.wantStore {
				t.Errorf("errors.Is(err, ErrStoreUnavailable) = %v, want %v: %v", got, tc.wantStore, err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("error %q lacks %q", err, tc.wantSubstring)
			}
		})
	}
}

// TestSchemaLevelEndsWithTheCallersBudget is the handshake's budget on a bd
// call (gt-3lvv0): a read still waiting on the database when the store's
// context ends is killed, the kill is classified as the store not answering,
// and the error names the budget that ended it rather than the seam's.
func TestSchemaLevelEndsWithTheCallersBudget(t *testing.T) {
	t.Parallel()
	const budget = 20 * time.Millisecond
	readCtx := make(chan context.Context, 1)
	run := func(ctx context.Context, c bdCall) ([]byte, []byte, error) {
		if len(c.args) == 2 && c.args[0] == "--allow-stale" {
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		}
		readCtx <- ctx
		// A read that outlives the caller's budget is the failure under test,
		// so the runner caps its own wait: a hang guard for a call no deadline
		// ever ends, not the measurement.
		select {
		case <-ctx.Done():
		case <-time.After(2 * time.Second):
			return nil, nil, errors.New("the read outlived the caller's budget")
		}
		return nil, nil, errors.New("signal: killed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()
	callerDeadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("the test's own context has no deadline")
	}
	b := newBeads(beadsFields{workDir: t.TempDir(), isolated: true, noRoute: true, exec: run, callCtx: ctx, budget: time.Minute})

	_, err := b.SchemaLevel()
	if err == nil {
		t.Fatal("SchemaLevel: want an error")
	}
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("a deadline-killed read must wrap ErrStoreUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("error %q does not name the deadline that ended the read", err)
	}
	var readUnder context.Context
	select {
	case readUnder = <-readCtx:
	default:
		t.Fatal("the bd read never reached the seam")
	}
	if !errors.Is(readUnder.Err(), context.DeadlineExceeded) {
		t.Errorf("the read ran under a context the caller's deadline did not cancel: %v", readUnder.Err())
	}
	deadline, ok := readUnder.Deadline()
	if !ok {
		t.Fatal("the bd read ran with no deadline")
	}
	if !deadline.Equal(callerDeadline) {
		t.Errorf("the read's deadline is %s, want the caller's %s", deadline, callerDeadline)
	}
}

// TestSchemaLevelKeepsBdsAnswerAfterTheCallersBudgetEnded pins gt-p0ha3: a bd
// read that exited with a status answered, even when the caller's deadline had
// passed by the time the error was read, so its status classifies the failure.
// Only a call the deadline itself ended is stamped as a timeout — the last row
// keeps gt-824d's timeout text for that kill.
func TestSchemaLevelKeepsBdsAnswerAfterTheCallersBudgetEnded(t *testing.T) {
	t.Parallel()
	// The deadline has ended before the read starts, as in the gt-strle gate
	// log: bd's answer and the caller's deadline race, and the answer wins.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	for _, tc := range []struct {
		name          string
		err           error
		wantStore     bool
		wantSubstring string
	}{
		{"schema skew answered", exitError{26}, false, "the database schema is ahead of this bd"},
		{"other status answered", exitError{2}, false, "could not read the database migration level"},
		// The deadline killed bd: its process error is the *exec.ExitError a
		// signaled process leaves, an exit status with no code (exit -1), not
		// a bare error carrying no exit status at all (gt-824d).
		{"killed at the deadline", &exec.ExitError{}, true, "timed out after 1m0s"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRecorder(func([]string) reply { return reply{stderr: "bd said so", err: tc.err} })
			b := newBeads(beadsFields{workDir: t.TempDir(), isolated: true, noRoute: true, exec: r.exec, callCtx: expired, budget: time.Minute})
			_, err := b.SchemaLevel()
			if err == nil {
				t.Fatal("SchemaLevel: want an error")
			}
			if got := errors.Is(err, ErrStoreUnavailable); got != tc.wantStore {
				t.Errorf("errors.Is(err, ErrStoreUnavailable) = %v, want %v: %v", got, tc.wantStore, err)
			}
			if !strings.Contains(err.Error(), tc.wantSubstring) {
				t.Errorf("error %q lacks %q", err, tc.wantSubstring)
			}
			if !tc.wantStore && strings.Contains(err.Error(), "timed out after") {
				t.Errorf("an answered read must keep bd's answer, not a deadline kill: %v", err)
			}
		})
	}
}

// TestSchemaLevelClassifiesTheSeamBudgetKill: with no caller context, the
// seam's own subprocess budget ends a read waiting on the database, and that
// kill is the store not answering too.
func TestSchemaLevelClassifiesTheSeamBudgetKill(t *testing.T) {
	t.Parallel()
	run := func(ctx context.Context, c bdCall) ([]byte, []byte, error) {
		if len(c.args) == 2 && c.args[0] == "--allow-stale" {
			return nil, []byte("Error: unknown flag: --allow-stale\n"), nil
		}
		<-ctx.Done()
		return nil, nil, errors.New("signal: killed")
	}
	b := newBeads(beadsFields{workDir: t.TempDir(), isolated: true, noRoute: true, exec: run, budget: 20 * time.Millisecond})
	_, err := b.SchemaLevel()
	if err == nil {
		t.Fatal("SchemaLevel: want an error")
	}
	if !errors.Is(err, ErrStoreUnavailable) {
		t.Errorf("a deadline-killed read must wrap ErrStoreUnavailable: %v", err)
	}
	if !strings.Contains(err.Error(), "timed out after 20ms") {
		t.Errorf("error %q does not name the budget that ended the read", err)
	}
}

// TestPlainVersionNamesAMissingBd: a plain wrapper's failed exec is the seam's
// ErrNotInstalled, as the policy path's is, so a caller can tell bd not being
// installed from bd failing to answer.
func TestPlainVersionNamesAMissingBd(t *testing.T) {
	t.Parallel()
	missing := func(context.Context, []string) ([]byte, []byte, error) {
		return nil, nil, &exec.Error{Name: "bd", Err: exec.ErrNotFound}
	}
	b := NewPlain(t.TempDir(), nil, WithRunner(missing))
	_, err := b.Version()
	if err == nil {
		t.Fatal("Version with no bd: want an error")
	}
	if !errors.Is(err, ErrNotInstalled) {
		t.Errorf("a missing bd must wrap ErrNotInstalled: %v", err)
	}
	if errors.Is(err, ErrUnavailable) {
		t.Errorf("a missing bd is not an unavailable answer: %v", err)
	}
}

// TestVersionRefusesWhatBdDidNotAnswer: a failed read and a report that names
// no version are errors, never a zero VersionInfo a caller could judge as a
// real answer.
func TestVersionRefusesWhatBdDidNotAnswer(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply { return reply{stderr: "boom", err: exitError{2}} })
	if _, err := newRecordedBeads(t.TempDir(), r).Version(); err == nil {
		t.Error("Version of a failed read: want an error")
	}
	for _, out := range []string{"", "some other output", `{"commit":"2357327"}`, `{"version": 5}`} {
		r := newRecorder(func(args []string) reply { return reply{stdout: out} })
		if _, err := newRecordedBeads(t.TempDir(), r).Version(); err == nil {
			t.Errorf("Version(%q): want an error", out)
		}
	}
}

// TestSchemaLevelRefusesAQueryBeadsqlWillNotBuild: the statement comes from
// beadsql, so a query its guard refuses fails here rather than reaching bd as
// an empty argv.
func TestSchemaLevelRefusesAQueryBeadsqlWillNotBuild(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply { return reply{stdout: `[{"version": 66}]`} })
	b := newRecordedBeads(t.TempDir(), r)
	if _, err := b.schemaLevel(beadsql.TableProbe("not a table")); err == nil {
		t.Fatal("schemaLevel with an unusable table: want an error")
	}
	if got := r.argvs(); len(got) != 0 {
		t.Errorf("a query beadsql refused still reached bd: %v", got)
	}
}

func TestParseVersionReport(t *testing.T) {
	t.Parallel()
	fork := VersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}
	for _, tc := range []struct {
		name string
		out  string
		want VersionInfo
	}{
		{"fork build", bdVersionFork, fork},
		{"machine envelope", `{"schema_version":1,"contract_version":1,"data":` + bdVersionFork + `,"pagination":null,"error":null}`, fork},
		{"pre-machine fork build", `{"build":"537accb","commit":"537accb","schema_version":1,"version":"1.2.2"}`,
			VersionInfo{Version: "1.2.2", Build: "537accb", Commit: "537accb"}},
		{"upstream", `{"version":"1.0.5","build":"release"}`, VersionInfo{Version: "1.0.5", Build: "release"}},
		{"text from a bd before machine mode", "bd version 0.52.0 (dev: main@3e1378e)", VersionInfo{Version: "0.52.0"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseVersionReport([]byte(tc.out))
			if err != nil {
				t.Fatalf("parseVersionReport(%q): %v", tc.out, err)
			}
			if got != tc.want {
				t.Errorf("parseVersionReport(%q) = %+v, want %+v", tc.out, got, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "garbage", `{"commit":"2357327"}`, `{"version":""}`} {
		if _, err := parseVersionReport([]byte(bad)); err == nil {
			t.Errorf("parseVersionReport(%q): want error", bad)
		}
	}
}

func TestParseSchemaLevelReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want int
	}{
		{"number", `[{"version": 66}]`, 66},
		{"string", `[{"version": "66"}]`, 66},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := parseSchemaLevel([]byte(tc.out))
			if err != nil || got != tc.want {
				t.Errorf("parseSchemaLevel(%q) = %d, %v; want %d", tc.out, got, err, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "No rows.", `[]`, `[{"version": 0}]`, `[{"version": null}]`, `[{"version": 1}, {"version": 2}]`} {
		if _, err := parseSchemaLevel([]byte(bad)); err == nil {
			t.Errorf("parseSchemaLevel(%q): want error", bad)
		}
	}
}
