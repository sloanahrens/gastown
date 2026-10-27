package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/git/gitfake"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
	"github.com/steveyegge/gastown/internal/tiertrigger"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

const (
	testDockerPath = "internal/testpolicy/docker.txt"
	testDockerList = "# container packages\ninternal/beads\ninternal/testutil\n"
)

// at is a UTC instant a case reads its records and clock at, so no case
// depends on the host's clock or zone.
func at(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

// world is one case's fixture: a gitfake repository with the docker list's
// packages, a town root whose state file the command reads, and the seam that
// records what the command asked the repository.
type world struct {
	t        *testing.T
	fake     *gitfake.Fake
	checkout string
	watch    *watchedRepo
	e        env
	files    *mapFiles
}

// newWorld builds the checkout and the town. The seed commit holds the docker
// list, go.mod and one file for each container package.
func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	w := &world{t: t, files: &mapFiles{files: map[string][]byte{
		testDockerPath: []byte(testDockerList),
	}}}
	origin := filepath.Join(root, "origin.git")
	w.fake = gitfake.New()
	w.fake.InitBare(t, origin)
	w.fake.Commit(t, origin, "main", "seed", map[string]string{
		testDockerPath:           testDockerList,
		"go.mod":                 "module example.com/root\n",
		"internal/beads/a.go":    "package beads\n",
		"internal/testutil/a.go": "package testutil\n",
	})
	w.checkout = filepath.Join(root, "repo")
	w.fake.Clone(t, origin, w.checkout)
	opened, ok := w.fake.Open(w.checkout).(fakeRepo)
	if !ok {
		t.Fatalf("gitfake's repository does not answer Status")
	}
	w.watch = &watchedRepo{repo: opened}
	// The run began at the commit the checkout is on; a record stamps that commit.
	began, err := opened.Rev("HEAD")
	if err != nil {
		t.Fatalf("resolve HEAD: %v", err)
	}
	w.e = env{
		town:   filepath.Join(root, "town"),
		now:    at(2026, 10, 25, 5, 0, 0),
		zone:   time.UTC,
		repo:   w.watch,
		files:  w.files,
		docker: testDockerPath,
		tested: began,
	}
	return w
}

// fakeRepo is the slice of gitfake's repository the command reads: gitfake's
// own Repo, whose listing omits Status, plus Status.
type fakeRepo interface {
	gitfake.Repo
	Status() (*git.GitStatus, error)
}

// watchedRepo is the fixture's repository: gitfake's model, plus a log of the
// refs the command read and the diff ranges it asked for, so a case proves the
// decision compares the recorded commit with HEAD. Each read fails on its own,
// because the commands read different ones at different points in a run.
type watchedRepo struct {
	repo        fakeRepo
	revErr      error
	diffErr     error
	statusErr   error
	worktreeErr error
	revs        []string
	diffs       []string
}

func (w *watchedRepo) Rev(ref string) (string, error) {
	w.revs = append(w.revs, ref)
	if w.revErr != nil {
		return "", w.revErr
	}
	return w.repo.Rev(ref)
}

func (w *watchedRepo) DiffNameOnly(base, head string) ([]string, error) {
	w.diffs = append(w.diffs, base+"..."+head)
	if w.diffErr != nil {
		return nil, w.diffErr
	}
	return w.repo.DiffNameOnly(base, head)
}

func (w *watchedRepo) Status() (*git.GitStatus, error) {
	if w.statusErr != nil {
		return nil, w.statusErr
	}
	return w.repo.Status()
}

// Worktree answers as the production repository does, from the checkout's
// status.
func (w *watchedRepo) Worktree() ([]string, error) {
	if w.worktreeErr != nil {
		return nil, w.worktreeErr
	}
	status, err := w.Status()
	if err != nil {
		return nil, err
	}
	return worktreePaths(status), nil
}

// head is the commit the checkout is on.
func (w *world) head() string {
	w.t.Helper()
	id, err := w.watch.repo.Rev("HEAD")
	if err != nil {
		w.t.Fatalf("resolve HEAD: %v", err)
	}
	return id
}

// advance commits files on the checkout's branch and writes them to the
// checkout, so the tree stays at the new HEAD as a real commit and checkout
// would.
func (w *world) advance(files map[string]string) string {
	w.t.Helper()
	id := w.fake.Commit(w.t, w.checkout, "main", "change", files)
	for p, content := range files {
		w.write(p, content)
	}
	return id
}

