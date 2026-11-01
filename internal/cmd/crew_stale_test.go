package cmd

import (
	"bytes"
	"errors"
	"io/fs"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/crewfresh"
	"github.com/steveyegge/gastown/internal/crewstale"
)

// fakeCrewStaleFS is the town the walk reads: the entries each directory
// holds, when each FETCH_HEAD was written, and the errors to answer with
// instead. Every call is recorded, so a test can assert the walk only asked to
// read.
type fakeCrewStaleFS struct {
	dirs    map[string][]fs.DirEntry
	dirErrs map[string]error
	mtime   map[string]time.Time
	fileErr map[string]error
	calls   []string
}

func newFakeCrewStaleFS() *fakeCrewStaleFS {
	return &fakeCrewStaleFS{
		dirs:    map[string][]fs.DirEntry{},
		dirErrs: map[string]error{},
		mtime:   map[string]time.Time{},
		fileErr: map[string]error{},
	}
}

func (f *fakeCrewStaleFS) ReadDir(name string) ([]fs.DirEntry, error) {
	f.calls = append(f.calls, "ReadDir "+name)
	if err := f.dirErrs[name]; err != nil {
		return nil, err
	}
	entries, ok := f.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
	}
	return entries, nil
}

func (f *fakeCrewStaleFS) Stat(name string) (fs.FileInfo, error) {
	f.calls = append(f.calls, "Stat "+name)
	if err := f.fileErr[name]; err != nil {
		return nil, err
	}
	at, ok := f.mtime[name]
	if !ok {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
	}
	return fileInfo{name: filepath.Base(name), at: at}, nil
}

// entry is one name in a fake directory listing: the walk asks a listing for a
// name and whether it is a directory, and nothing else.
func entry(name string, dir bool) fs.DirEntry {
	return fs.FileInfoToDirEntry(fileInfo{name: name, dir: dir})
}

type fileInfo struct {
	name string
	dir  bool
	at   time.Time
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return 0 }
func (i fileInfo) ModTime() time.Time { return i.at }
func (i fileInfo) IsDir() bool        { return i.dir }
func (i fileInfo) Sys() any           { return nil }

func (i fileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}

// fakeCrewStaleGit answers one checkout's reads from the test's tables and
// records every call, so a test can assert what the command asked and that it
// asked nothing that writes.
type fakeCrewStaleGit struct {
	branch map[string]string
	behind map[string]int
	// commonDir names a git directory other than the checkout's own .git,
	// as a linked worktree has.
	commonDir map[string]string
	fail      map[string]string
	calls     []string
}

// fakeCrewStaleRepo is the seam for one checkout root. The command builds it
// per root, so the root travels beside the table.
type fakeCrewStaleRepo struct {
	town *fakeCrewStaleGit
	root string
}

func (f *fakeCrewStaleGit) repo(root string) crewstale.Git {
	return fakeCrewStaleRepo{town: f, root: root}
}

func (r fakeCrewStaleRepo) record(method string) {
	r.town.calls = append(r.town.calls, method+" "+r.root)
}

func (r fakeCrewStaleRepo) CommonDir() (string, error) {
	r.record("CommonDir")
	if r.town.fail[r.root] == "gitdir" {
		return "", errors.New("not a git repository")
	}
	if dir, ok := r.town.commonDir[r.root]; ok {
		return dir, nil
	}
	return filepath.Join(r.root, ".git"), nil
}

func (r fakeCrewStaleRepo) CurrentBranch() (string, error) {
	r.record("CurrentBranch")
	if r.town.fail[r.root] == "branch" {
		return "", errors.New("bad revision")
	}
	return r.town.branch[r.root], nil
}

func (r fakeCrewStaleRepo) CountCommitsBehind(ref string) (int, error) {
	r.record("CountCommitsBehind")
	if ref != crewstale.OriginMain {
		return 0, errors.New("unexpected ref " + ref)
	}
	if r.town.fail[r.root] == "behind" {
		return 0, errors.New("no such ref")
	}
	return r.town.behind[r.root], nil
}

// staleTown is a town the walk reads through fakes: the rigs and crew clones
// the fake filesystem holds, and the git answers for each clone.
type staleTown struct {
	root string
	now  time.Time
	fs   *fakeCrewStaleFS
	git  *fakeCrewStaleGit
}

