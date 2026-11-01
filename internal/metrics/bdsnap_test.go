package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// bdDailyNow is the clock every daily-snapshot test reads, in UTC so the local
// day is the UTC day.
var bdDailyNow = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

// bdDailyRigs is a mayor/rigs.json registering the named rigs.
func bdDailyRigs(names ...string) string {
	var b strings.Builder
	b.WriteString(`{"version":1,"rigs":{`)
	for i, name := range names {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "%q:{}", name)
	}
	b.WriteString("}}")
	return b.String()
}

// bdDailyEnv is an Env whose registry is registry, whose bd answers listing in
// every directory, and whose clock is now.
func bdDailyEnv(t *testing.T, now time.Time, registry, listing string) Env {
	t.Helper()
	town := t.TempDir()
	return Env{
		Town: town,
		ReadFile: func(path string) ([]byte, error) {
			if path == filepath.Join(town, "mayor", "rigs.json") {
				return []byte(registry), nil
			}
			// The day's log is read back from the test town, so a second run
			// sees what the first appended.
			return os.ReadFile(path)
		},
		RunDir: func(_ context.Context, _ string, argv []string) ([]byte, error) {
			// The daily run must send bd the command the bead names.
			want := []string{"bd", "list", "--all", "--limit", "0", "--json"}
			if !reflect.DeepEqual(argv, want) {
				t.Errorf("bd argv = %v, want %v", argv, want)
			}
			return []byte(listing), nil
		},
		Now: func() time.Time { return now },
	}
}

// bdDailyRead is the lines the run appended, decoded into the two record
// shapes: a counts line keeps its total, a gap line its reason.
func bdDailyRead(t *testing.T, env Env) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(BdDailyPath(env.Town))
	if err != nil {
		t.Fatalf("reading the daily log: %v", err)
	}
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// bdDailyListing is the listing TestBdDailyCountsByStatusPriorityAndLabel
// counts: an open shaped bead with a day of age, a closed bug, an old open
// bead, and an in_progress bead, with labels used 3, 2 and 1 times.
const bdDailyListing = `[
 {"id":"gt-1","created_at":"2026-11-01T02:00:00Z","closed_at":null,"status":"open",
  "issue_type":"task","priority":1,"labels":["common","common","a"],"dependency_count":0,
  "acceptance_criteria":"- [ ] x"},
 {"id":"gt-2","created_at":"2026-10-30T12:00:00Z","closed_at":"2026-11-01T03:00:00Z",
  "status":"closed","issue_type":"bug","priority":2,"labels":["common","a"],"dependency_count":3},
 {"id":"gt-3","created_at":"2026-10-20T12:00:00Z","closed_at":null,"status":"open",
  "issue_type":"task","priority":null,"labels":["common","b"],"dependency_count":7},
 {"id":"gt-4","created_at":"2026-09-01T12:00:00Z","closed_at":null,"status":"in_progress",
  "issue_type":"task","priority":0,"labels":["c"],"dependency_count":1}
]`

