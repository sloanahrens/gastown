package tiertrigger

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeFiles is the state store's file interface over a map, so a load tests
// what the file holds without a filesystem.
type fakeFiles struct {
	files     map[string][]byte
	readErr   error
	writeErr  error
	removeErr error
}

func (f *fakeFiles) RemoveFile(path string) error {
	if f.removeErr != nil {
		return f.removeErr
	}
	delete(f.files, path)
	return nil
}

func (f *fakeFiles) ReadFile(path string) ([]byte, error) {
	if f.readErr != nil {
		return nil, f.readErr
	}
	data, ok := f.files[path]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return data, nil
}

func (f *fakeFiles) WriteFile(path string, data []byte) error {
	if f.writeErr != nil {
		return f.writeErr
	}
	if f.files == nil {
		f.files = map[string][]byte{}
	}
	f.files[path] = data
	return nil
}

// The fixture commits are object ids: a record holds one, and Changed refuses
// anything else before it reaches git.
const (
	commit1    = "1111111111111111111111111111111111111111"
	commit2    = "2222222222222222222222222222222222222222"
	commit0    = "0000000000000000000000000000000000000000"
	commitGone = "abcabcabcabcabcabcabcabcabcabcabcabcabca"
)

// fakeGit is the repository the due decision reads: changes and errs are keyed
// by the commit a package recorded, so a test sees which base was asked for
// and can fail one package's diff alone. dirty and worktreeErr are the
// checkout's uncommitted changes and their read failure.
type fakeGit struct {
	changes     map[string][]string
	errs        map[string]error
	asked       []string
	dirty       []string
	worktreeErr error
}

func (g *fakeGit) DiffNameOnly(base, _ string) ([]string, error) {
	g.asked = append(g.asked, base)
	if err := g.errs[base]; err != nil {
		return nil, err
	}
	return g.changes[base], nil
}

func (g *fakeGit) Worktree() ([]string, error) {
	if g.worktreeErr != nil {
		return nil, g.worktreeErr
	}
	return g.dirty, nil
}

// rec is a record from a run this morning at 04:00, after the daily window
// opened, so only a change or a later window makes it due again.
func rec(tb testing.TB, commit string) Record {
	return Record{Commit: commit, At: at(tb, 2026, 10, 25, 4, 0, 0)}
}

// sameRecord compares the fields, not the struct: a time.Time also carries its
// location pointer and wall/ext encoding, so == is false for the same instant
// once the value has been through JSON.
func sameRecord(got, want Record) bool {
	return got.Commit == want.Commit && got.At.Equal(want.At)
}

func TestStoreLoad(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   *fakeFiles
		want    State
		wantErr string
	}{
		{
			name:  "a missing file is an empty state",
			files: &fakeFiles{},
			want:  State{},
		},
		{
			name:    "a file that does not parse is an error",
			files:   &fakeFiles{files: map[string][]byte{"/state.json": []byte(`{"internal/beads":`)}},
			wantErr: "parse /state.json",
		},
		{
			name: "a record with no commit is an error",
			files: &fakeFiles{files: map[string][]byte{"/state.json": []byte(
				`{"internal/beads": {"commit": "", "at": "2026-10-25T09:00:00Z"}}`)}},
			wantErr: "internal/beads has no commit or time",
		},
		{
			name: "a record with no time is an error",
			files: &fakeFiles{files: map[string][]byte{"/state.json": []byte(
				`{"internal/beads": {"commit": "c1"}}`)}},
			wantErr: "internal/beads has no commit or time",
		},
		{
			name:    "a read that fails for another reason is an error, not an empty state",
			files:   &fakeFiles{readErr: fs.ErrPermission},
			wantErr: "read /state.json",
		},
		{
			name: "a good file loads its records",
			files: &fakeFiles{files: map[string][]byte{"/state.json": []byte(
				`{"internal/beads": {"commit": "c1", "at": "2026-10-25T09:00:00Z"}}`)}},
			want: State{"internal/beads": {Commit: "c1", At: time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			st, err := Store{Path: "/state.json", Files: tc.files}.Load()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Load() = %v, %v; want error containing %q", st, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load(): %v", err)
			}
			if len(st) != len(tc.want) {
				t.Fatalf("Load() = %v; want %v", st, tc.want)
			}
			for pkg, want := range tc.want {
				if got := st[pkg]; !sameRecord(got, want) {
					t.Errorf("Load()[%q] = %v; want %v", pkg, got, want)
				}
			}
		})
	}
}