func newStaleTown() *staleTown {
	return &staleTown{
		root: "/town",
		now:  time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		fs:   newFakeCrewStaleFS(),
		git: &fakeCrewStaleGit{
			branch:    map[string]string{},
			behind:    map[string]int{},
			commonDir: map[string]string{},
			fail:      map[string]string{},
		},
	}
}

// addDir records that path holds entries, keeping what it already holds.
func (s *staleTown) addDir(path string, entries ...fs.DirEntry) {
	s.fs.dirs[path] = append(s.fs.dirs[path], entries...)
}

// rig adds a directory to the town root, once however many clones it holds.
func (s *staleTown) rig(name string) {
	if slices.ContainsFunc(s.fs.dirs[s.root], func(e fs.DirEntry) bool { return e.Name() == name }) {
		return
	}
	s.addDir(s.root, entry(name, true))
}

// clone adds <town>/<rig>/crew/<dir>, with a FETCH_HEAD last written at
// fetched. A zero fetched leaves no FETCH_HEAD: the clone never fetched.
func (s *staleTown) clone(rig, dir string, fetched time.Time) string {
	s.rig(rig)
	crewDir := filepath.Join(s.root, rig, "crew")
	s.addDir(crewDir, entry(dir, true))
	root := filepath.Join(crewDir, dir)
	s.git.branch[root] = "main"
	if !fetched.IsZero() {
		s.fs.mtime[filepath.Join(root, ".git", "FETCH_HEAD")] = fetched
	}
	return root
}

func (s *staleTown) deps() crewStaleDeps {
	return crewStaleDeps{
		findTown: func() (string, error) { return s.root, nil },
		now:      func() time.Time { return s.now },
		fs:       s.fs,
		repos:    s.git.repo,
	}
}

// TestCrewStaleListsOnlyCrewCheckoutsPastTheLimit pins what the report names:
// the five-day clone appears, and neither a clone fetched two hours ago — in
// this rig or the other one — nor the town's mayor directory nor the rig's own
// mayor/rig and polecat worktrees does.
func TestCrewStaleListsOnlyCrewCheckoutsPastTheLimit(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	town.rig("mayor")  // the town's own bookkeeping directory, not a rig
	town.rig("review") // a rig with no crew directory
	fresh := town.clone("gastown", "dunlin", town.now.Add(-2*time.Hour))
	stale := town.clone("gastown", "merlin", town.now.Add(-5*24*time.Hour))
	town.clone("devops", "otto", town.now.Add(-time.Hour))
	town.git.behind[fresh] = 0
	town.git.behind[stale] = 7

	// The fake holds these so a walk that reached into them would show in the
	// recorded calls rather than finding an empty town.
	town.addDir(filepath.Join(town.root, "gastown", "mayor"), entry("rig", true))
	town.addDir(filepath.Join(town.root, "gastown", "polecats"), entry("coma", true))
	town.addDir(filepath.Join(town.root, "gastown", "polecats", "coma", "gastown"), entry(".git", true))

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}

	want := "gastown/merlin  main  120h  7\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
	for _, call := range append(slices.Clone(town.git.calls), town.fs.calls...) {
		if strings.Contains(call, "mayor/rig") || strings.Contains(call, "polecats/") {
			t.Errorf("gt crew stale read a checkout that is not a crew clone: %q", call)
		}
	}
}

// TestCrewStaleDoesNotFetch pins the command's whole surface: every git call
// is a read and every filesystem call is a read, so no later change can add a
// fetch or a write without this failing.
func TestCrewStaleDoesNotFetch(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	root := town.clone("gastown", "merlin", town.now.Add(-5*24*time.Hour))
	town.git.behind[root] = 3

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}

	if len(town.git.calls) == 0 || len(town.fs.calls) == 0 {
		t.Fatal("no read was recorded, so this test proves nothing")
	}
	gitReads := []string{"CommonDir", "CurrentBranch", "CountCommitsBehind"}
	for _, call := range town.git.calls {
		method, _, _ := strings.Cut(call, " ")
		if !slices.Contains(gitReads, method) {
			t.Errorf("gt crew stale asked git to %s, and it may only read", call)
		}
	}
	fsReads := []string{"ReadDir", "Stat"}
	for _, call := range town.fs.calls {
		method, _, _ := strings.Cut(call, " ")
		if !slices.Contains(fsReads, method) {
			t.Errorf("gt crew stale asked the filesystem to %s, and it may only read", call)
		}
	}
}

