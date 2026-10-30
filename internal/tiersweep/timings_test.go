package tiersweep

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// readTimings reads a timings file the way the next beads will: one JSON object
// per line, in file order.
func readTimings(t *testing.T, path string) []Timing {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var got []Timing
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" {
			continue
		}
		var tm Timing
		if err := json.Unmarshal([]byte(line), &tm); err != nil {
			t.Fatalf("line %q of %s: %v", line, path, err)
		}
		got = append(got, tm)
	}
	return got
}

// namingLines counts the stderr lines that name path: a timings failure names
// the file once, and the error text names it again in the same line.
func namingLines(stderr, path string) int {
	n := 0
	for _, line := range strings.Split(stderr, "\n") {
		if strings.Contains(line, path) {
			n++
		}
	}
	return n
}

// TestTierWritesTimings: three packages through fakeGo and fakeClock, one
// passing, one failing and one skipped by the skip list, leave exactly two
// lines in timings.jsonl, in run order, with the wall time the clock moved, the
// exit code and the start time of each.
func TestTierWritesTimings(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, time.October, 30, 0, 0, 0, 0, time.UTC)
	takes := map[string]time.Duration{
		"./internal/a": 1500 * time.Millisecond,
		"./internal/b": 2500 * time.Millisecond,
	}
	clock := &fakeClock{at: start}
	fake := &fakeGo{codes: map[string]int{"./internal/b": 1}}
	// A package's run is what moves the clock, so each line's wall time is that
	// package's own.
	exec := func(dir string, env []string, argv []string, out io.Writer) int {
		clock.at = clock.at.Add(takes[argv[len(argv)-1]])
		return fake.exec(dir, env, argv, out)
	}
	logDir := t.TempDir()
	stderr := &bytes.Buffer{}
	r := &Runner{
		Exec:     exec,
		LogDir:   logDir,
		Timeout:  "7m",
		SkipList: "integration:./internal/c",
		Stderr:   stderr,
		Timings:  OpenTimings(filepath.Join(logDir, TimingsName), stderr),
		Now:      clock.now,
	}
	r.Tier(TierIntegration, ".", nil, nil, []string{"./internal/a", "./internal/b", "./internal/c"})
	r.Timings.Close()

	want := []Timing{
		{Tier: TierIntegration, Package: "./internal/a", WallMS: 1500, Exit: 0, Start: "2026-10-30T00:00:00Z"},
		{Tier: TierIntegration, Package: "./internal/b", WallMS: 2500, Exit: 1, Start: "2026-10-30T00:00:01.5Z"},
	}
	if got := readTimings(t, filepath.Join(logDir, TimingsName)); !reflect.DeepEqual(got, want) {
		t.Errorf("timings = %+v, want %+v", got, want)
	}
}