func TestStoreSave(t *testing.T) {
	t.Parallel()
	files := &fakeFiles{}
	store := Store{Path: "/state.json", Files: files}
	in := State{}
	in.Ran("internal/beads", "c1", time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC))
	if err := store.Save(in); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() after Save(): %v", err)
	}
	if want := in["internal/beads"]; !sameRecord(got["internal/beads"], want) {
		t.Fatalf("round trip = %v; want %v", got, want)
	}
	if err := (Store{Path: "/nil.json", Files: files}).Save(nil); err != nil {
		t.Fatalf("Save(nil): %v", err)
	}
	if got := string(files.files["/nil.json"]); strings.TrimSpace(got) != "{}" {
		t.Errorf("Save(nil) wrote %q; want {}", got)
	}
	if err := (Store{Path: "/x", Files: &fakeFiles{writeErr: fs.ErrPermission}}).Save(in); err == nil {
		t.Error("Save() over an unwritable file returned nil")
	}
}

// TestStatePath pins the path the sweep reads and writes, spelled out rather
// than rebuilt from the helper it exercises.
func TestStatePath(t *testing.T) {
	t.Parallel()
	if got, want := StatePath("/town"), "/town/.runtime/tiertrigger/state.json"; got != want {
		t.Errorf("StatePath(/town) = %q; want %q", got, want)
	}
}

// TestRunPath pins the run's start state path, spelled out rather than rebuilt
// from the helper it exercises.
func TestRunPath(t *testing.T) {
	t.Parallel()
	want := "/town/.runtime/tiertrigger/integration-docker.run.json"
	if got := RunPath("/town", "integration-docker"); got != want {
		t.Errorf("RunPath(/town, integration-docker) = %q; want %q", got, want)
	}
}

// TestOSFilesRemoveFile pins the removal's rule: the file goes, a file that is
// already gone is not a failure, and a removal that fails says so rather than
// leaving a caller thinking the file is gone.
func TestOSFilesRemoveFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "run.json")
	if err := (OSFiles{}).WriteFile(path, []byte("x\n")); err != nil {
		t.Fatalf("WriteFile(): %v", err)
	}
	if err := (OSFiles{}).RemoveFile(path); err != nil {
		t.Fatalf("RemoveFile(): %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the file is still there after RemoveFile: %v", err)
	}
	if err := (OSFiles{}).RemoveFile(path); err != nil {
		t.Errorf("RemoveFile() of a file that is gone = %v; want nil", err)
	}
	// A directory with a file in it is not removable, and that failure must
	// come back rather than read as done.
	holding := filepath.Join(dir, "holding")
	if err := (OSFiles{}).WriteFile(filepath.Join(holding, "f"), []byte("x\n")); err != nil {
		t.Fatalf("WriteFile(%s): %v", holding, err)
	}
	if err := (OSFiles{}).RemoveFile(holding); err == nil {
		t.Errorf("RemoveFile() of the directory %s holding a file returned nil", holding)
	}
}

func TestRunRoundTrip(t *testing.T) {
	t.Parallel()
	files := &fakeFiles{}
	in := Run{Commit: commit1, Dirty: []string{"internal/beads/a.go"}}
	if err := SaveRun(files, "/run.json", in); err != nil {
		t.Fatalf("SaveRun(): %v", err)
	}
	got, err := LoadRun(files, "/run.json")
	if err != nil {
		t.Fatalf("LoadRun(): %v", err)
	}
	if got.Commit != in.Commit || strings.Join(got.Dirty, ",") != strings.Join(in.Dirty, ",") {
		t.Errorf("LoadRun() = %+v; want %+v", got, in)
	}

	if _, err := LoadRun(&fakeFiles{}, "/missing.json"); !errors.Is(err, ErrNoRun) {
		t.Errorf("LoadRun(missing) = %v; want ErrNoRun", err)
	}
	for name, files := range map[string]*fakeFiles{
		"no commit":  {files: map[string][]byte{"/r.json": []byte("\n")}},
		"empty file": {files: map[string][]byte{"/r.json": nil}},
		"unreadable": {readErr: fs.ErrPermission},
	} {
		if _, err := LoadRun(files, "/r.json"); err == nil {
			t.Errorf("LoadRun(%s) returned nil", name)
		}
	}
	if err := SaveRun(&fakeFiles{writeErr: fs.ErrPermission}, "/x", in); err == nil {
		t.Error("SaveRun() over an unwritable file returned nil")
	}
}