func TestBdDailyCountsByStatusPriorityAndLabel(t *testing.T) {
	t.Parallel()
	env := bdDailyEnv(t, bdDailyNow, bdDailyRigs("gastown"), bdDailyListing)
	res, err := RunBdDaily(context.Background(), env)
	if err != nil {
		t.Fatalf("RunBdDaily: %v", err)
	}
	if !reflect.DeepEqual(res.Written, []string{"gastown"}) || len(res.Recorded) != 0 || len(res.Gaps) != 0 {
		t.Fatalf("result = %+v, want gastown written and nothing else", res)
	}

	var lines []BdDailyLine
	data, err := os.ReadFile(BdDailyPath(env.Town))
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range strings.Split(strings.TrimSuffix(string(data), "\n"), "\n") {
		var line BdDailyLine
		if err := json.Unmarshal([]byte(raw), &line); err != nil {
			t.Fatalf("line %q: %v", raw, err)
		}
		lines = append(lines, line)
	}
	if len(lines) != 1 {
		t.Fatalf("wrote %d lines, want 1", len(lines))
	}
	p50, p90, max := 0.4, 12.0, 12.0
	want := BdDailyLine{
		Date:            "2026-11-01",
		Rig:             "gastown",
		Total:           4,
		ByStatus:        map[string]int{"open": 2, "closed": 1, "in_progress": 1},
		ByPriority:      map[string]int{"0": 1, "1": 1, "2": 1, "none": 1},
		ByType:          map[string]int{"task": 3, "bug": 1},
		ByLabel:         map[string]int{"common": 3, "other": 4},
		DependencyCount: map[string]int{"0": 1, "1": 1, "2": 0, "3-5": 1, "6+": 1},
		OpenAgeDays:     BdDailyAges{P50: &p50, P90: &p90, Max: &max, OldestID: "gt-3"},
		CreatedToday:    1,
		ClosedToday:     1,
		ShapedOpen:      1,
		UnshapedOpen:    1,
	}
	if !reflect.DeepEqual(lines[0], want) {
		t.Fatalf("line = %+v\nwant %+v", lines[0], want)
	}
}

// The day's lines are written once: a second run the same local day writes
// nothing, and the next day writes them again.
func TestBdDailyWritesOncePerDayPerRig(t *testing.T) {
	t.Parallel()
	registry := bdDailyRigs("alpha", "beta")
	env := bdDailyEnv(t, bdDailyNow, registry, `[{"id":"al-1","created_at":"2026-11-01T00:00:00Z",
	 "status":"open","issue_type":"task","priority":2,"labels":[],"dependency_count":0}]`)

	first, err := RunBdDaily(context.Background(), env)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if len(first.Written) != 2 || len(first.Recorded) != 0 {
		t.Fatalf("first run = %+v, want both rigs written", first)
	}
	// A later hour of the same local day.
	env.Now = func() time.Time { return bdDailyNow.Add(6 * time.Hour) }
	second, err := RunBdDaily(context.Background(), env)
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	if len(second.Written) != 0 || len(second.Recorded) != 2 {
		t.Fatalf("second run = %+v, want both rigs already recorded", second)
	}
	if got := len(bdDailyRead(t, env)); got != 2 {
		t.Fatalf("after two runs the log holds %d lines, want 2", got)
	}
	// The next local day has no lines yet.
	env.Now = func() time.Time { return bdDailyNow.Add(24 * time.Hour) }
	third, err := RunBdDaily(context.Background(), env)
	if err != nil {
		t.Fatalf("next-day run: %v", err)
	}
	if len(third.Written) != 2 {
		t.Fatalf("next-day run = %+v, want both rigs written", third)
	}
	if got := len(bdDailyRead(t, env)); got != 4 {
		t.Fatalf("after three runs the log holds %d lines, want 4", got)
	}
}

// A rig whose listing fails is a gap line with no counts, so a reader cannot
// take it for a rig that holds no beads.
func TestBdDailyFailedRigIsAGapNotZeros(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	bad := filepath.Join(town, "bad")
	env := Env{
		Town: town,
		ReadFile: func(path string) ([]byte, error) {
			if path == filepath.Join(town, "mayor", "rigs.json") {
				return []byte(bdDailyRigs("bad", "good")), nil
			}
			return nil, fs.ErrNotExist
		},
		RunDir: func(_ context.Context, dir string, _ []string) ([]byte, error) {
			if dir == bad {
				return nil, errors.New("dolt is down")
			}
			return []byte(`[{"id":"gd-1","created_at":"2026-11-01T00:00:00Z","status":"open",
			 "issue_type":"task","priority":1,"labels":[],"dependency_count":0}]`), nil
		},
		Now: func() time.Time { return bdDailyNow },
	}
	res, err := RunBdDaily(context.Background(), env)
	if err != nil {
		t.Fatalf("RunBdDaily: %v", err)
	}
	if !reflect.DeepEqual(res.Written, []string{"good"}) || !reflect.DeepEqual(res.Gaps, []string{"bad"}) {
		t.Fatalf("result = %+v, want good written and bad a gap", res)
	}
	lines := bdDailyRead(t, env)
	if len(lines) != 2 {
		t.Fatalf("wrote %d lines, want 2", len(lines))
	}
	gap, counts := lines[0], lines[1]
	if gap["rig"] != "bad" || gap["gap"] == "" || gap["gap"] == nil {
		t.Fatalf("gap line = %v", gap)
	}
	if _, ok := gap["total"]; ok {
		t.Fatalf("gap line carries counts: %v", gap)
	}
	if counts["rig"] != "good" || counts["total"] != float64(1) {
		t.Fatalf("counts line = %v", counts)
	}
}