// TestCrewStaleListsACloneThatNeverFetched covers the checkout with no
// FETCH_HEAD: it has no fetch time to read, and that is "never", not a read
// failure, so the line shows it instead of a reason.
func TestCrewStaleListsACloneThatNeverFetched(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	town.clone("gastown", "dunlin", time.Time{})

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	want := "gastown/dunlin  main  never  0\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
}

// TestCrewStaleListsACloneDatedInTheFuture covers a FETCH_HEAD written after
// this run: its age cannot be printed as a stale duration, and treating it as
// fresh would drop the clone from the report entirely. It is listed with the
// reason instead.
func TestCrewStaleListsACloneDatedInTheFuture(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	town.clone("gastown", "dunlin", town.now.Add(2*time.Hour))

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	want := "gastown/dunlin  main  ?  0  FETCH_HEAD is dated in the future\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
}

// TestCrewStaleShowsADetachedHead pins the rendering of the branch read that
// prints HEAD: the report names it detached rather than calling it a branch.
func TestCrewStaleShowsADetachedHead(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	root := town.clone("gastown", "dunlin", town.now.Add(-2*24*time.Hour))
	town.git.branch[root] = "HEAD"
	town.git.behind[root] = 4

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	want := "gastown/dunlin  detached  48h  4\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
}

// TestCrewStaleReadsTheSharedFetchHead covers a checkout whose own .git holds
// a fresh FETCH_HEAD but whose shared git directory holds the stale one, as a
// linked worktree's does: the shared file is the fetch that brought the tree
// down, so it is the time the report counts.
func TestCrewStaleReadsTheSharedFetchHead(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	root := town.clone("gastown", "dunlin-fix-flake", town.now.Add(-2*time.Hour))
	shared := filepath.Join(town.root, "gastown", ".repo.git")
	town.fs.mtime[filepath.Join(shared, "FETCH_HEAD")] = town.now.Add(-5 * 24 * time.Hour)
	town.git.commonDir[root] = shared

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	want := "gastown/dunlin-fix-flake  main  120h  0\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
}

