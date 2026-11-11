package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// fakeDirectiveSource is an in-memory directiveSource: a map of commit ids to
// their directives/ trees. A commit with noTree has no directives/ entry at all.
type fakeDirectiveSource struct {
	commits map[string]fakeCommit
}

type fakeCommit struct {
	files    []fakeEntry
	noTree   bool
	unreadOn string
}

type fakeEntry struct {
	name    string
	content string
	symlink bool
}

func (f fakeDirectiveSource) hasCommit(commit string) error {
	if _, ok := f.commits[commit]; !ok {
		return fmt.Errorf("commit %s is not in the fake", commit)
	}
	return nil
}

func (f fakeDirectiveSource) listDirectives(commit string) ([]directiveEntry, error) {
	c := f.commits[commit]
	if c.noTree {
		return nil, fmt.Errorf("commit %s has no directives/ tree", commit)
	}
	var out []directiveEntry
	for _, e := range c.files {
		out = append(out, directiveEntry{name: e.name, symlink: e.symlink})
	}
	return out, nil
}

func (f fakeDirectiveSource) readDirective(commit, name string) ([]byte, error) {
	c := f.commits[commit]
	if c.unreadOn == name {
		return nil, errors.New("fake read failure")
	}
	for _, e := range c.files {
		if e.name == name {
			return []byte(e.content), nil
		}
	}
	return nil, fmt.Errorf("no directives/%s at commit %s", name, commit)
}

// dirContents is every regular file in dir, by name, with its bytes. A missing
// dir is empty.
func dirContents(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return out
	}
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		out[e.Name()] = string(data)
	}
	return out
}

// seedDirectives writes the files a town already has deployed.
func seedDirectives(t *testing.T, town string, files map[string]string) {
	t.Helper()
	dir := CentralDirectiveDir(town)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("seed %s: %v", name, err)
		}
	}
}

func baseNames(paths []string) []string {
	names := make([]string, 0, len(paths))
	for _, p := range paths {
		names = append(names, filepath.Base(p))
	}
	sort.Strings(names)
	return names
}

func buildCommitOf(commit string) func() string {
	return func() string { return commit }
}

func TestDeployCentralDirectivesFromFakeSource(t *testing.T) {
	t.Parallel()
	const second = "2222222"
	const build = "5555555"
	tests := []struct {
		name        string
		commit      string
		buildCommit func() string
		src         fakeDirectiveSource
		seed        map[string]string
		wantErr     string
		want        map[string]string
		wantWritten []string
	}{
		{
			name:   "writes each directive and removes one the commit no longer has",
			commit: second,
			src: fakeDirectiveSource{commits: map[string]fakeCommit{second: {files: []fakeEntry{
				{name: "keep.md", content: "keep-new"},
				{name: "new.md", content: "new"},
			}}}},
			seed:        map[string]string{"keep.md": "keep-old", "stale.md": "gone", "notes.txt": "not ours"},
			want:        map[string]string{"keep.md": "keep-new", "new.md": "new", "notes.txt": "not ours"},
			wantWritten: []string{"keep.md", "new.md"},
		},
		{
			name:        "an empty commit resolves to the build commit",
			commit:      "",
			buildCommit: buildCommitOf(build),
			src: fakeDirectiveSource{commits: map[string]fakeCommit{build: {files: []fakeEntry{
				{name: "a.md", content: "A"},
			}}}},
			want:        map[string]string{"a.md": "A"},
			wantWritten: []string{"a.md"},
		},
		{
			name:        "an empty commit with no build commit is refused and writes nothing",
			commit:      "",
			buildCommit: buildCommitOf(""),
			src:         fakeDirectiveSource{},
			seed:        map[string]string{"x.md": "x"},
			wantErr:     "no commit to deploy from",
			want:        map[string]string{"x.md": "x"},
		},
		{
			name:    "a commit the source does not hold is refused, naming it, and writes nothing",
			commit:  "9999999",
			src:     fakeDirectiveSource{commits: map[string]fakeCommit{second: {}}},
			seed:    map[string]string{"x.md": "x"},
			wantErr: "9999999",
			want:    map[string]string{"x.md": "x"},
		},
		{
			name:    "a value that is not a commit id is refused before the source is read",
			commit:  "--upload-pack=sh",
			src:     fakeDirectiveSource{},
			seed:    map[string]string{"x.md": "x"},
			wantErr: "is not a commit id",
			want:    map[string]string{"x.md": "x"},
		},
		{
			name:    "a commit with no directives tree is refused and writes nothing",
			commit:  second,
			src:     fakeDirectiveSource{commits: map[string]fakeCommit{second: {noTree: true}}},
			seed:    map[string]string{"x.md": "x"},
			wantErr: "no directives/ tree",
			want:    map[string]string{"x.md": "x"},
		},
		{
			name:   "a symlink named *.md is refused by name and nothing is written",
			commit: second,
			src: fakeDirectiveSource{commits: map[string]fakeCommit{second: {files: []fakeEntry{
				{name: "a.md", content: "A"},
				{name: "link.md", content: "../../secret", symlink: true},
			}}}},
			seed:    map[string]string{"old.md": "old"},
			wantErr: "link.md",
			want:    map[string]string{"old.md": "old"},
		},
		{
			name:   "a file name that is not a flat *.md name is refused and nothing is written",
			commit: second,
			src: fakeDirectiveSource{commits: map[string]fakeCommit{second: {files: []fakeEntry{
				{name: "a.md", content: "A"},
				{name: "../escape.md", content: "E"},
			}}}},
			seed:    map[string]string{"old.md": "old"},
			wantErr: "../escape.md",
			want:    map[string]string{"old.md": "old"},
		},
		{
			name:   "a directive that cannot be read fails the deploy before any write",
			commit: second,
			src: fakeDirectiveSource{commits: map[string]fakeCommit{second: {
				unreadOn: "b.md",
				files: []fakeEntry{
					{name: "a.md", content: "A"},
					{name: "b.md", content: "B"},
				},
			}}},
			seed:    map[string]string{"old.md": "old"},
			wantErr: "b.md",
			want:    map[string]string{"old.md": "old"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			seedDirectives(t, town, tc.seed)
			build := tc.buildCommit
			if build == nil {
				build = buildCommitOf("")
			}

			written, err := deployFrom(town, tc.commit, build, tc.src, realDirFS())
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("deployFrom error = nil, want one naming %q", tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("deployFrom error = %q, want it to name %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("deployFrom: %v", err)
			}
			if got := dirContents(t, CentralDirectiveDir(town)); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("deployed directives = %v, want %v", got, tc.want)
			}
			if tc.wantWritten != nil && !reflect.DeepEqual(baseNames(written), tc.wantWritten) {
				t.Errorf("written = %v, want %v", baseNames(written), tc.wantWritten)
			}
		})
	}
}