// write puts a file in the checkout without committing it.
func (w *world) write(path, content string) {
	w.t.Helper()
	full := filepath.Join(w.checkout, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		w.t.Fatalf("mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		w.t.Fatalf("write %s: %v", path, err)
	}
}

// statePath is the file the command reads and writes.
func (w *world) statePath() string {
	return tiertrigger.StatePath(w.e.town)
}

// runPath is the file the skips command saves the run's start state in.
func (w *world) runPath() string {
	return tiertrigger.RunPath(w.e.town, "integration-docker")
}

// seedRun saves the run's start state, as skips does before the container run.
func (w *world) seedRun(commit string, dirty []string) {
	w.t.Helper()
	if err := tiertrigger.SaveRun(w.files, w.runPath(), tiertrigger.Run{Commit: commit, Dirty: dirty}); err != nil {
		w.t.Fatalf("seed the run: %v", err)
	}
}

// seedState puts records in the state file, as a previous sweep left them.
func (w *world) seedState(records map[string]tiertrigger.Record) {
	w.t.Helper()
	data, err := json.Marshal(records)
	if err != nil {
		w.t.Fatalf("marshal state: %v", err)
	}
	w.files.files[w.statePath()] = data
}

// mapFiles is the file seam over a map, so a case seeds the docker list and
// the state file without a filesystem.
type mapFiles struct {
	files    map[string][]byte
	writeErr error
}

func (f *mapFiles) ReadFile(path string) ([]byte, error) {
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (f *mapFiles) WriteFile(path string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	f.files[path] = data
	return nil
}

// runSkips runs the skips command and returns its stdout, stderr and exit code.
func runSkips(t *testing.T, e env) (string, string, int) {
	t.Helper()
	var out, errb strings.Builder
	code := runWith([]string{"skips", "integration-docker"}, e, &out, &errb)
	return out.String(), errb.String(), code
}

func TestSkips(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		setup    func(w *world)
		want     string
		wantWarn []string
		wantDiff int
		wantCode int
	}{
		{
			name:     "no town runs every package",
			setup:    func(w *world) { w.e.town = "" },
			wantWarn: []string{"GT_TOWN_ROOT is unset", "every container package runs"},
			wantCode: 1,
		},
		{
			name:     "a missing docker list runs every package",
			setup:    func(w *world) { delete(w.files.files, testDockerPath) },
			wantWarn: []string{"read " + testDockerPath, "every container package runs"},
			wantCode: 1,
		},
		{
			name:     "a docker list naming no package runs every package",
			setup:    func(w *world) { w.files.files[testDockerPath] = []byte("# none\n") },
			wantWarn: []string{"lists no container package", "every container package runs"},
			wantCode: 1,
		},
		{
			name:     "a state file that does not parse runs every package",
			setup:    func(w *world) { w.files.files[w.statePath()] = []byte("{") },
			wantWarn: []string{"parse ", "every container package runs"},
			wantCode: 1,
		},
		{
			name: "a first sweep with no state runs every package",
			want: "",
		},
		{
			name: "an import list that cannot be read runs every package",
			setup: func(w *world) {
				w.e.deps = func(string) ([]string, error) { return nil, errors.New("go list: no module") }
			},
			wantWarn: []string{"list the imports of ./internal/beads", "every container package runs"},
			wantCode: 1,
		},
		{
			name: "a zone that cannot be loaded runs every package",
			setup: func(w *world) {
				w.e.zone = nil
				w.e.loadZone = func() (*time.Location, error) { return nil, errors.New("no zoneinfo") }
			},
			wantWarn: []string{"no zoneinfo", "every container package runs"},
			wantCode: 1,
		},
		{
			name: "a package with an unchanged record is skipped in the tier's spelling",
			setup: func(w *world) {
				w.seedState(map[string]tiertrigger.Record{
					"./internal/beads":    {Commit: w.head(), At: at(2026, 10, 25, 4, 0, 0)},
					"./internal/testutil": {Commit: w.head(), At: at(2026, 10, 25, 4, 0, 0)},
				})
			},
			want:     "integration-docker:./internal/beads integration-docker:./internal/testutil\n",
			wantDiff: 2,
		},
		{
			name: "a package whose change is on a trigger path runs, the other is skipped",
			setup: func(w *world) {
				head := w.head()
				w.seedState(map[string]tiertrigger.Record{
					"./internal/beads":    {Commit: head, At: at(2026, 10, 25, 4, 0, 0)},
					"./internal/testutil": {Commit: head, At: at(2026, 10, 25, 4, 0, 0)},
				})
				w.advance(map[string]string{"internal/beads/a.go": "package beads // changed\n"})
			},
			want:     "integration-docker:./internal/testutil\n",
			wantDiff: 2,
		},
		{
			name: "a go.mod change runs every package",
			setup: func(w *world) {
				head := w.head()
				w.seedState(map[string]tiertrigger.Record{
					"./internal/beads":    {Commit: head, At: at(2026, 10, 25, 4, 0, 0)},
					"./internal/testutil": {Commit: head, At: at(2026, 10, 25, 4, 0, 0)},
				})
				w.advance(map[string]string{"go.mod": "module example.com/root\n\nrequire example.com/x v1.0.0\n"})
			},
			want:     "",
			wantDiff: 2,
		},
		{
			name: "a package whose daily window has passed is due without reading git",
			setup: func(w *world) {
				w.seedState(map[string]tiertrigger.Record{
					"./internal/beads":    {Commit: w.head(), At: at(2026, 10, 24, 4, 0, 0)},
					"./internal/testutil": {Commit: w.head(), At: at(2026, 10, 25, 4, 0, 0)},
				})
			},
			want:     "integration-docker:./internal/testutil\n",
			wantDiff: 1,
		},
		{
			name: "a git read that fails runs every package",
			setup: func(w *world) {
				w.seedState(map[string]tiertrigger.Record{
					"./internal/beads": {Commit: w.head(), At: at(2026, 10, 25, 4, 0, 0)},
				})
				w.watch.diffErr = errors.New("bad object " + w.head())
			},
			wantWarn: []string{"bad object", "./internal/beads", "running it"},
			wantDiff: 1,
			wantCode: 0,
		},
		{
			name: "an uncommitted change on a trigger path runs the package",
			setup: func(w *world) {
				head := w.head()
				w.seedState(map[string]tiertrigger.Record{
					"./internal/beads":    {Commit: head, At: at(2026, 10, 25, 4, 0, 0)},
					"./internal/testutil": {Commit: head, At: at(2026, 10, 25, 4, 0, 0)},
				})
				// The container tests build the working tree, so a change nobody
				// committed there is code they read.
				w.write("internal/beads/a.go", "package beads // uncommitted\n")
			},
			want:     "integration-docker:./internal/testutil\n",
			wantDiff: 2,
		},
		{
			name: "a checkout that cannot be read runs every package",
			setup: func(w *world) {
				w.watch.statusErr = errors.New("index.lock exists")
			},
			wantWarn: []string{"index.lock exists", "every container package runs"},
			wantCode: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			if tc.setup != nil {
				tc.setup(w)
			}
			out, stderr, code := runSkips(t, w.e)
			if code != tc.wantCode {
				t.Errorf("skips exited %d; want %d", code, tc.wantCode)
			}
			if out != tc.want {
				t.Errorf("skips wrote %q; want %q", out, tc.want)
			}
			for _, want := range tc.wantWarn {
				if !strings.Contains(stderr, want) {
					t.Errorf("skips warned %q; want it to name %q", stderr, want)
				}
			}
			if got := len(w.watch.diffs); got != tc.wantDiff {
				t.Errorf("asked for %d diffs; want %d (%q)", got, tc.wantDiff, w.watch.diffs)
			}
			for _, d := range w.watch.diffs {
				if !strings.HasSuffix(d, "...HEAD") {
					t.Errorf("asked for %q; every diff must end at HEAD", d)
				}
			}
		})
	}
}