// A listing whose rows cannot be counted is a gap too, naming the bead, rather
// than a line that is partly untrue.
func TestBdDailyUncountableRowIsAGap(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		row  string
		want string
	}{
		{
			name: "unparseable created_at",
			row:  `{"id":"gt-1","created_at":"yesterday","status":"open","issue_type":"task","priority":1}`,
			want: "created_at",
		},
		{
			name: "unparseable closed_at",
			row:  `{"id":"gt-1","created_at":"2026-11-01T00:00:00Z","closed_at":"yesterday","status":"closed","issue_type":"task","priority":1}`,
			want: "closed_at",
		},
		{
			name: "negative dependency_count",
			row:  `{"id":"gt-1","created_at":"2026-11-01T00:00:00Z","status":"open","issue_type":"task","priority":1,"dependency_count":-1}`,
			want: "dependency_count",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			env := bdDailyEnv(t, bdDailyNow, bdDailyRigs("gastown"), "["+tc.row+"]")
			res, err := RunBdDaily(context.Background(), env)
			if err != nil {
				t.Fatalf("RunBdDaily: %v", err)
			}
			if len(res.Gaps) != 1 || len(res.Written) != 0 {
				t.Fatalf("result = %+v, want the rig a gap", res)
			}
			lines := bdDailyRead(t, env)
			if got, _ := lines[0]["gap"].(string); !strings.Contains(got, tc.want) {
				t.Fatalf("gap = %q, want it to name %q", got, tc.want)
			}
		})
	}
}

// A registry or log the run cannot read stops it rather than writing a day
// that looks complete.
func TestBdDailyRefusesUnreadableInput(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		readFile func(town string) func(string) ([]byte, error)
	}{
		{
			name: "unreadable registry",
			readFile: func(town string) func(string) ([]byte, error) {
				return func(string) ([]byte, error) { return nil, errors.New("permission denied") }
			},
		},
		{
			name: "unreadable log",
			readFile: func(town string) func(string) ([]byte, error) {
				return func(path string) ([]byte, error) {
					if path == filepath.Join(town, "mayor", "rigs.json") {
						return []byte(bdDailyRigs("gastown")), nil
					}
					return nil, errors.New("permission denied")
				}
			},
		},
		{
			name: "malformed line in the log",
			readFile: func(town string) func(string) ([]byte, error) {
				return func(path string) ([]byte, error) {
					if path == filepath.Join(town, "mayor", "rigs.json") {
						return []byte(bdDailyRigs("gastown")), nil
					}
					return []byte("garbage\n{\"date\":\"2026-10-31\",\"rig\":\"gastown\"}\n"), nil
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			env := Env{
				Town:     town,
				ReadFile: tc.readFile(town),
				RunDir: func(context.Context, string, []string) ([]byte, error) {
					return []byte(`[]`), nil
				},
				Now: func() time.Time { return bdDailyNow },
			}
			if _, err := RunBdDaily(context.Background(), env); err == nil {
				t.Fatalf("RunBdDaily accepted the %s", tc.name)
			}
			if _, err := os.Stat(BdDailyPath(town)); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("the run left a log after refusing: %v", err)
			}
		})
	}
}

