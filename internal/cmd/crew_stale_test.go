package cmd

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/crewfresh"
)

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

func (f *fakeCrewStaleGit) repo(root string) crewStaleGit {
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
	if ref != crewStaleOriginMain {
		return 0, errors.New("unexpected ref " + ref)
	}
	if r.town.fail[r.root] == "behind" {
		return 0, errors.New("no such ref")
	}
	return r.town.behind[r.root], nil
}

// staleTown is a town on disk under t.TempDir: crew clones whose FETCH_HEAD
// carries the fetch time the test sets, plus the git surface a fake answers.
type staleTown struct {
	root string
	now  time.Time
	git  *fakeCrewStaleGit
}

func newStaleTown(t *testing.T) *staleTown {
	t.Helper()
	return &staleTown{
		root: t.TempDir(),
		now:  time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
		git: &fakeCrewStaleGit{
			branch:    map[string]string{},
			behind:    map[string]int{},
			commonDir: map[string]string{},
			fail:      map[string]string{},
		},
	}
}

// clone creates <town>/<rig>/crew/<dir>, with a FETCH_HEAD last modified at
// fetched. A zero fetched leaves no FETCH_HEAD: the clone never fetched.
func (s *staleTown) clone(t *testing.T, rig, dir string, fetched time.Time) string {
	t.Helper()
	root := filepath.Join(s.root, rig, "crew", dir)
	gitDir := filepath.Join(root, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("create %s: %v", gitDir, err)
	}
	s.git.branch[root] = "main"
	head := filepath.Join(gitDir, "FETCH_HEAD")
	if fetched.IsZero() {
		return root
	}
	if err := os.WriteFile(head, []byte("deadbeef\t\tbranch 'main' of origin\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", head, err)
	}
	if err := os.Chtimes(head, fetched, fetched); err != nil {
		t.Fatalf("set the mtime of %s: %v", head, err)
	}
	return root
}

func (s *staleTown) deps() crewStaleDeps {
	return crewStaleDeps{
		findTown: func() (string, error) { return s.root, nil },
		now:      func() time.Time { return s.now },
		repos:    s.git.repo,
	}
}

// TestCrewStaleListsOnlyCrewCheckoutsPastTheLimit pins what the report names:
// the five-day clone appears, and neither a clone fetched two hours ago — in
// this rig or the other one — nor mayor/rig nor a polecat worktree does.
func TestCrewStaleListsOnlyCrewCheckoutsPastTheLimit(t *testing.T) {
	t.Parallel()
	town := newStaleTown(t)
	fresh := town.clone(t, "gastown", "dunlin", town.now.Add(-2*time.Hour))
	stale := town.clone(t, "gastown", "merlin", town.now.Add(-5*24*time.Hour))
	other := town.clone(t, "devops", "otto", town.now.Add(-time.Hour))
	town.git.behind[fresh] = 0
	town.git.behind[stale] = 7
	town.git.behind[other] = 0

	// A town holds a rig with no crew, its own bookkeeping directory, and
	// these two checkouts; none of them is a crew clone, so none may be read
	// or reported.
	if err := os.MkdirAll(filepath.Join(town.root, "review"), 0o755); err != nil {
		t.Fatalf("create a rig with no crew: %v", err)
	}
	notCrew := []string{
		filepath.Join(town.root, "gastown", "mayor", "rig"),
		filepath.Join(town.root, "gastown", "polecats", "coma", "gastown"),
		filepath.Join(town.root, "devops", "polecats"),
	}
	for _, root := range notCrew {
		if err := os.MkdirAll(filepath.Join(root, ".git"), 0o755); err != nil {
			t.Fatalf("create %s: %v", root, err)
		}
		town.git.branch[root] = "main"
	}
	if err := os.MkdirAll(filepath.Join(town.root, ".beads", "crew"), 0o755); err != nil {
		t.Fatalf("create the town's .beads: %v", err)
	}

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}

	want := "gastown/merlin  main  120h  7\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
	for _, call := range town.git.calls {
		if strings.Contains(call, "mayor") || strings.Contains(call, "polecats") || strings.Contains(call, ".beads") {
			t.Errorf("gt crew stale read a checkout that is not a crew clone: %q", call)
		}
	}
}