// TestSkipsReadsTheDailyWindowInTheInjectedZone holds the clock and the zone to
// the seams: the same record answers differently in two zones, so no case can
// pass on the host's zone instead.
func TestSkipsReadsTheDailyWindowInTheInjectedZone(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		zone *time.Location
		want string
	}{
		{
			name: "UTC: today's window has not opened, so the record still covers the day",
			zone: time.UTC,
			want: "integration-docker:./internal/beads integration-docker:./internal/testutil\n",
		},
		{
			name: "nine hours west: the window opened after the record, so both are due",
			zone: time.FixedZone("minus9", -9*60*60),
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.e.now = at(2026, 10, 25, 2, 0, 0)
			w.e.zone = tc.zone
			w.seedState(map[string]tiertrigger.Record{
				"./internal/beads":    {Commit: w.head(), At: at(2026, 10, 24, 4, 0, 0)},
				"./internal/testutil": {Commit: w.head(), At: at(2026, 10, 24, 4, 0, 0)},
			})
			out, _, code := runSkips(t, w.e)
			if code != 0 {
				t.Errorf("skips exited %d; want 0", code)
			}
			if out != tc.want {
				t.Errorf("skips wrote %q; want %q", out, tc.want)
			}
		})
	}
}

// runRecord runs the record command with the env's skip list and returns its
// stderr and exit code.
func runRecord(t *testing.T, e env) (string, int) {
	t.Helper()
	var out, errb strings.Builder
	code := runWith([]string{"record", "integration-docker"}, e, &out, &errb)
	return errb.String(), code
}

