package metrics

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSnapshotAppendsExactlyOneParseableLine(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := envAt(fixedNow())
	env.Town = town
	rec, path, err := RunSnapshot(context.Background(), env, []Source{healthy("queue")}, noDeadline)
	if err != nil {
		t.Fatalf("RunSnapshot: %v", err)
	}
	if want := filepath.Join(town, ".runtime", "metrics", "snapshots.jsonl"); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want 1:\n%s", len(lines), data)
	}
	var got Record
	if err := json.Unmarshal([]byte(lines[0]), &got); err != nil {
		t.Fatalf("line does not parse: %v\n%s", err, lines[0])
	}
	if got.TS != rec.TS || got.Groups["queue"]["open"] != float64(3) {
		t.Fatalf("appended record = %+v, want the collected one %+v", got, rec)
	}
}

func TestSnapshotKillsHungRunnerAtDeadline(t *testing.T) {
	t.Parallel()
	// The fake waits on its context, so it returns only when RunSnapshot's
	// deadline for the hung source kills it; there is no sleep and the test
	// ends when it does.
	killed := make(chan struct{})
	env := envAt(fixedNow())
	env.Town = t.TempDir()
	env.Run = func(ctx context.Context, _ []string) ([]byte, error) {
		<-ctx.Done()
		close(killed)
		return nil, ctx.Err()
	}
	// The hung source is alone here: one that answered instantly would share
	// the same 1ms deadline and, on a loaded host, miss it too, adding a
	// second errors entry for a reason this test is not about (gt-m5mxp).
	// That other groups still fill beside a failed one is Collect's contract,
	// covered by its own tests under a budget no loaded host misses.
	hung := sourceFunc{group: "host", fn: func(ctx context.Context, env Env) (map[string]any, error) {
		_, err := env.Run(ctx, []string{"vm_stat"})
		return nil, err
	}}
	rec, _, err := RunSnapshot(context.Background(), env, []Source{hung}, time.Millisecond)
	if err != nil {
		t.Fatalf("RunSnapshot: %v", err)
	}
	guard, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	select {
	case <-killed:
	case <-guard.Done():
		t.Fatal("the hung runner was not killed at its deadline")
	}
	if len(rec.Errors) != 1 || rec.Errors[0].Group != "host" {
		t.Fatalf("errors = %+v, want one entry for host", rec.Errors)
	}
	if rec.Groups["host"] != nil {
		t.Fatalf("Groups[host] = %v, want null", rec.Groups["host"])
	}
}

func TestSnapshotAppendFailureIsAnError(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	// A file where the log's directory belongs makes the append fail.
	if err := os.WriteFile(filepath.Join(town, ".runtime"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	env := envAt(fixedNow())
	env.Town = town
	if _, _, err := RunSnapshot(context.Background(), env, nil, noDeadline); err == nil {
		t.Fatal("RunSnapshot returned no error when the log cannot be written")
	}
}

// regimeTestHash is the identity form the regime columns use, computed here
// rather than by calling the production hash, so the test pins the value.
func regimeTestHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:12]
}

// regimeTestTown is the town's files the four regime columns read.
func regimeTestTown(town string) map[string]string {
	return map[string]string{
		filepath.Join(town, "settings", "config.json"): `{"type": "town-settings", "version": 1,
			"polecat_pool": {"max_total": 6, "seats": {
				"claude-haiku": {"cap": 2}, "deepseek-flash": {"cap": 2},
				"claude-sonnet": {"cap": 1}, "claude-opus": {"cap": 1}}},
			"agents": {"claude-sonnet": {"command": "claude", "args": ["--model", "opus"]},
				"claude-opus": {}}}`,
		filepath.Join(town, "mayor", "rigs.json"):                                  `{"version": 1, "rigs": {"gastown": {}, "beads": {}, "docs": {}}}`,
		filepath.Join(town, "daemon", "state.json"):                                `{"pid": 42, "commit": "abc1234def"}`,
		filepath.Join(town, "gastown", "mayor", "rig", "directives", "_common.md"): "shared policy\n",
		filepath.Join(town, "gastown", "mayor", "rig", "directives", "polecat.md"): "role policy\n",
		filepath.Join(town, "gastown", "settings", "config.json"): `{"type": "rig-settings", "version": 1,
			"merge_queue": {"lint_command": "make lint", "build_command": "make build", "test_command": "make gate"}}`,
		// docs sets a merge policy but names no gate command, so it has no gate
		// id; beads has no settings file at all.
		filepath.Join(town, "docs", "settings", "config.json"): `{"type": "rig-settings", "version": 1,
			"merge_queue": {"merge_strategy": "pr"}}`,
	}
}