// TestTierTimingsComeFromTheSweepClock: each Go tier's lines are timed by the
// sweep's clock, not the real one, and each names its own tier and package.
func TestTierTimingsComeFromTheSweepClock(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		tier  string
		setup func(*rig)
		want  []Timing
	}{
		{
			name: "integration", tier: TierIntegration,
			setup: func(r *rig) { oneTagged(r.repo) },
			want: []Timing{{
				Tier: TierIntegration, Package: "./r", WallMS: 45000, Exit: 0,
				Start: "2026-10-30T00:00:00Z",
			}},
		},
		{
			name: "race", tier: TierRace,
			want: []Timing{{
				Tier: TierRace, Package: "./internal/cmd", WallMS: 45000, Exit: 0,
				Start: "2026-10-30T00:00:00Z",
			}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			r.goTestTakes = 45 * time.Second
			if code, _, stderr := r.run(tc.tier); code != 0 {
				t.Fatalf("exit code = %d, stderr %q", code, stderr)
			}
			if got := readTimings(t, filepath.Join(r.logDir, TimingsName)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("timings = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestTierTimingsLeaveTheSummaryAlone: a fixture run's summary line is byte for
// byte the same with a writable timings file and with one that cannot be
// created, so the daemon's parse is untouched.
func TestTierTimingsLeaveTheSummaryAlone(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	oneTagged(r.repo)
	r.goTestTakes = 45 * time.Second
	code, withTimings, stderr := r.run(TierIntegration)
	if code != 0 {
		t.Fatalf("exit code = %d, stderr %q", code, stderr)
	}
	if got := readTimings(t, filepath.Join(r.logDir, TimingsName)); len(got) != 1 {
		t.Fatalf("timings = %+v, want the one package's line", got)
	}
	// The same log directory, its timings file now a directory that cannot be
	// written, so the second run's summary line is not compared against a
	// different log directory.
	timingsPath := filepath.Join(r.logDir, TimingsName)
	if err := os.Remove(timingsPath); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(timingsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	code, withoutTimings, stderr := r.run(TierIntegration)
	if code != 0 {
		t.Fatalf("exit code = %d with an unwritable timings file, stderr %q", code, stderr)
	}
	if withoutTimings != withTimings {
		t.Errorf("summary line = %q, want %q", withoutTimings, withTimings)
	}
}

// TestTierTimingsWriteFailureIsNotRed: a timings file that cannot be created
// names the file on stderr once and leaves the tier's verdict, counts and exit
// code alone.
func TestTierTimingsWriteFailureIsNotRed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	oneTagged(r.repo)
	timingsPath := filepath.Join(r.logDir, TimingsName)
	if err := os.Mkdir(timingsPath, 0o755); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr := r.run(TierIntegration)
	if code != 0 || !strings.Contains(stdout, "integration GREEN passed=1 failed=0 skipped=0") {
		t.Errorf("code %d, stdout %q, want exit 0 and the GREEN counts", code, stdout)
	}
	if got := namingLines(stderr, timingsPath); got != 1 {
		t.Errorf("stderr names %s on %d lines, want once:\n%s", timingsPath, got, stderr)
	}
}

// TestTimingsFileIsReplacedAtTheEnd: with TIER_SWEEP_TIMINGS set, the run
// replaces that file with its own lines, through a temp file beside it and a
// rename over it, so an earlier run's line is gone; with it unset no such write
// happens; and a replace that fails is named once and changes no verdict.
func TestTimingsFileIsReplacedAtTheEnd(t *testing.T) {
	t.Parallel()
	t.Run("a set file is replaced with this run's lines", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		oneTagged(r.repo)
		r.timingsFile = filepath.Join(t.TempDir(), TimingsName)
		if err := os.WriteFile(r.timingsFile, []byte(`{"tier":"an earlier run"}`+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		if code, _, stderr := r.run(TierIntegration); code != 0 {
			t.Fatalf("exit code = %d, stderr %q", code, stderr)
		}
		own := readTimings(t, filepath.Join(r.logDir, TimingsName))
		if len(own) != 1 {
			t.Fatalf("the run's own timings = %+v, want one line", own)
		}
		if got := readTimings(t, r.timingsFile); !reflect.DeepEqual(got, own) {
			t.Errorf("persistent timings = %+v, want this run's %+v", got, own)
		}
		if len(r.writes) != 1 {
			t.Fatalf("WriteFile calls = %v, want one temp file", r.writes)
		}
		tmp := r.writes[0]
		if tmp == r.timingsFile || filepath.Dir(tmp) != filepath.Dir(r.timingsFile) {
			t.Errorf("wrote %q, want a temp file beside %q", tmp, r.timingsFile)
		}
		if want := [][2]string{{tmp, r.timingsFile}}; !reflect.DeepEqual(r.renames, want) {
			t.Errorf("renames = %v, want %v", r.renames, want)
		}
	})
	t.Run("an unset file is not written", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		oneTagged(r.repo)
		if code, _, stderr := r.run(TierIntegration); code != 0 {
			t.Fatalf("exit code = %d, stderr %q", code, stderr)
		}
		if len(r.writes) != 0 || len(r.renames) != 0 {
			t.Errorf("writes %v, renames %v, want none", r.writes, r.renames)
		}
	})
	t.Run("a replace that fails is named once and changes no verdict", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		oneTagged(r.repo)
		// A directory that does not exist cannot hold the temp file.
		r.timingsFile = filepath.Join(t.TempDir(), "missing", TimingsName)
		code, stdout, stderr := r.run(TierIntegration)
		if code != 0 || !strings.Contains(stdout, "integration GREEN passed=1 failed=0 skipped=0") {
			t.Errorf("code %d, stdout %q, want exit 0 and the GREEN counts", code, stdout)
		}
		if got := namingLines(stderr, r.timingsFile); got != 1 {
			t.Errorf("stderr names %s on %d lines, want once:\n%s", r.timingsFile, got, stderr)
		}
		if len(r.renames) != 0 {
			t.Errorf("renames = %v, want none after a failed write", r.renames)
		}
	})
}

// TestTimingsFailuresAreNamedOnce: a write that fails drops the file, so a
// whole run of packages names it once and keeps every line for the persistent
// file; a close that fails is named too. Both drive the production Add and
// Close, with the open file closed out of band to make them fail the way a full
// disk or an unmounted directory would.
func TestTimingsFailuresAreNamedOnce(t *testing.T) {
	t.Parallel()
	t.Run("a failed write", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		path := filepath.Join(t.TempDir(), TimingsName)
		tm := OpenTimings(path, stderr)
		if err := tm.out.Close(); err != nil {
			t.Fatal(err)
		}
		tm.Add(Timing{Tier: TierIntegration, Package: "./a"})
		tm.Add(Timing{Tier: TierIntegration, Package: "./b"})
		tm.Close()

		if got := namingLines(stderr.String(), path); got != 1 {
			t.Errorf("stderr names %s on %d lines, want once:\n%s", path, got, stderr)
		}
		if got := strings.Count(string(tm.Bytes()), "\n"); got != 2 {
			t.Errorf("Bytes() holds %d lines, want both:\n%s", got, tm.Bytes())
		}
	})
	t.Run("a failed close", func(t *testing.T) {
		t.Parallel()
		stderr := &bytes.Buffer{}
		path := filepath.Join(t.TempDir(), TimingsName)
		tm := OpenTimings(path, stderr)
		if err := tm.out.Close(); err != nil {
			t.Fatal(err)
		}
		tm.Close()

		if got := namingLines(stderr.String(), path); got != 1 {
			t.Errorf("stderr names %s on %d lines, want once:\n%s", path, got, stderr)
		}
	})
}

// TestOpenTimingsNamesAnUncreatableFile: a timings file that cannot be created
// is named on stderr by the open itself, and every later Add only keeps its
// line for the persistent file.
func TestOpenTimingsNamesAnUncreatableFile(t *testing.T) {
	t.Parallel()
	stderr := &bytes.Buffer{}
	path := filepath.Join(t.TempDir(), TimingsName)
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	tm := OpenTimings(path, stderr)
	tm.Add(Timing{Tier: TierIntegration, Package: "./a"})
	tm.Close()

	if got := namingLines(stderr.String(), path); got != 1 {
		t.Errorf("stderr names %s on %d lines, want once:\n%s", path, got, stderr)
	}
	if want := 1; strings.Count(string(tm.Bytes()), "\n") != want {
		t.Errorf("Bytes() = %q, want the line kept", tm.Bytes())
	}
}

// TestParseWalls: a timings file's lines become the wall each tier and package
// took, keyed by tier and package, and a line that is not a timing is dropped
// so a torn file starts the packages in list order.
func TestParseWalls(t *testing.T) {
	t.Parallel()
	data := `{"tier":"integration","package":"./cmd","wall_ms":196000,"exit":0,"start":"2026-10-30T00:00:00Z"}
{"tier":"integration-docker","package":"./cmd","wall_ms":1000,"exit":0,"start":"2026-10-30T00:00:00Z"}
not a timing line

{"tier":"race","package":"./cmd","wall_ms":500,"exit":1,"start":"2026-10-30T00:00:00Z"}`
	want := map[string]int64{
		TimingsKey(TierIntegration, "./cmd"): 196000,
		TimingsKey(TierDocker, "./cmd"):      1000,
		TimingsKey(TierRace, "./cmd"):        500,
	}
	if got := ParseWalls([]byte(data)); !reflect.DeepEqual(got, want) {
		t.Errorf("ParseWalls() = %v, want %v", got, want)
	}
	if got := ParseWalls([]byte("not a timing\n")); len(got) != 0 {
		t.Errorf("ParseWalls over no timing = %v, want none", got)
	}
}