// TestStoreOSFiles covers what the fake filesystem cannot: Save creates the
// runtime directory, replaces the file without leaving its temp file behind,
// and Load reads a missing file through the real os.ReadFile as an empty
// state.
func TestStoreOSFiles(t *testing.T) {
	t.Parallel()
	path := StatePath(t.TempDir())
	store := Store{Path: path, Files: OSFiles{}}

	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load() before any Save: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Load() before any Save = %v; want an empty state", got)
	}

	st := State{}
	st.Ran("internal/beads", "c1", at(t, 2026, 10, 25, 4, 0, 0))
	if err := store.Save(st); err != nil {
		t.Fatalf("Save(): %v", err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatalf("Load() after Save(): %v", err)
	}
	if len(got) != 1 || !sameRecord(got["internal/beads"], st["internal/beads"]) {
		t.Fatalf("round trip = %v; want %v", got, st)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != StateFileName {
		t.Fatalf("state dir holds %v; want only %s", entries, StateFileName)
	}

	st.Ran("internal/beads", "c2", at(t, 2026, 10, 26, 4, 0, 0))
	if err := store.Save(st); err != nil {
		t.Fatalf("Save() over an existing file: %v", err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatalf("Load() after the second Save(): %v", err)
	}
	if len(got) != 1 || !sameRecord(got["internal/beads"], st["internal/beads"]) {
		t.Errorf("record after a second Save = %v; want %v", got, st["internal/beads"])
	}
}

func TestStateChanged(t *testing.T) {
	t.Parallel()
	st := State{}
	st.Ran("internal/beads", commit1, at(t, 2026, 10, 25, 4, 0, 0))
	tests := []struct {
		name    string
		git     *fakeGit
		want    []string
		wantErr string
	}{
		{
			name: "the commits since the recorded run",
			git:  &fakeGit{changes: map[string][]string{commit1: {"internal/beads/schema.go"}}},
			want: []string{"internal/beads/schema.go"},
		},
		{
			name: "an uncommitted change is code the tests build too",
			git: &fakeGit{
				changes: map[string][]string{commit1: {"internal/beads/schema.go"}},
				dirty:   []string{"internal/cmd/root.go"},
			},
			want: []string{"internal/beads/schema.go", "internal/cmd/root.go"},
		},
		{
			name:    "a diff that fails is an error",
			git:     &fakeGit{errs: map[string]error{commit1: errors.New("bad object")}},
			wantErr: "bad object",
		},
		{
			name:    "a working tree that cannot be read is an error, not a clean tree",
			git:     &fakeGit{worktreeErr: errors.New("index.lock exists")},
			wantErr: "index.lock exists",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := st.Changed(tc.git, "internal/beads")
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("Changed() = %q, %v; want error naming %q", got, err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Changed(): %v", err)
			}
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Changed() = %q; want %q", got, tc.want)
			}
			if len(tc.git.asked) != 1 || tc.git.asked[0] != commit1 {
				t.Errorf("asked git for %q; want the recorded commit %s", tc.git.asked, commit1)
			}
		})
	}

	if _, err := st.Changed(&fakeGit{}, "internal/testutil"); !errors.Is(err, ErrNoRecord) {
		t.Errorf("Changed(no record) = %v; want ErrNoRecord", err)
	}
}

// TestChangedRefusesANonObjectID pins the state file as input: a record whose
// commit is a flag or a revision expression is never handed to git, whose
// argument list it would change.
func TestChangedRefusesANonObjectID(t *testing.T) {
	t.Parallel()
	for _, commit := range []string{"--upload-pack=evil", "-n", "HEAD~1", "main", "c1", strings.Repeat("g", 40)} {
		st := State{"internal/beads": Record{Commit: commit, At: at(t, 2026, 10, 25, 4, 0, 0)}}
		g := &fakeGit{changes: map[string][]string{commit: {"internal/beads/x.go"}}}
		if _, err := st.Changed(g, "internal/beads"); err == nil {
			t.Errorf("Changed() over the recorded commit %q returned no error", commit)
		}
		if len(g.asked) != 0 {
			t.Errorf("asked git for %q; a commit that is not an object id must not reach git", g.asked)
		}
	}
}