func TestRecord(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		setup   func(w *world)
		entries []string
		// runCommit and runDirty are how the sweep recorded the checkout when it
		// asked which packages were due; empty means HEAD and a clean tree.
		runCommit string
		runDirty  []string
		noRun     bool
		want      []string
		wantWarn  string
		wantRuns  bool
	}{
		{
			name:     "a clean checkout records every package at HEAD and the injected clock",
			want:     []string{"./internal/beads", "./internal/testutil"},
			wantRuns: true,
		},
		{
			name: "an existing record for a package is replaced",
			setup: func(w *world) {
				w.seedState(map[string]tiertrigger.Record{
					"./internal/beads": {Commit: "old", At: at(2026, 10, 20, 4, 0, 0)},
				})
			},
			want:     []string{"./internal/beads", "./internal/testutil"},
			wantRuns: true,
		},
		{
			name:     "an untracked file does not stop the record",
			setup:    func(w *world) { w.write("gt", "binary") },
			want:     []string{"./internal/beads", "./internal/testutil"},
			wantRuns: true,
		},
		{
			name:     "a tracked file the run itself dirtied does not stop the record",
			setup:    func(w *world) { w.write("internal/beads/a.go", "package beads // written by the run\n") },
			want:     []string{"./internal/beads", "./internal/testutil"},
			wantRuns: true,
		},
		{
			name:     "a checkout that already differed from HEAD when the run began records nothing",
			runDirty: []string{"internal/beads/a.go"},
			wantWarn: "already differed from",
			wantRuns: true,
		},
		{
			name:      "a sweep that decided at another commit records nothing",
			runCommit: "1111111111111111111111111111111111111111",
			wantWarn:  "the sweep decided at",
			wantRuns:  true,
		},
		{
			name:     "a run whose start was never recorded records nothing",
			noRun:    true,
			wantWarn: "no record of how the run began",
			wantRuns: true,
		},
		{
			name:     "a state file that does not parse is not overwritten",
			setup:    func(w *world) { w.files.files[w.statePath()] = []byte("{") },
			wantWarn: "parse ",
			wantRuns: true,
		},
		{
			name: "a save that fails warns",
			setup: func(w *world) {
				// The run began cleanly; only writing the record fails.
				w.files.files[w.runPath()] = []byte(`{"commit": "` + w.head() + `"}`)
				w.files.writeErr = fs.ErrPermission
			},
			noRun:    true,
			wantWarn: "write ",
			wantRuns: true,
		},
		{
			name:     "a whole-package skip entry is not recorded",
			entries:  []string{"integration-docker:./internal/beads"},
			want:     []string{"./internal/testutil"},
			wantRuns: true,
		},
		{
			name:     "a partial skip entry does not record the package: some of its tests did not run",
			entries:  []string{"integration-docker:./internal/beads:TestFlaky"},
			want:     []string{"./internal/testutil"},
			wantRuns: true,
		},
		{
			name:     "an entry for another tier is ignored",
			entries:  []string{"shell:scripts/x_test.sh"},
			want:     []string{"./internal/beads", "./internal/testutil"},
			wantRuns: true,
		},
		{
			name:     "every package skipped leaves the state file alone",
			entries:  []string{"integration-docker:./internal/beads", "integration-docker:./internal/testutil"},
			wantRuns: true,
		},
		{
			name:     "no town warns",
			setup:    func(w *world) { w.e.town = "" },
			wantWarn: "GT_TOWN_ROOT is unset",
		},
		{
			name: "a HEAD read that fails warns",
			setup: func(w *world) {
				w.watch.revErr = errors.New("no commits yet")
			},
			wantWarn: "read HEAD",
			wantRuns: true,
		},
		{
			name:     "a missing docker list warns",
			setup:    func(w *world) { delete(w.files.files, testDockerPath) },
			wantWarn: "read " + testDockerPath,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			if tc.setup != nil {
				tc.setup(w)
			}
			if !tc.noRun {
				commit := w.head()
				if tc.runCommit != "" {
					commit = tc.runCommit
				}
				w.seedRun(commit, tc.runDirty)
			}
			before, _ := tiertrigger.Store{Path: w.statePath(), Files: w.files}.Load()
			w.e.skip = strings.Join(tc.entries, " ")
			stderr, code := runRecord(t, w.e)
			wantCode := 0
			if tc.wantWarn != "" {
				wantCode = 1
			}
			if code != wantCode {
				t.Errorf("record exited %d; want %d", code, wantCode)
			}
			if tc.wantWarn != "" {
				if !strings.Contains(stderr, tc.wantWarn) {
					t.Errorf("record warned %q; want it to name %q", stderr, tc.wantWarn)
				}
				if !strings.Contains(stderr, "the next sweep runs the packages again") {
					t.Errorf("record warned %q; want the consequence named", stderr)
				}
			} else if stderr != "" {
				t.Errorf("record warned %q; want no warning", stderr)
			}
			if tc.wantRuns {
				if len(w.watch.revs) == 0 || w.watch.revs[0] != "HEAD" {
					t.Errorf("read %q; want HEAD", w.watch.revs)
				}
			}
			st, loadErr := tiertrigger.Store{Path: w.statePath(), Files: w.files}.Load()
			want := map[string]bool{}
			for _, pkg := range tc.want {
				want[pkg] = true
				rec, ok := st[pkg]
				if !ok {
					t.Errorf("%s was not recorded; want it at HEAD after a passing run", pkg)
					continue
				}
				if rec.Commit != w.head() {
					t.Errorf("%s recorded at %q; want HEAD %q", pkg, rec.Commit, w.head())
				}
				if !rec.At.Equal(w.e.now) {
					t.Errorf("%s recorded at %v; want the injected clock %v", pkg, rec.At, w.e.now)
				}
			}
			if loadErr == nil {
				for pkg, rec := range st {
					if !want[pkg] && before[pkg] != rec {
						t.Errorf("%s became %v; want the record it had before the run %v", pkg, rec, before[pkg])
					}
				}
			}
			if !tc.wantRuns && len(w.watch.revs) > 0 {
				t.Errorf("read %q; want no repository read before the warning", w.watch.revs)
			}
		})
	}
}