// TestCrewStaleListsACloneThatNeverFetched covers the checkout with no
// FETCH_HEAD: it has no fetch time to read, and that is "never", not a read
// failure, so the line shows it instead of a reason.
func TestCrewStaleListsACloneThatNeverFetched(t *testing.T) {
	t.Parallel()
	town := newStaleTown(t)
	town.clone(t, "gastown", "dunlin", time.Time{})

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}
	want := "gastown/dunlin  main  never  0\n"
	if got := out.String(); got != want {
		t.Errorf("gt crew stale printed %q, want %q", got, want)
	}
}

// TestCrewStaleShowsADetachedHead pins the rendering of the branch read that
// prints HEAD: the report names it detached rather than calling it a branch.
func TestCrewStaleShowsADetachedHead(t *testing.T) {
	t.Parallel()
	town := newStaleTown(t)
	root := town.clone(t, "gastown", "dunlin", town.now.Add(-2*24*time.Hour))
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
	town := newStaleTown(t)
	root := town.clone(t, "gastown", "dunlin-fix-flake", town.now.Add(-2*time.Hour))

	shared := filepath.Join(town.root, "gastown", ".repo.git")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatalf("create %s: %v", shared, err)
	}
	head := filepath.Join(shared, "FETCH_HEAD")
	if err := os.WriteFile(head, []byte("deadbeef\t\tbranch 'main' of origin\n"), 0o644); err != nil {
		t.Fatalf("write %s: %v", head, err)
	}
	fetched := town.now.Add(-5 * 24 * time.Hour)
	if err := os.Chtimes(head, fetched, fetched); err != nil {
		t.Fatalf("set the mtime of %s: %v", head, err)
	}
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

