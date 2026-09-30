package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"

	beadsdk "github.com/steveyegge/beads"
	"github.com/steveyegge/gastown/internal/beads"
)

// TestSchemaLevelProblem is the guard's verdict without a database: equal
// levels pass, anything else (including a read failure or an unknown bd
// level) is a problem naming both integers (B5-02).
func TestSchemaLevelProblem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		level   int
		readErr error
		want    int
		problem string
	}{
		{"equal", 66, nil, 66, ""},
		{"database ahead", 67, nil, 66, "database schema 67 does not match bd schema 66"},
		{"database behind", 60, nil, 66, "database schema 60 does not match bd schema 66"},
		{"read failed", 0, errors.New("table not found: schema_migrations"), 66, "cannot read schema_migrations: table not found: schema_migrations"},
		{"no bd level", 66, nil, 0, "bd schema level unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := schemaLevelProblem(tc.level, tc.readErr, tc.want)
			if tc.problem == "" && got != "" || tc.problem != "" && !strings.Contains(got, tc.problem) {
				t.Fatalf("schemaLevelProblem(%d, %v, %d) = %q, want %q", tc.level, tc.readErr, tc.want, got, tc.problem)
			}
		})
	}
}

// fakeStoreProbe answers the compatibility reads for one store.
type fakeStoreProbe struct {
	level      int
	levelErr   error
	tailErr    error
	journal    string
	journalErr error
}

func (f *fakeStoreProbe) SchemaLevel(context.Context) (int, error) { return f.level, f.levelErr }
func (f *fakeStoreProbe) EventsTail(int64, int) (*beads.EventsPage, error) {
	if f.tailErr != nil {
		return nil, f.tailErr
	}
	return &beads.EventsPage{}, nil
}
func (f *fakeStoreProbe) JournalConfig() (string, error) { return f.journal, f.journalErr }

// probesFor serves each store name's fake.
func probesFor(fakes map[string]*fakeStoreProbe) func(string, string) (storeProbe, error) {
	return func(_, name string) (storeProbe, error) {
		f, ok := fakes[name]
		if !ok {
			return nil, errors.New("no probe for " + name)
		}
		return f, nil
	}
}

// The compat check reads every store through bd (gt-7iwy0.2): the schema
// level from schema_migrations via bd sql, the events journal via a
// one-record tail, and the journal setting via bd config get. A level other
// than bd's, a failed read, or a journal bd cannot tail refuses; a journal
// switched off in config.yaml is only reported, because gastown's own bd
// calls journal regardless (BD_EVENTS_JOURNAL=1).
func TestCheckBeadsStoreCompatibility_ReadsThroughBD(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		probe   fakeStoreProbe
		problem string
		warning string
	}{
		{"compatible", fakeStoreProbe{level: 66, journal: "true"}, "", ""},
		{"database ahead", fakeStoreProbe{level: 67, journal: "true"}, "database schema 67 does not match bd schema 66", ""},
		{"schema read fails", fakeStoreProbe{levelErr: errors.New("the database schema is ahead of this bd (bd exit 26)"), journal: "true"}, "cannot read schema_migrations: the database schema is ahead", ""},
		{"journal unreadable", fakeStoreProbe{level: 66, tailErr: errors.New("bd events tail: store_unavailable"), journal: "true"}, "events journal probe failed: bd events tail: store_unavailable", ""},
		{"journal off in config", fakeStoreProbe{level: 66, journal: "false"}, "", "events journal is off in its config.yaml"},
		{"journal config unreadable", fakeStoreProbe{level: 66, journalErr: errors.New("exit 1")}, "", "cannot read events-journal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			probe := tc.probe
			var warnings []string
			err := checkBeadsStoreCompatibility(context.Background(), t.TempDir(), []string{"gastown"}, 66,
				probesFor(map[string]*fakeStoreProbe{"gastown": &probe}), func(w string) { warnings = append(warnings, w) })
			switch {
			case tc.problem == "" && err != nil:
				t.Fatalf("unexpected refusal: %v", err)
			case tc.problem != "" && (err == nil || !strings.Contains(err.Error(), tc.problem)):
				t.Fatalf("err = %v, want a refusal naming %q", err, tc.problem)
			}
			joined := strings.Join(warnings, "\n")
			if tc.warning == "" && joined != "" || tc.warning != "" && !strings.Contains(joined, tc.warning) {
				t.Errorf("warnings = %q, want %q", warnings, tc.warning)
			}
			if tc.warning != "" && !strings.Contains(joined, "bd config set events-journal true") {
				t.Errorf("warning %q does not say how to turn the journal on", joined)
			}
		})
	}
}