// TestRecordKeepsTheStateFileOnFailure pins the corrupt-file rule: a record
// that cannot be sure what ran leaves the bytes it found, so a later sweep
// still fails open rather than trusting a reset.
func TestRecordKeepsTheStateFileOnFailure(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.files.files[w.statePath()] = []byte("{")
	before := string(w.files.files[w.statePath()])
	runRecord(t, w.e)
	if got := string(w.files.files[w.statePath()]); got != before {
		t.Errorf("state file became %q; want the unparseable bytes %q untouched", got, before)
	}
}

func TestUsage(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil, {"skips"}, {"record", ""}, {"bogus", "x"},
		{"skips", "../escape"}, {"skips", "-n"}, {"skips", "a/b"},
	} {
		var out, errb strings.Builder
		if code := runWith(args, env{}, &out, &errb); code != 2 {
			t.Errorf("runWith(%q) exited %d; want 2", args, code)
		}
		if !strings.Contains(errb.String(), "usage") && !strings.Contains(errb.String(), "unknown command") &&
			!strings.Contains(errb.String(), "is not a tier name") {
			t.Errorf("runWith(%q) wrote %q; want a usage line", args, errb.String())
		}
	}
}

// TestTriggerOffAnswersNothing pins TIER_SWEEP_TRIGGER=0: the sweep asked for
// the every-package run it had before the trigger, so both commands answer
// nothing and succeed, and nothing is read or written.
func TestTriggerOffAnswersNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.e.off = true
	for _, mode := range []string{"skips", "record"} {
		var out, errb strings.Builder
		if code := runWith([]string{mode, "integration-docker"}, w.e, &out, &errb); code != 0 {
			t.Errorf("%s with the trigger off exited %d; want 0", mode, code)
		}
		if out.Len() != 0 || errb.Len() != 0 {
			t.Errorf("%s with the trigger off wrote %q/%q; want nothing", mode, out.String(), errb.String())
		}
	}
	if len(w.watch.revs) != 0 {
		t.Errorf("read %q with the trigger off; want no repository read", w.watch.revs)
	}
	if _, ok := w.files.files[w.statePath()]; ok {
		t.Error("a record was written with the trigger off")
	}
}