func TestDue(t *testing.T) {
	t.Parallel()
	loc, err := DailyZoneLocation()
	if err != nil {
		t.Fatalf("DailyZoneLocation: %v", err)
	}
	beads := Package{Name: "internal/beads", Triggers: []string{"internal/beads"}}
	testutil := Package{Name: "internal/testutil", Triggers: []string{"internal/testutil", "go.mod"}}
	yesterday := Record{Commit: commit0, At: at(t, 2026, 10, 24, 4, 0, 0)}

	tests := []struct {
		name      string
		state     State
		pkgs      []Package
		git       *fakeGit
		want      []string
		wantAsked []string
	}{
		{
			name:  "a package with no record runs",
			state: State{},
			pkgs:  []Package{beads},
			git:   &fakeGit{},
			want:  []string{"internal/beads"},
		},
		{
			name:  "an empty state runs every package",
			state: State{},
			pkgs:  []Package{beads, testutil},
			git:   &fakeGit{},
			want:  []string{"internal/beads", "internal/testutil"},
		},
		{
			name:      "a change on a trigger path since the recorded commit runs it",
			state:     State{"internal/beads": rec(t, commit1)},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{commit1: {"internal/beads/store/schema.go"}}},
			want:      []string{"internal/beads"},
			wantAsked: []string{commit1},
		},
		{
			name:      "a change elsewhere since the recorded commit does not run it",
			state:     State{"internal/beads": rec(t, commit1)},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{commit1: {"internal/cmd/root.go"}}},
			want:      nil,
			wantAsked: []string{commit1},
		},
		{
			name:      "a change with nothing since the recorded commit does not run it",
			state:     State{"internal/beads": rec(t, commit1)},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{commit1: nil}},
			want:      nil,
			wantAsked: []string{commit1},
		},
		{
			name:      "an uncommitted change on a trigger path runs it with nothing committed",
			state:     State{"internal/beads": rec(t, commit1)},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{commit1: nil}, dirty: []string{"internal/beads/schema.go"}},
			want:      []string{"internal/beads"},
			wantAsked: []string{commit1},
		},
		{
			name:      "a working tree that cannot be read runs that package",
			state:     State{"internal/beads": rec(t, commit1)},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{commit1: nil}, worktreeErr: errors.New("index.lock exists")},
			want:      []string{"internal/beads"},
			wantAsked: []string{commit1},
		},
		{
			name:  "the daily window since the recorded run runs it without reading git",
			state: State{"internal/beads": yesterday},
			pkgs:  []Package{beads},
			git:   &fakeGit{},
			want:  []string{"internal/beads"},
		},
		{
			name:      "a run after the daily window does not run it again that day",
			state:     State{"internal/beads": rec(t, commit1)},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{commit1: nil}},
			want:      nil,
			wantAsked: []string{commit1},
		},
		{
			name: "each package reads the change since its own recorded commit",
			state: State{
				"internal/beads":    rec(t, commit1),
				"internal/testutil": rec(t, commit2),
			},
			pkgs: []Package{beads, testutil},
			git: &fakeGit{changes: map[string][]string{
				commit1: {"internal/beads/schema.go"},
				commit2: {"internal/cmd/root.go"},
			}},
			want:      []string{"internal/beads"},
			wantAsked: []string{commit1, commit2},
		},
		{
			name:      "a record whose commit cannot be read runs that package",
			state:     State{"internal/beads": rec(t, commit1)},
			pkgs:      []Package{beads},
			git:       &fakeGit{errs: map[string]error{commit1: errors.New("bad object c1")}},
			want:      []string{"internal/beads"},
			wantAsked: []string{commit1},
		},
		{
			name: "one unreadable record leaves the rest of the sweep going",
			state: State{
				"internal/beads":    rec(t, commit1),
				"internal/testutil": rec(t, commit2),
			},
			pkgs: []Package{beads, testutil},
			git: &fakeGit{
				errs:    map[string]error{commit1: errors.New("bad object c1")},
				changes: map[string][]string{commit2: {"internal/cmd/root.go"}},
			},
			want:      []string{"internal/beads"},
			wantAsked: []string{commit1, commit2},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, _ := Due(tc.state, tc.pkgs, tc.git, at(t, 2026, 10, 25, 5, 0, 0), loc)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("Due() = %q; want %q", got, tc.want)
			}
			if strings.Join(tc.git.asked, ",") != strings.Join(tc.wantAsked, ",") {
				t.Errorf("asked git for %q; want %q", tc.git.asked, tc.wantAsked)
			}
		})
	}
}

// gt-42nss: a record whose diff fails runs its package and is reported with the
// error, so a broken repository is visible instead of a silent full run.
func TestDueReportsEachUnreadableRecord(t *testing.T) {
	t.Parallel()
	loc, err := DailyZoneLocation()
	if err != nil {
		t.Fatalf("DailyZoneLocation: %v", err)
	}
	beads := Package{Name: "internal/beads", Triggers: []string{"internal/beads"}}
	state := State{"internal/beads": Record{Commit: commitGone, At: at(t, 2026, 10, 25, 4, 0, 0)}}
	git := &fakeGit{errs: map[string]error{commitGone: errors.New("unknown revision")}}
	due, unreadable := Due(state, []Package{beads}, git, at(t, 2026, 10, 25, 5, 0, 0), loc)
	if strings.Join(due, ",") != "internal/beads" {
		t.Errorf("Due() = %q; want the unreadable package to run", due)
	}
	if _, ok := unreadable["internal/beads"]; !ok {
		t.Errorf("unreadable = %v; want internal/beads named with its diff error", unreadable)
	}
}