// An environment missing a seam stops the run instead of writing a day from
// nothing.
func TestBdDailyRefusesAnIncompleteEnv(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  Env
	}{
		{name: "no town", env: Env{Now: func() time.Time { return bdDailyNow }}},
		{name: "no file reader", env: Env{Town: t.TempDir(), Now: func() time.Time { return bdDailyNow }}},
		{name: "no bd runner", env: Env{
			Town:     t.TempDir(),
			ReadFile: func(string) ([]byte, error) { return nil, fs.ErrNotExist },
			Now:      func() time.Time { return bdDailyNow },
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := RunBdDaily(context.Background(), tc.env); err == nil {
				t.Fatalf("RunBdDaily accepted an env with %s", tc.name)
			}
		})
	}
}

// A registry name that is not one directory name is a gap, and bd is never run
// outside the rigs.
func TestBdDailyRigNameOutsideTheTownIsAGap(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	env := Env{
		Town: town,
		ReadFile: func(path string) ([]byte, error) {
			if path == filepath.Join(town, "mayor", "rigs.json") {
				return []byte(bdDailyRigs("../outside")), nil
			}
			return os.ReadFile(path)
		},
		RunDir: func(_ context.Context, dir string, _ []string) ([]byte, error) {
			t.Errorf("bd ran in %s, outside the town's rigs", dir)
			return []byte(`[]`), nil
		},
		Now: func() time.Time { return bdDailyNow },
	}
	res, err := RunBdDaily(context.Background(), env)
	if err != nil {
		t.Fatalf("RunBdDaily: %v", err)
	}
	if !reflect.DeepEqual(res.Gaps, []string{"../outside"}) || len(res.Written) != 0 {
		t.Fatalf("result = %+v, want only the outside name a gap", res)
	}
}

// A final fragment a crashed append left without its newline is not a record,
// so that rig is written again; battery.Append separates the fragment.
func TestBdDailyIgnoresATrailingPartialLine(t *testing.T) {
	t.Parallel()
	registry := bdDailyRigs("alpha", "beta")
	env := bdDailyEnv(t, bdDailyNow, registry, `[]`)
	env.ReadFile = func(path string) ([]byte, error) {
		if path == filepath.Join(env.Town, "mayor", "rigs.json") {
			return []byte(registry), nil
		}
		return []byte("{\"date\":\"2026-11-01\",\"rig\":\"alpha\"}\n{\"date\":\"2026-11-01\",\"rig\":\"beta\""), nil
	}
	res, err := RunBdDaily(context.Background(), env)
	if err != nil {
		t.Fatalf("RunBdDaily: %v", err)
	}
	if !reflect.DeepEqual(res.Recorded, []string{"alpha"}) || !reflect.DeepEqual(res.Written, []string{"beta"}) {
		t.Fatalf("result = %+v, want alpha recorded and beta written", res)
	}
}

// A failed append fails the run rather than leaving it looking written. The
// reader reports the log missing while the log's directory cannot be made, so
// the failure lands on the append and not on the earlier read.
func TestBdDailyRefusesAFailedAppend(t *testing.T) {
	t.Parallel()
	env := bdDailyEnv(t, bdDailyNow, bdDailyRigs("gastown"), `[]`)
	if err := os.WriteFile(filepath.Join(env.Town, ".runtime"), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry := filepath.Join(env.Town, "mayor", "rigs.json")
	env.ReadFile = func(path string) ([]byte, error) {
		if path == registry {
			return []byte(bdDailyRigs("gastown")), nil
		}
		return nil, fs.ErrNotExist
	}
	if _, err := RunBdDaily(context.Background(), env); err == nil {
		t.Fatal("RunBdDaily accepted a failed append")
	}
}