// TestFailuresCarryTheSweepLine pins the words the sweep's own output needs: a
// skips that cannot answer writes no entry at all (so every container package
// runs), and both failures name the mode, the tier and what the sweep does
// instead, in the sweep's prefix rather than the command's.
func TestFailuresCarryTheSweepLine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		mode    string
		instead string
	}{
		{mode: "skips", instead: "every container package runs"},
		{mode: "record", instead: "the next sweep runs the packages again"},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			w.e.town = ""
			var out, errb strings.Builder
			if code := runWith([]string{tc.mode, "integration-docker"}, w.e, &out, &errb); code != 1 {
				t.Errorf("%s exited %d; want 1", tc.mode, code)
			}
			if out.Len() != 0 {
				t.Errorf("%s wrote %q; want no answer at all", tc.mode, out.String())
			}
			want := "tier-sweep: TIER_SWEEP_TRIGGER: " + tc.mode + " integration-docker failed (GT_TOWN_ROOT is unset); " + tc.instead
			if !strings.Contains(errb.String(), want) {
				t.Errorf("%s wrote %q; want it to hold %q", tc.mode, errb.String(), want)
			}
		})
	}
}

// TestWorktreePaths pins which uncommitted paths make a package due: a test
// file nobody has added yet is compiled by go test just as a committed one is,
// so it counts, and every tracked kind of change counts too.
func TestWorktreePaths(t *testing.T) {
	t.Parallel()
	status := &git.GitStatus{
		Modified:  []string{"internal/beads/a.go"},
		Added:     []string{"internal/beads/b.go"},
		Deleted:   []string{"internal/beads/c.go"},
		Unmerged:  []string{"internal/beads/d.go"},
		Untracked: []string{"internal/beads/e_test.go"},
	}
	want := []string{"internal/beads/a.go", "internal/beads/b.go", "internal/beads/c.go", "internal/beads/d.go", "internal/beads/e_test.go"}
	if got := worktreePaths(status); !slices.Equal(got, want) {
		t.Errorf("worktreePaths() = %q; want %q", got, want)
	}
	if got := trackedDirty(status); slices.Contains(got, "internal/beads/e_test.go") {
		t.Errorf("trackedDirty() = %q; want no untracked file, which the sweep's own build leaves at the root", got)
	}
}

// TestRecordRefusesWhenHeadMovedSinceTheRunBegan pins the stale-stamp fix: a
// commit that landed during the run was never tested, so nothing is recorded
// for it, and the state file keeps what it held.
func TestRecordRefusesWhenHeadMovedSinceTheRunBegan(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.e.tested = "0000000000000000000000000000000000000000"
	before := w.files.files[w.statePath()]
	var errb bytes.Buffer
	if code := w.e.record("integration-docker", &errb); code != 1 {
		t.Errorf("record exited %d; want 1 so the sweep names the failure", code)
	}
	if !strings.Contains(errb.String(), "not the commit the run began at") {
		t.Errorf("record warned %q, want it to say HEAD is not the commit the run began at", errb.String())
	}
	if got := w.files.files[w.statePath()]; !bytes.Equal(got, before) {
		t.Errorf("state file changed although HEAD moved: %q", got)
	}
}

// TestRecordRefusesWithNoTestedCommit pins the other half: a record that is not
// told what the run tested stamps nothing.
func TestRecordRefusesWithNoTestedCommit(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.e.tested = ""
	var errb bytes.Buffer
	if code := w.e.record("integration-docker", &errb); code != 1 {
		t.Errorf("record exited %d; want 1 so the sweep names the failure", code)
	}
	if !strings.Contains(errb.String(), "TIER_SWEEP_TESTED is unset") {
		t.Errorf("record warned %q, want it to name TIER_SWEEP_TESTED", errb.String())
	}
	if _, ok := w.files.files[w.statePath()]; ok {
		t.Error("state file was written with no tested commit")
	}
}

// depsFixture is one case's repository: a real tree under a temp dir, because
// the root and the go tool's directories are resolved before they are compared
// and a checkout reached through a symlink is one of the cases. Only the go
// tool is faked, so no subprocess runs.
type depsFixture struct {
	root    string
	link    string
	outside string
}