// regimeTestEnv serves the town's files (or the named read errors) through
// Env.ReadFile and answers the parked-rigs probe, so no host is read.
// overrides replace a fixture file, and errs make a read fail.
func regimeTestEnv(t *testing.T, town string, overrides map[string]string, errs map[string]error) Env {
	t.Helper()
	files := regimeTestTown(town)
	for path, data := range overrides {
		files[path] = data
	}
	env := envAt(fixedNow())
	env.Town = town
	env.ReadFile = func(path string) ([]byte, error) {
		if err := errs[path]; err != nil {
			return nil, err
		}
		if data, ok := files[path]; ok {
			return []byte(data), nil
		}
		return nil, fs.ErrNotExist
	}
	env.RunDir = func(context.Context, string, []string) ([]byte, error) {
		return []byte(`[{"name": "gastown", "status": "operational"}]`), nil
	}
	return env
}

// readSnapshotLine appends one snapshot and reads it back as the log holds it,
// so the assertions see the JSON the next reader will parse.
func readSnapshotLine(t *testing.T, env Env) map[string]any {
	t.Helper()
	_, path, err := RunSnapshot(context.Background(), env, []Source{configSource{}}, noDeadline)
	if err != nil {
		t.Fatalf("RunSnapshot: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	records, err := ReadSnapshots(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("ReadSnapshots: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("log has %d records, want 1", len(records))
	}
	cols := records[0].Groups["config"]
	if cols == nil {
		t.Fatalf("config group = nil, want the regime columns: %+v", records[0])
	}
	return cols
}

// The hourly record carries what was in force when it was taken: the daemon's
// in-force build, the polecat directive's hash, the seat-to-model map and each
// rig's gate id (gt-16efl).
func TestSnapshotCarriesTheRegimeFields(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cols := readSnapshotLine(t, regimeTestEnv(t, town, nil, nil))

	if got := cols["gt_commit"]; got != "abc1234def" {
		t.Errorf("gt_commit = %v, want the daemon's recorded commit", got)
	}
	if want := regimeTestHash("shared policy\nrole policy"); cols["directive_hash"] != want {
		t.Errorf("directive_hash = %v, want %s", cols["directive_hash"], want)
	}
	// claude-sonnet's agent config names opus, claude-haiku and deepseek-flash
	// are read from their names, and claude-opus is defined with no command, so
	// it runs the claude CLI's default rather than the family its name carries.
	wantSeats := map[string]any{
		"claude-haiku": "haiku", "deepseek-flash": "flash",
		"claude-sonnet": "opus", "claude-opus": "claude",
	}
	if !reflect.DeepEqual(cols["seat_models"], wantSeats) {
		t.Errorf("seat_models = %#v, want %#v", cols["seat_models"], wantSeats)
	}
	wantGate := regimeTestHash("lint\tmake lint\nbuild\tmake build\ntest\tmake gate\n")
	if want := map[string]any{"gastown": wantGate}; !reflect.DeepEqual(cols["gate_ids"], want) {
		t.Errorf("gate_ids = %#v, want %#v", cols["gate_ids"], want)
	}
	if cols["config_probe_failures"] != float64(0) {
		t.Errorf("config_probe_failures = %v, want 0", cols["config_probe_failures"])
	}
}

// A regime source that is there and cannot be read leaves its key at the
// group's missing marker (null, never a zero or an empty string) and counts
// the gap in config_probe_failures; the readable columns still fill.
func TestSnapshotRegimeFieldsMissingWhenUnreadable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		gaps    []string // fixture files whose read fails
		garble  map[string]string
		wantNil []string
	}{
		{
			name:    "the daemon state cannot be read",
			gaps:    []string{filepath.Join("daemon", "state.json")},
			wantNil: []string{"gt_commit"},
		},
		{
			name:    "a directive file cannot be read",
			gaps:    []string{filepath.Join("gastown", "mayor", "rig", "directives", "polecat.md")},
			wantNil: []string{"directive_hash"},
		},
		{
			name:    "a rig's gate config cannot be read",
			gaps:    []string{filepath.Join("gastown", "settings", "config.json")},
			wantNil: []string{"gate_ids"},
		},
		{
			name:    "a rig's root gate config cannot be read",
			gaps:    []string{filepath.Join("gastown", "config.json")},
			wantNil: []string{"gate_ids"},
		},
		{
			name:    "the daemon state does not parse",
			garble:  map[string]string{filepath.Join("daemon", "state.json"): `{"commit":`},
			wantNil: []string{"gt_commit"},
		},
		{
			name:    "a rig's gate config does not parse",
			garble:  map[string]string{filepath.Join("gastown", "settings", "config.json"): `{"merge_queue":`},
			wantNil: []string{"gate_ids"},
		},
		{
			name:    "a rig's root gate config does not parse",
			garble:  map[string]string{filepath.Join("gastown", "config.json"): `{"merge_queue":`},
			wantNil: []string{"gate_ids"},
		},
		{
			name: "every regime source at once",
			gaps: []string{
				filepath.Join("daemon", "state.json"),
				filepath.Join("gastown", "mayor", "rig", "directives", "polecat.md"),
				filepath.Join("gastown", "settings", "config.json"),
			},
			wantNil: []string{"gt_commit", "directive_hash", "gate_ids"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			errs := make(map[string]error, len(tc.gaps))
			for _, rel := range tc.gaps {
				errs[filepath.Join(town, rel)] = fs.ErrPermission
			}
			overrides := make(map[string]string, len(tc.garble))
			for rel, data := range tc.garble {
				overrides[filepath.Join(town, rel)] = data
			}
			cols := readSnapshotLine(t, regimeTestEnv(t, town, overrides, errs))

			for _, key := range tc.wantNil {
				if value, ok := cols[key]; !ok || value != nil {
					t.Errorf("%s = %v, want the missing marker null", key, value)
				}
			}
			if got, want := cols["config_probe_failures"], float64(len(tc.gaps)+len(tc.garble)); got != want {
				t.Errorf("config_probe_failures = %v, want %v, one gap line per unreadable source", got, want)
			}
			// The settings file is readable in every row, so the seat map still
			// answers beside a gap: one source costs only its own column.
			if cols["seat_models"] == nil {
				t.Errorf("seat_models = nil, want the map from the readable settings file")
			}
		})
	}
}

