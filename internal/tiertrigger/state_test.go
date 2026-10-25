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
	files    map[string][]byte
	readErr  error
	writeErr error
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

// fakeGit is the repository the due decision reads: changes and errs are keyed
// by the commit a package recorded, so a test sees which base was asked for
// and can fail one package's diff alone.
type fakeGit struct {
	changes map[string][]string
	errs    map[string]error
	asked   []string
}

func (g *fakeGit) DiffNameOnly(base, _ string) ([]string, error) {
	g.asked = append(g.asked, base)
	if err := g.errs[base]; err != nil {
		return nil, err
	}
	return g.changes[base], nil
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
	st.Ran("internal/beads", "c1", at(t, 2026, 10, 25, 4, 0, 0))
	g := &fakeGit{changes: map[string][]string{"c1": {"internal/beads/schema.go"}}}
	got, err := st.Changed(g, "internal/beads")
	if err != nil {
		t.Fatalf("Changed(): %v", err)
	}
	if len(got) != 1 || got[0] != "internal/beads/schema.go" {
		t.Errorf("Changed() = %q; want the change since c1", got)
	}
	if len(g.asked) != 1 || g.asked[0] != "c1" {
		t.Errorf("asked git for %q; want the recorded commit c1", g.asked)
	}

	if _, err := st.Changed(g, "internal/testutil"); !errors.Is(err, ErrNoRecord) {
		t.Errorf("Changed(no record) = %v; want ErrNoRecord", err)
	}
	if _, err := st.Changed(&fakeGit{errs: map[string]error{"c1": errors.New("bad object")}}, "internal/beads"); err == nil {
		t.Error("Changed() over a failing git returned nil")
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
	yesterday := Record{Commit: "c0", At: at(t, 2026, 10, 24, 4, 0, 0)}

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
			state:     State{"internal/beads": rec(t, "c1")},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{"c1": {"internal/beads/store/schema.go"}}},
			want:      []string{"internal/beads"},
			wantAsked: []string{"c1"},
		},
		{
			name:      "a change elsewhere since the recorded commit does not run it",
			state:     State{"internal/beads": rec(t, "c1")},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{"c1": {"internal/cmd/root.go"}}},
			want:      nil,
			wantAsked: []string{"c1"},
		},
		{
			name:      "a change with nothing since the recorded commit does not run it",
			state:     State{"internal/beads": rec(t, "c1")},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{"c1": nil}},
			want:      nil,
			wantAsked: []string{"c1"},
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
			state:     State{"internal/beads": rec(t, "c1")},
			pkgs:      []Package{beads},
			git:       &fakeGit{changes: map[string][]string{"c1": nil}},
			want:      nil,
			wantAsked: []string{"c1"},
		},
		{
			name: "each package reads the change since its own recorded commit",
			state: State{
				"internal/beads":    rec(t, "c1"),
				"internal/testutil": rec(t, "c2"),
			},
			pkgs: []Package{beads, testutil},
			git: &fakeGit{changes: map[string][]string{
				"c1": {"internal/beads/schema.go"},
				"c2": {"internal/cmd/root.go"},
			}},
			want:      []string{"internal/beads"},
			wantAsked: []string{"c1", "c2"},
		},
		{
			name:      "a record whose commit cannot be read runs that package",
			state:     State{"internal/beads": rec(t, "c1")},
			pkgs:      []Package{beads},
			git:       &fakeGit{errs: map[string]error{"c1": errors.New("bad object c1")}},
			want:      []string{"internal/beads"},
			wantAsked: []string{"c1"},
		},
		{
			name: "one unreadable record leaves the rest of the sweep going",
			state: State{
				"internal/beads":    rec(t, "c1"),
				"internal/testutil": rec(t, "c2"),
			},
			pkgs: []Package{beads, testutil},
			git: &fakeGit{
				errs:    map[string]error{"c1": errors.New("bad object c1")},
				changes: map[string][]string{"c2": {"internal/cmd/root.go"}},
			},
			want:      []string{"internal/beads"},
			wantAsked: []string{"c1", "c2"},
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
	state := State{"internal/beads": Record{Commit: "gone", At: at(t, 2026, 10, 25, 4, 0, 0)}}
	git := &fakeGit{errs: map[string]error{"gone": errors.New("unknown revision")}}
	due, unreadable := Due(state, []Package{beads}, git, at(t, 2026, 10, 25, 5, 0, 0), loc)
	if strings.Join(due, ",") != "internal/beads" {
		t.Errorf("Due() = %q; want the unreadable package to run", due)
	}
	if _, ok := unreadable["internal/beads"]; !ok {
		t.Errorf("unreadable = %v; want internal/beads named with its diff error", unreadable)
	}
}