func newDepsFixture(t *testing.T) depsFixture {
	t.Helper()
	base := t.TempDir()
	f := depsFixture{
		root: filepath.Join(base, "repo"),
		link: filepath.Join(base, "link"),
	}
	for _, d := range []string{"internal/beads", "internal/testutil", "internal/slot", "..cache"} {
		if err := os.MkdirAll(filepath.Join(f.root, d), 0o755); err != nil {
			t.Fatalf("build the fixture tree: %v", err)
		}
	}
	f.outside = filepath.Join(base, "elsewhere", "x")
	if err := os.MkdirAll(f.outside, 0o755); err != nil {
		t.Fatalf("build the directory above the root: %v", err)
	}
	if err := os.Symlink(f.root, f.link); err != nil {
		t.Fatalf("link the checkout: %v", err)
	}
	return f
}

// dir is a path inside the fixture's repository.
func (f depsFixture) dir(parts ...string) string {
	return filepath.Join(append([]string{f.root}, parts...)...)
}

// moduleCache stands in for a package outside the repository. It is never
// created: the go tool names it, and the answer must not need it to exist.
func (f depsFixture) moduleCache() string {
	return filepath.Join(filepath.Dir(f.root), "gomodcache", "github.com", "gofrs", "flock@v0.13.0")
}

// TestListDeps keeps the imported directories inside the repo, and asks the go
// tool for the ones the untagged build without tests cannot see. The answers
// are the shape the tool really prints: `go list -deps -test -f
// '{{if not .Standard}}{{.Dir}}{{end}}' ./internal/beads` lines a module-cache
// directory, a repo directory per import, the package itself twice (it and its
// test variant), and with -test the test-only imports.
func TestListDeps(t *testing.T) {
	t.Parallel()
	f := newDepsFixture(t)
	tests := []struct {
		name     string
		untagged string
		tagged   string
		runErr   error
		getwd    func() (string, error)
		want     []string
		wantErr  string
	}{
		{
			name: "the untagged and integration answers, the module cache left out and repeats dropped",
			untagged: f.moduleCache() + "\n" +
				f.dir("internal/beads") + "\n" + f.dir("internal/testutil") + "\n" + f.dir("internal/beads") + "\n",
			tagged: f.dir("internal/slot") + "\n" + f.dir("internal/testutil") + "\n",
			want:   []string{"internal/beads", "internal/testutil", "internal/slot"},
		},
		{
			name:     "a directory whose name starts with two dots is repo code",
			untagged: f.dir("..cache") + "\n",
			want:     []string{"..cache"},
		},
		{
			name:     "a directory above the root is not repo code",
			untagged: f.outside + "\n",
			want:     nil,
		},
		{
			name:     "the root package is repo code, and Runs reads it as every path",
			untagged: f.root + "\n" + f.dir("internal/beads") + "\n",
			want:     []string{".", "internal/beads"},
		},
		{
			// The cwd is the link while the go tool names the target: without
			// resolving the root every repo import reads as the module cache and
			// is dropped, which shrinks the trigger set to the package alone.
			name:     "a checkout the working directory spells through a link",
			untagged: f.dir("internal/beads") + "\n" + f.dir("internal/slot") + "\n",
			getwd:    func() (string, error) { return f.link, nil },
			want:     []string{"internal/beads", "internal/slot"},
		},
		{
			name:     "a directory the go tool spells through the link",
			untagged: filepath.Join(f.link, "internal", "testutil") + "\n" + f.dir("internal/beads") + "\n",
			want:     []string{"internal/testutil", "internal/beads"},
		},
		{
			name:    "a go list that fails names the package",
			runErr:  errors.New("exit status 1"),
			wantErr: "go list -deps -test ./internal/beads",
		},
		{
			name:     "a working directory that cannot be read fails the decision",
			untagged: f.dir("internal/beads") + "\n",
			getwd:    func() (string, error) { return "", errors.New("getwd: no such directory") },
			wantErr:  "getwd: no such directory",
		},
		{
			name:     "a repo root that does not resolve fails the decision",
			untagged: f.dir("internal/beads") + "\n",
			getwd:    func() (string, error) { return filepath.Join(f.root, "gone"), nil },
			wantErr:  "resolve the repo root",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var calls [][]string
			run := func(args ...string) ([]byte, error) {
				calls = append(calls, args)
				if tc.runErr != nil {
					return nil, tc.runErr
				}
				if slices.Contains(args, "integration") {
					return []byte(tc.tagged), nil
				}
				return []byte(tc.untagged), nil
			}
			getwd := tc.getwd
			if getwd == nil {
				getwd = func() (string, error) { return f.root, nil }
			}
			got, err := listDeps(run, getwd, "./internal/beads")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("listDeps error = %v; want it to name %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("listDeps: %v", err)
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("listDeps = %q; want %q", got, tc.want)
			}
			if len(calls) != 2 {
				t.Fatalf("ran the go tool %d times; want the untagged run and the integration run", len(calls))
			}
			for _, call := range calls {
				if !slices.Contains(call, "-test") {
					t.Errorf("go list %q omits -test: a test-only import is code the container tests build", call)
				}
			}
			if tagged := slices.Contains(calls[1], "integration"); !tagged {
				t.Errorf("second go list %q omits the integration tag", calls[1])
			}
		})
	}
}