// The new keys are additive: a record written before them parses the same, and
// a reader that does not know them simply does not see them (gt-pkrmg).
func TestSnapshotOldLinesStillParse(t *testing.T) {
	t.Parallel()
	const oldLine = `{"v":1,"ts":"2026-09-01T00:00:00Z","placement":{"daemon":"mac-host"},` +
		`"errors":[],"config":{"pool_max_total":8,"parked_rigs":0,"config_probe_failures":0}}`
	const newLine = `{"v":1,"ts":"2026-11-01T00:00:00Z","placement":null,"errors":[],` +
		`"config":{"pool_max_total":8,"gt_commit":"abc1234def","directive_hash":"0123456789ab",` +
		`"seat_models":{"claude-haiku":"haiku"},"gate_ids":{"gastown":"fedcba987654"},` +
		`"config_probe_failures":0}}`
	records, err := ReadSnapshots(strings.NewReader(oldLine + "\n" + newLine + "\n"))
	if err != nil {
		t.Fatalf("ReadSnapshots: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("read %d records, want 2", len(records))
	}
	old := records[0].Groups["config"]
	if old["pool_max_total"] != float64(8) {
		t.Errorf("old line config.pool_max_total = %v, want 8", old["pool_max_total"])
	}
	if _, known := old["gt_commit"]; known {
		t.Errorf("old line carries gt_commit = %v, want the key absent", old["gt_commit"])
	}
	if records[1].Groups["config"]["gt_commit"] != "abc1234def" {
		t.Errorf("new line gt_commit = %v, want abc1234def", records[1].Groups["config"]["gt_commit"])
	}
}

func TestTailFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "log")
	if err := os.WriteFile(path, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		max  int64
		want string
	}{{4, "6789"}, {10, "0123456789"}, {100, "0123456789"}} {
		got, err := tailFile(path, tc.max)
		if err != nil || !bytes.Equal(got, []byte(tc.want)) {
			t.Errorf("tailFile(max %d) = %q, %v, want %q", tc.max, got, err, tc.want)
		}
	}
}