// TestCrewStaleDoesNotFetch pins the command's whole git surface: every call
// it makes is a read, and the clone's FETCH_HEAD is left standing after the
// run. The fake records each call, so a call a later change adds is recorded
// here and fails the test.
func TestCrewStaleDoesNotFetch(t *testing.T) {
	t.Parallel()
	town := newStaleTown(t)
	root := town.clone(t, "gastown", "merlin", town.now.Add(-5*24*time.Hour))
	town.git.behind[root] = 3
	head := filepath.Join(root, ".git", "FETCH_HEAD")
	before, err := os.Stat(head)
	if err != nil {
		t.Fatalf("stat %s: %v", head, err)
	}

	var out bytes.Buffer
	if err := crewStale(&out, town.deps()); err != nil {
		t.Fatalf("crewStale() = %v, want no error", err)
	}

	if len(town.git.calls) == 0 {
		t.Fatal("no git call was recorded, so this test proves nothing")
	}
	reads := []string{"CommonDir", "CurrentBranch", "CountCommitsBehind"}
	for _, call := range town.git.calls {
		method, _, _ := strings.Cut(call, " ")
		if !slices.Contains(reads, method) {
			t.Errorf("gt crew stale asked git to %s, and it may only read", call)
		}
	}

	after, err := os.Stat(head)
	if err != nil {
		t.Fatalf("stat %s after the run: %v", head, err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("FETCH_HEAD mtime moved from %v to %v: the command wrote to the clone",
			before.ModTime(), after.ModTime())
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
		reading crewStaleReading
		want    string
	}{
		{
			name: "a clone on main",
			reading: crewStaleReading{
				clone:    crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "main", LastFetch: now.Add(-5 * time.Hour), Behind: 2},
				branchOK: true, fetchOK: true, behindOK: true,
			},
			want: "gastown/dunlin  main  5h  2",
		},
		{
			name: "a detached head",
			reading: crewStaleReading{
				clone:    crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "", LastFetch: now.Add(-30 * time.Hour)},
				branchOK: true, fetchOK: true, behindOK: true,
			},
			want: "gastown/dunlin  detached  30h  0",
		},
		{
			name: "a clone that never fetched",
			reading: crewStaleReading{
				clone:    crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "main"},
				branchOK: true, fetchOK: true, behindOK: true,
			},
			want: "gastown/dunlin  main  never  0",
		},
		{
			name: "a behind count that could not be read",
			reading: crewStaleReading{
				clone: crewfresh.Clone{Rig: "gastown", Dir: "dunlin", Branch: "main",
					LastFetch: now.Add(-26 * time.Hour), Err: "the commits behind origin/main could not be counted"},
				branchOK: true, fetchOK: true,
			},
			want: "gastown/dunlin  main  26h  ?  the commits behind origin/main could not be counted",
		},
		{
			name: "a clone no read reached",
			reading: crewStaleReading{
				clone: crewfresh.Clone{Rig: "gastown", Dir: "dunlin",
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
	town := newStaleTown(t)
	fetched := town.now.Add(-5 * 24 * time.Hour)
	noBranch := town.clone(t, "gastown", "aaa", fetched)
	noBehind := town.clone(t, "gastown", "bbb", fetched)
	noGitDir := town.clone(t, "gastown", "ccc", fetched)
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
// not absence: the checkout's git path is a file, so FETCH_HEAD cannot be
// reached. That is a read that failed, not a clone that never fetched, so the
// line names the question instead of showing "never".
func TestCrewStaleReportsAnUnreadableFetchHead(t *testing.T) {
	t.Parallel()
	town := newStaleTown(t)
	root := filepath.Join(town.root, "gastown", "crew", "dunlin")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("create %s: %v", root, err)
	}
	// A linked checkout's .git is a file, and a common dir that does not
	// resolve leaves FETCH_HEAD's parent a non-directory.
	if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: lost\n"), 0o644); err != nil {
		t.Fatalf("write the .git file: %v", err)
	}
	town.git.branch[root] = "main"

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
	town := newStaleTown(t)
	town.clone(t, "gastown", "dunlin", town.now.Add(-2*time.Hour))

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
	crewPathIsFile := func(t *testing.T) crewStaleDeps {
		t.Helper()
		root := t.TempDir()
		if err := os.MkdirAll(filepath.Join(root, "gastown"), 0o755); err != nil {
			t.Fatalf("create the rig: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, "gastown", "crew"), nil, 0o644); err != nil {
			t.Fatalf("write the crew path: %v", err)
		}
		return crewStaleDeps{
			findTown: func() (string, error) { return root, nil },
			now:      time.Now,
			repos:    (&fakeCrewStaleGit{}).repo,
		}
	}

	cases := []struct {
		name    string
		deps    func(t *testing.T) crewStaleDeps
		wantErr string
	}{
		{
			name: "a town root that cannot be found",
			deps: func(t *testing.T) crewStaleDeps {
				return crewStaleDeps{
					findTown: func() (string, error) { return "", errors.New("not in a town") },
					now:      time.Now,
					repos:    (&fakeCrewStaleGit{}).repo,
				}
			},
			wantErr: "find the town root",
		},
		{
			name: "a town root that does not exist",
			deps: func(t *testing.T) crewStaleDeps {
				return crewStaleDeps{
					findTown: func() (string, error) { return filepath.Join(t.TempDir(), "gone"), nil },
					now:      time.Now,
					repos:    (&fakeCrewStaleGit{}).repo,
				}
			},
			wantErr: "read the town root",
		},
		{
			name:    "a crew path that is a file",
			deps:    crewPathIsFile,
			wantErr: "read the crew directory",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := crewStale(&out, tc.deps(t))
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