// TestListDepsOnCapturedGoList drives the answer `go list -deps -test` really
// printed for ./internal/beads at the repo root, captured in
// testdata/golist-deps.txt with the root rewritten to ROOT. It holds the shape
// the tool's output has — a package listed twice for its test variant, the
// module cache beside the repo's own directories — which hand-written fixtures
// drift away from (om, gt-e69b2).
func TestListDepsOnCapturedGoList(t *testing.T) {
	t.Parallel()
	f := newDepsFixture(t)
	captured, err := os.ReadFile(filepath.Join("testdata", "golist-deps.txt"))
	if err != nil {
		t.Fatalf("read the captured go list answer: %v", err)
	}
	out := strings.ReplaceAll(string(captured), "ROOT", f.root)
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, f.root+string(filepath.Separator)) {
			continue
		}
		if err := os.MkdirAll(line, 0o755); err != nil {
			t.Fatalf("create %s: %v", line, err)
		}
	}
	run := func(args ...string) ([]byte, error) {
		if slices.Contains(args, "integration") {
			return nil, nil
		}
		return []byte(out), nil
	}
	got, err := listDeps(run, func() (string, error) { return f.link, nil }, "./internal/beads")
	if err != nil {
		t.Fatalf("listDeps: %v", err)
	}
	for _, want := range []string{"internal/beads", "internal/slot", "internal/testutil", "internal/testutil/unittier"} {
		if !slices.Contains(got, want) {
			t.Errorf("listDeps did not find %q among %q", want, got)
		}
	}
	for _, dir := range got {
		if strings.HasPrefix(dir, "..") || strings.HasPrefix(dir, "/") {
			t.Errorf("listDeps kept %q, which is not a repo directory", dir)
		}
	}
	seen := map[string]bool{}
	for _, dir := range got {
		if seen[dir] {
			t.Errorf("listDeps listed %q twice; want each directory once", dir)
		}
		seen[dir] = true
	}
}

// TestPackagesReadsTheDockerListsSpellings holds the parse to the sweep's own
// awk: blank lines and whole-line comments are skipped, a trailing comment is
// cut, and "./" or a trailing slash on the directory does not change the name
// the skip entry must match.
func TestPackagesReadsTheDockerListsSpellings(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.files.files[testDockerPath] = []byte("# header\n\n./internal/beads  # trailing comment\ninternal/testutil/\n")
	pkgs, err := w.e.packages()
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	var names []string
	for _, pkg := range pkgs {
		names = append(names, pkg.Name)
	}
	want := []string{"./internal/beads", "./internal/testutil"}
	if !slices.Equal(names, want) {
		t.Errorf("packages = %q; want %q", names, want)
	}
}

// TestPackageTriggersIncludeTheRepoDirectoriesItImports pins the transitive
// fix: a change in a repo package the container package imports makes it due,
// not only a change in its own directory.
func TestPackageTriggersIncludeTheRepoDirectoriesItImports(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	w.files.files[testDockerPath] = []byte("./internal/beads\n")
	w.e.deps = func(pkg string) ([]string, error) {
		if pkg != "./internal/beads" {
			t.Fatalf("deps asked for %q, want ./internal/beads", pkg)
		}
		return []string{"internal/testutil"}, nil
	}
	pkgs, err := w.e.packages()
	if err != nil {
		t.Fatalf("packages: %v", err)
	}
	if len(pkgs) != 1 || !slices.Contains(pkgs[0].Triggers, "./internal/testutil") {
		t.Fatalf("triggers = %v, want the imported directory among them", pkgs)
	}
	if !tiertrigger.Runs(pkgs[0].Triggers, []string{"internal/testutil/x.go"}, false) {
		t.Error("a change in an imported directory does not make the package due")
	}
}