// TestCrewStaleLine shows a field the command could not read as "?" and says
// why, so an unreadable clone never renders as one on main, fetched now and up
// to date.
func TestCrewStaleLine(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		reading crewstale.Reading
		want    string
	}{
		{
			name: "a clone on main",
			reading: crewstale.Reading{
				Clone:    crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "main", LastFetch: now.Add(-5 * time.Hour), Behind: 2},
				BranchOK: true, FetchOK: true, BehindOK: true,
			},
			want: "gastown/dunlin  main  5h  2",
		},
		{
			name: "a detached head",
			reading: crewstale.Reading{
				Clone:    crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "", LastFetch: now.Add(-30 * time.Hour)},
				BranchOK: true, FetchOK: true, BehindOK: true,
			},
			want: "gastown/dunlin  detached  30h  0",
		},
		{
			name: "a clone that never fetched",
			reading: crewstale.Reading{
				Clone:    crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "main"},
				BranchOK: true, FetchOK: true, BehindOK: true,
			},
			want: "gastown/dunlin  main  never  0",
		},
		{
			name: "a fetch time that could not be trusted",
			reading: crewstale.Reading{
				Clone: crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "main",
					Err: "FETCH_HEAD is dated in the future"},
				BranchOK: true, BehindOK: true,
			},
			want: "gastown/dunlin  main  ?  0  FETCH_HEAD is dated in the future",
		},
		{
			name: "a behind count that could not be read",
			reading: crewstale.Reading{
				Clone: crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "main",
					LastFetch: now.Add(-26 * time.Hour), Err: "the commits behind origin/main could not be counted"},
				BranchOK: true, FetchOK: true,
			},
			want: "gastown/dunlin  main  26h  ?  the commits behind origin/main could not be counted",
		},
		{
			name: "a clone no read reached",
			reading: crewstale.Reading{
				Clone: crewfresh.Clone{Rig: "gastown", Dir: "dunlin",
					Err: "the branch could not be read; FETCH_HEAD could not be read"},
			},
			want: "gastown/dunlin  ?  ?  ?  the branch could not be read; FETCH_HEAD could not be read",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := crewStaleLine(tc.reading, now); got != tc.want {
				t.Errorf("crewStaleLine() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCrewStaleReportsUnreadableCheckouts drives each failed read through the
// command: the clone is still listed, its failed field shows "?", and the
// command still exits 0, since a clone it cannot judge is a fact to report and
// not a reason to withhold the rest of the town.
func TestCrewStaleReportsUnreadableCheckouts(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	fetched := town.now.Add(-5 * 24 * time.Hour)
	noBranch := town.clone("gastown", "aaa", fetched)
	noBehind := town.clone("gastown", "bbb", fetched)
	noGitDir := town.clone("gastown", "ccc", fetched)
	town.git.fail[noBranch] = "branch"
	town.git.fail[noBehind] = "behind"
	town.git.fail[noGitDir] = "gitdir"
	town.git.behind[noGitDir] = 0

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}

	// ccc leads because a fetch time that could not be read is the zero time
	// crewfresh.Stale sorts first, as it sorts a clone that never fetched.
	want := strings.Join([]string{
		"gastown/ccc  main  ?  0  FETCH_HEAD could not be read",
		"gastown/aaa  ?  120h  0  the branch could not be read",
		"gastown/bbb  main  120h  ?  the commits behind origin/main could not be counted",
		"",
	}, "\n")
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
}

// TestCrewStaleReportsAnUnreadableFetchHead drives the stat failure that is
// not absence: FETCH_HEAD exists but cannot be read. That is a read that
// failed, not a clone that never fetched, so the line names the question
// instead of showing "never".
func TestCrewStaleReportsAnUnreadableFetchHead(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	root := town.clone("gastown", "dunlin", town.now.Add(-5*24*time.Hour))
	town.fs.fileErr[filepath.Join(root, ".git", "FETCH_HEAD")] = errors.New("permission denied")

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	want := "gastown/dunlin  main  ?  0  FETCH_HEAD could not be read\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
}

// TestCrewStalePrintsNothingWhenEveryCloneIsFresh pins the convention the
// report relies on: empty output means no crew clone is stale.
func TestCrewStalePrintsNothingWhenEveryCloneIsFresh(t *testing.T) {
	t.Parallel()
	town := newStaleTown()
	town.clone("gastown", "dunlin", town.now.Add(-2*time.Hour))

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("gt crew stale printed %q, want nothing", got)
	}
}

// TestCrewStaleRefusesWhatItCannotList covers the failures that leave the
// command with no set of clones: an unreadable town root and a crew path that
// is not a directory are errors, never an empty report.
func TestCrewStaleRefusesWhatItCannotList(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		deps    func() crewStaleDeps
		wantErr string
	}{
		{
			name: "a town root that cannot be found",
			deps: func() crewStaleDeps {
				deps := newStaleTown().deps()
				deps.findTown = func() (string, error) { return "", errors.New("not in a town") }
				return deps
			},
			wantErr: "find the town root",
		},
		{
			name:    "a town root that does not exist",
			deps:    func() crewStaleDeps { return newStaleTown().deps() },
			wantErr: "read the town root",
		},
		{
			name: "a crew path that is a file",
			deps: func() crewStaleDeps {
				town := newStaleTown()
				town.rig("gastown")
				crewDir := filepath.Join(town.root, "gastown", "crew")
				town.fs.dirErrs[crewDir] = &fs.PathError{Op: "readdir", Path: crewDir, Err: fs.ErrInvalid}
				return town.deps()
			},
			wantErr: "read the crew directory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := crewStale(&out, tc.deps())
			if err == nil {
				t.Fatalf("crewStale() = nil, want an error naming %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("crewStale() = %v, want it to name %q", err, tc.wantErr)
			}
			if out.Len() != 0 {
				t.Errorf("crewStale() printed %q, want nothing on a failed read", out.String())
			}
		})
	}
}