// A second run over the commit the first run deployed changes no byte and
// writes no file.
func TestDeployCentralDirectivesSecondRunIsNoOp(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	src := fakeDirectiveSource{commits: map[string]fakeCommit{commit: {files: []fakeEntry{
		{name: "a.md", content: "A"},
		{name: "b.md", content: "B"},
	}}}}
	town := t.TempDir()

	first, err := deployFrom(town, commit, buildCommitOf(""), src, realDirFS())
	if err != nil {
		t.Fatalf("first deployFrom: %v", err)
	}
	afterFirst := dirContents(t, CentralDirectiveDir(town))

	second, err := deployFrom(town, commit, buildCommitOf(""), src, realDirFS())
	if err != nil {
		t.Fatalf("second deployFrom: %v", err)
	}
	if got := dirContents(t, CentralDirectiveDir(town)); !reflect.DeepEqual(got, afterFirst) {
		t.Errorf("second run changed the deployed bytes: %v, want %v", got, afterFirst)
	}
	if len(first) != 2 {
		t.Errorf("first run wrote %v, want both files", baseNames(first))
	}
	if len(second) != 0 {
		t.Errorf("second run wrote %v, want nothing", baseNames(second))
	}
}

// A deployed directive that is a symlink is replaced by a regular file with the
// commit's bytes, even when the link's target already holds those bytes: the
// write does not follow the link.
func TestDeployCentralDirectivesReplacesALinkedDirective(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	src := fakeDirectiveSource{commits: map[string]fakeCommit{commit: {files: []fakeEntry{
		{name: "a.md", content: "A"},
	}}}}
	town := t.TempDir()
	dir := CentralDirectiveDir(town)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	target := filepath.Join(town, "elsewhere.md")
	if err := os.WriteFile(target, []byte("A"), 0o644); err != nil {
		t.Fatalf("write link target: %v", err)
	}
	if err := os.Symlink(target, filepath.Join(dir, "a.md")); err != nil {
		t.Fatalf("link a.md: %v", err)
	}

	written, err := deployFrom(town, commit, buildCommitOf(""), src, realDirFS())
	if err != nil {
		t.Fatalf("deployFrom: %v", err)
	}
	if len(written) != 1 {
		t.Errorf("written = %v, want a.md rewritten over the link", baseNames(written))
	}
	fi, err := os.Lstat(filepath.Join(dir, "a.md"))
	if err != nil {
		t.Fatalf("stat a.md: %v", err)
	}
	if !fi.Mode().IsRegular() {
		t.Errorf("a.md mode = %v, want a regular file in place of the link", fi.Mode())
	}
}

// The default source reads the gastown bare repo under the town, so the deploy
// names the one path the install and the config share.
func TestDeployCentralDirectivesDefaultSourceIsTheGastownRepo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	src, ok := defaultDirectiveSource(town).(gitDirectiveSource)
	if !ok {
		t.Fatalf("defaultDirectiveSource = %T, want the git-backed source", defaultDirectiveSource(town))
	}
	want := filepath.Join(town, "gastown", ".repo.git")
	if src.repo != want {
		t.Errorf("default source repo = %q, want %q", src.repo, want)
	}
	if got := CentralDirectiveRepo(town); got != want {
		t.Errorf("CentralDirectiveRepo = %q, want %q", got, want)
	}
}