// closeRecorder is a store that only records Close; the guard must refuse
// before it reads anything from a store.
type closeRecorder struct {
	beadsdk.Storage
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// TestVerifyBeadsStoresBlocksOnBDSchemaLevel drives the startup gate through
// the bdSchemaLevel seam: a failed read and a bd that reports no schema level
// (a pre-machine-surface build) each block startup and close every store.
func TestVerifyBeadsStoresBlocksOnBDSchemaLevel(t *testing.T) {
	old := bdSchemaLevel
	t.Cleanup(func() { bdSchemaLevel = old })
	for _, tc := range []struct {
		name  string
		level int
		err   error
		want  string
	}{
		{"read fails", 0, errors.New("bd version --json: exit status 1"), "cannot read bd's schema level: bd version --json: exit status 1"},
		{"no level reported", 0, nil, "reports no db_schema_version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bdSchemaLevel = func(context.Context, string) (int, error) { return tc.level, tc.err }
			store := &closeRecorder{}
			err := verifyBeadsStores(context.Background(), nil, t.TempDir(), map[string]beadsdk.Storage{"hq": store}, probesFor(nil))
			if err == nil || !strings.Contains(err.Error(), "daemon startup blocked") || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("verifyBeadsStores = %v, want a startup block naming %q", err, tc.want)
			}
			if !store.closed {
				t.Error("stores were not closed on refusal")
			}
		})
	}
}

func TestParseConfigGetValue(t *testing.T) {
	t.Parallel()
	for in, want := range map[string]string{
		`{"schema_version":1,"contract_version":1,"data":{"key":"events-journal","location":"config.yaml","value":"false"},"pagination":null,"error":null}`: "false",
		`{"key":"events-journal","location":"env var","value":"true"}`:                                                                                       "true",
	} {
		if got, err := parseConfigGetValue([]byte(in)); err != nil || got != want {
			t.Errorf("parseConfigGetValue(%s) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "events-journal (not set)", `{"schema_version":1,"contract_version":1,"data":null,"error":{"kind":"internal"}}`} {
		if got, err := parseConfigGetValue([]byte(bad)); err == nil {
			t.Errorf("parseConfigGetValue(%q) = %q, want an error", bad, got)
		}
	}
}

// exitStatus is a scripted process failure carrying an exit code.
type exitStatus int

func (e exitStatus) Error() string { return "exit status " + itoa(int(e)) }
func (e exitStatus) ExitCode() int { return int(e) }

// The real probe, driven through recorded runners: every read is pinned to
// the store's directory with gastown's journal override cleared, runs in
// machine mode, and maps bd's typed failures (gt-7iwy0.2).
func TestBDStoreProbe_ReadsThroughPinnedBD(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var envs [][]string
	run := func(_ context.Context, extraEnv []string, args ...string) ([]byte, []byte, error) {
		envs = append(envs, extraEnv)
		switch args[0] {
		case "sql":
			return []byte(`{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"schema_skew","message":"ahead"}}`), nil, exitStatus(26)
		case "config":
			return []byte(`{"schema_version":1,"contract_version":1,"data":{"key":"events-journal","location":"config.yaml","value":"false"},"pagination":null,"error":null}`), nil, nil
		}
		return nil, nil, errors.New("unexpected " + strings.Join(args, " "))
	}
	tail := func(_ context.Context, c beads.BDCall) ([]byte, []byte, error) {
		return []byte(`{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"store_unavailable","message":"no store"}}`), nil, exitStatus(25)
	}
	p := &bdStoreProbe{dir: dir, run: run, client: beads.NewWithBeadsDirAndRunner(dir, dir, tail)}

	if _, err := p.SchemaLevel(context.Background()); err == nil || !strings.Contains(err.Error(), "ahead of this bd") {
		t.Errorf("SchemaLevel on exit 26 = %v, want the schema-ahead refusal", err)
	}
	if v, err := p.JournalConfig(); err != nil || v != "false" {
		t.Errorf("JournalConfig = %q, %v; want false", v, err)
	}
	if _, err := p.EventsTail(0, 1); err == nil {
		t.Error("EventsTail on exit 25 succeeded")
	}
	for _, env := range envs {
		joined := strings.Join(env, " ")
		for _, want := range []string{"BEADS_DIR=" + dir, "BD_EVENTS_JOURNAL=", "BD_MACHINE=1"} {
			if !strings.Contains(joined, want) {
				t.Errorf("probe env %q lacks %q", env, want)
			}
		}
	}

	// A journal pruned below seq 1 is readable, not broken.
	probe := fakeStoreProbe{level: 66, journal: "true", tailErr: &beads.EventsTruncatedError{Floor: 50, Head: 90}}
	err := checkBeadsStoreCompatibility(context.Background(), dir, []string{"hq"}, 66,
		probesFor(map[string]*fakeStoreProbe{"hq": &probe}), nil)
	if err != nil {
		t.Errorf("a pruned journal refused startup: %v", err)
	}
}
