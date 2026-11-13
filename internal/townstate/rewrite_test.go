package townstate

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

const (
	rewriteOldRoot  = "/Users/sloan/gt"
	rewriteNewRoot  = "/town"
	rewriteOldOwner = "sloan"
	rewriteNewOwner = "gastown"
)

// errInjected is the failure the pointer tests inject; they match it with errors.Is.
var errInjected = errors.New("injected failure")

// rewriteQuery is the query every rewrite test runs: the old root and owner this
// town's files spell, and the ones the rename (gt-ipp4uy) sets.
func rewriteQuery() RewriteQuery {
	return RewriteQuery{
		FromRoot:  rewriteOldRoot,
		ToRoot:    rewriteNewRoot,
		FromOwner: rewriteOldOwner,
		ToOwner:   rewriteNewOwner,
	}
}

// rewriteTownFile is one file a rewrite test writes into its scratch town.
type rewriteTownFile struct {
	path string
	data string
}

// writeRewriteTown writes files into a new scratch town and returns its root.
func writeRewriteTown(t *testing.T, files []rewriteTownFile) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range files {
		full := filepath.Join(root, filepath.FromSlash(f.path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(full), err)
		}
		if err := os.WriteFile(full, []byte(f.data), 0o644); err != nil {
			t.Fatalf("writing %s: %v", f.path, err)
		}
	}
	return root
}

// readRewriteFile reads one file of the scratch town.
func readRewriteFile(t *testing.T, root, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("reading %s: %v", rel, err)
	}
	return string(data)
}

// rewriteClassTown holds one file of every listed class, plus files the pass
// leaves alone. Every file carries the old root; the settings file also carries
// the owner in both repository forms, and the settings, mayor and daemon files
// carry names that merely start with the old root's characters.
func rewriteClassTown() []rewriteTownFile {
	old := rewriteOldRoot
	return []rewriteTownFile{
		// The listed classes.
		{"settings/config.json", `{"town_root":"` + old + `","landing":"` + old + `/landing","url":"http://forgejo:3000/sloan/gastown.git","repo":"sloan/gt-landing","email":"sloan.ahrens@gmail.com","near":"sloanx/gt","host":"forgejo:3000/sloanx/gt"}`},
		{"mayor/town.json", `{"registry":{"rigs":{"gastown":{"git_url":"http://forgejo:3000/sloan/gastown.git"}}}}`},
		{"daemon/config.yaml", "town_root: " + old + "\n"},
		{"gastown/settings/config.json", `{"dir":"` + old + `/gastown"}`},
		{"gastown/config.json", `{"dir":"` + old + `"}`},
		{"gastown/.beads/redirect", old + "/gastown/mayor/rig/.beads\n"},
		{".beads/routes.jsonl", `{"path":"` + old + `/gastown/mayor/rig","prefix":"gt-"}` + "\n"},
		{".beads/metadata.json", `{"dolt_database":"hq","dir":"` + old + `"}`},
		{"gastown/crew/eagle/.git", "gitdir: " + old + "/gastown/.repo.git/worktrees/eagle\n"},
		{"gastown/.repo.git/worktrees/eagle/gitdir", old + "/gastown/crew/eagle/.git\n"},
		{"gastown/.repo.git/worktrees/eagle/HEAD", "ref: refs/heads/main\n"},

		// A path that merely starts with the old root's characters.
		{"daemon/patrols.yaml", "sibling: " + old + "-town\n"},

		// Outside the listed classes.
		{"logs/daemon.log", old + " started\n"},
		{"gastown/crew/eagle/notes.md", old + "\n"},
		{".runtime/usage/run.json", `{"dir":"` + old + `"}`},
	}
}

// rewriteClassCounts is the replacement count of each class file, keyed by path.
func rewriteClassCounts() map[string]int {
	return map[string]int{
		"settings/config.json":                     4,
		"mayor/town.json":                          1,
		"daemon/config.yaml":                       1,
		"gastown/settings/config.json":             1,
		"gastown/config.json":                      1,
		"gastown/.beads/redirect":                  1,
		".beads/routes.jsonl":                      1,
		".beads/metadata.json":                     1,
		"gastown/crew/eagle/.git":                  1,
		"gastown/.repo.git/worktrees/eagle/gitdir": 1,
	}
}

// TestRewriteReplacesRootAndOwnerOnlyInTheListedClasses drives a scratch town
// with one file of every class: the pass replaces the old root and owner in
// each, leaves a name that merely starts with the old root's characters and
// every file outside the classes alone, and a second apply refuses on the kept
// copies the first one left, writing nothing.
func TestRewriteReplacesRootAndOwnerOnlyInTheListedClasses(t *testing.T) {
	t.Parallel()
	root := writeRewriteTown(t, rewriteClassTown())

	report, err := Rewrite(root, rewriteQuery(), true)
	if err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	got := map[string]int{}
	for _, f := range report.Files {
		got[f.Path] = f.Replacements
	}
	if want := rewriteClassCounts(); !mapsEqual(got, want) {
		t.Errorf("Rewrite changed %v, want %v", got, want)
	}

	for rel, want := range map[string]string{
		"settings/config.json":                     `{"town_root":"` + rewriteNewRoot + `","landing":"` + rewriteNewRoot + `/landing","url":"http://forgejo:3000/gastown/gastown.git","repo":"gastown/gt-landing","email":"sloan.ahrens@gmail.com","near":"sloanx/gt","host":"forgejo:3000/sloanx/gt"}`,
		"mayor/town.json":                          `{"registry":{"rigs":{"gastown":{"git_url":"http://forgejo:3000/gastown/gastown.git"}}}}`,
		"daemon/config.yaml":                       "town_root: " + rewriteNewRoot + "\n",
		"gastown/settings/config.json":             `{"dir":"` + rewriteNewRoot + `/gastown"}`,
		"gastown/config.json":                      `{"dir":"` + rewriteNewRoot + `"}`,
		"gastown/.beads/redirect":                  rewriteNewRoot + "/gastown/mayor/rig/.beads\n",
		".beads/routes.jsonl":                      `{"path":"` + rewriteNewRoot + `/gastown/mayor/rig","prefix":"gt-"}` + "\n",
		".beads/metadata.json":                     `{"dolt_database":"hq","dir":"` + rewriteNewRoot + `"}`,
		"gastown/crew/eagle/.git":                  "gitdir: " + rewriteNewRoot + "/gastown/.repo.git/worktrees/eagle\n",
		"gastown/.repo.git/worktrees/eagle/gitdir": rewriteNewRoot + "/gastown/crew/eagle/.git\n",
		// A name that starts with the old root's characters and a file outside
		// the classes are both left byte for byte.
		"daemon/patrols.yaml":         "sibling: " + rewriteOldRoot + "-town\n",
		"logs/daemon.log":             rewriteOldRoot + " started\n",
		"gastown/crew/eagle/notes.md": rewriteOldRoot + "\n",
		".runtime/usage/run.json":     `{"dir":"` + rewriteOldRoot + `"}`,
	} {
		if got := readRewriteFile(t, root, rel); got != want {
			t.Errorf("%s =\n%s\nwant\n%s", rel, got, want)
		}
	}

	// The owner is only replaced in its two repository forms.
	settings := readRewriteFile(t, root, "settings/config.json")
	for _, left := range []string{"sloan.ahrens@gmail.com", `"sloanx/gt"`, "forgejo:3000/sloanx/gt"} {
		if !strings.Contains(settings, left) {
			t.Errorf("the settings lost a value that is not one of the owner forms: %s", left)
		}
	}

	backups := rewriteBackups(t, root)
	if len(backups) != len(rewriteClassCounts()) {
		t.Errorf("apply kept %d copies, want %d", len(backups), len(rewriteClassCounts()))
	}

	// A second apply refuses on the kept copies, and writes nothing.
	before := rewriteTree(t, root)
	if _, err := Rewrite(root, rewriteQuery(), true); err == nil || !strings.Contains(err.Error(), "kept copies") {
		t.Errorf("the second apply = %v, want a refusal naming the kept copies", err)
	}
	if after := rewriteTree(t, root); !mapsEqual(after, before) {
		t.Error("the second apply wrote the town")
	}
}

// TestRewriteUndoRestoresTheKeptCopies drives the undo of an applied rewrite:
// every class file comes back byte for byte, the kept copies are gone, and a
// kept copy left beside a file a new run would change refuses that run before it
// writes anything.
func TestRewriteUndoRestoresTheKeptCopies(t *testing.T) {
	t.Parallel()
	root := writeRewriteTown(t, rewriteClassTown())
	before := rewriteTree(t, root)

	if _, err := Rewrite(root, rewriteQuery(), true); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	report, err := Undo(root)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if len(report.Undone) != len(rewriteClassCounts()) {
		t.Errorf("Undo restored %d files, want %d", len(report.Undone), len(rewriteClassCounts()))
	}
	if after := rewriteTree(t, root); !mapsEqual(after, before) {
		t.Errorf("Undo did not restore the town byte for byte:\n got %v\nwant %v", keysOf(after), keysOf(before))
	}
	if backups := rewriteBackups(t, root); len(backups) != 0 {
		t.Errorf("Undo left the kept copies %v", backups)
	}

	// A kept copy anywhere in the town refuses the run, and no file the run would
	// change is written.
	stray := filepath.Join(root, "settings", "config.json"+RewriteBackupSuffix)
	if err := os.WriteFile(stray, []byte("kept by hand\n"), 0o644); err != nil {
		t.Fatalf("writing the kept copy: %v", err)
	}
	_, err = Rewrite(root, rewriteQuery(), true)
	if err == nil || !strings.Contains(err.Error(), "settings/config.json.pre-rewrite") {
		t.Fatalf("Rewrite with a kept copy = %v, want a refusal naming settings/config.json.pre-rewrite", err)
	}
	if got := readRewriteFile(t, root, "gastown/config.json"); got != `{"dir":"`+rewriteOldRoot+`"}` {
		t.Errorf("the refused run wrote gastown/config.json: %s", got)
	}
	if got, err := os.ReadFile(stray); err != nil || string(got) != "kept by hand\n" {
		t.Errorf("the refused run overwrote the kept copy: %q, %v", got, err)
	}
}

// TestRewriteRefusesAKeptCopyWhereItWouldWriteNothing plants a kept copy where a
// rewrite would otherwise write nothing, beside a file of a skipped class, and in
// a nested rig directory. A dry run and an apply both refuse, naming the copy,
// and neither writes the town.
func TestRewriteRefusesAKeptCopyWhereItWouldWriteNothing(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		files []rewriteTownFile
		kept  string
	}{
		{
			name: "beside a file with nothing to change",
			files: []rewriteTownFile{
				{"gastown/config.json", `{"dir":"/town"}`},
				{"gastown/config.json.pre-rewrite", `{"dir":"` + rewriteOldRoot + `"}`},
				{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
			},
			kept: "gastown/config.json.pre-rewrite",
		},
		{
			name: "beside a file of a skipped class",
			files: []rewriteTownFile{
				{"settings/notes.txt", rewriteOldRoot + " notes\n"},
				{"settings/notes.txt.pre-rewrite", rewriteOldRoot + " notes\n"},
				{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
			},
			kept: "settings/notes.txt.pre-rewrite",
		},
		{
			name: "in a nested rig directory",
			files: []rewriteTownFile{
				{"gastown/crew/eagle/notes.md.pre-rewrite", "kept\n"},
				{"gastown/settings/config.json", `{"dir":"` + rewriteOldRoot + `"}`},
			},
			kept: "gastown/crew/eagle/notes.md.pre-rewrite",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeRewriteTown(t, tc.files)
			before := rewriteTree(t, root)
			for _, apply := range []bool{false, true} {
				if _, err := Rewrite(root, rewriteQuery(), apply); err == nil || !strings.Contains(err.Error(), tc.kept) {
					t.Fatalf("Rewrite(apply=%v) = %v, want a refusal naming %s", apply, err, tc.kept)
				}
			}
			if after := rewriteTree(t, root); !mapsEqual(after, before) {
				t.Error("the refused runs wrote the town")
			}
		})
	}
}

// TestRewriteRefusesAStrayKeptCopyAnywhere plants a kept copy in each place the
// kept-copy search enters: the logs, events and runtime directories the rewrite
// walk skips, and a directory six levels down. A dry run and an apply both refuse
// on it, naming its path, and write nothing.
func TestRewriteRefusesAStrayKeptCopyAnywhere(t *testing.T) {
	t.Parallel()
	for _, kept := range []string{
		"logs/daemon.log.pre-rewrite",
		"events/feed.pre-rewrite",
		".runtime/usage/run.json.pre-rewrite",
		"a/b/c/d/e/f/notes.md.pre-rewrite",
	} {
		t.Run(kept, func(t *testing.T) {
			t.Parallel()
			root := writeRewriteTown(t, []rewriteTownFile{
				{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
				{kept, "kept\n"},
			})
			before := rewriteTree(t, root)
			for _, apply := range []bool{false, true} {
				_, err := Rewrite(root, rewriteQuery(), apply)
				if err == nil || !strings.Contains(err.Error(), kept) ||
					!strings.Contains(err.Error(), "a previous rewrite was not undone or cleaned: run --undo or delete them") {
					t.Fatalf("Rewrite(apply=%v) = %v, want a refusal naming %s", apply, err, kept)
				}
			}
			if after := rewriteTree(t, root); !mapsEqual(after, before) {
				t.Error("the refused runs wrote the town")
			}
		})
	}
}

// TestRewriteSkipsTheGitAndDoltTreesForKeptCopies plants a kept copy in the two
// trees the kept-copy search does not enter, a git directory and a Dolt data
// directory. The run proceeds and rewrites the class file.
func TestRewriteSkipsTheGitAndDoltTreesForKeptCopies(t *testing.T) {
	t.Parallel()
	for _, kept := range []string{".git/config.pre-rewrite", ".dolt-data-x/table.pre-rewrite"} {
		t.Run(kept, func(t *testing.T) {
			t.Parallel()
			root := writeRewriteTown(t, []rewriteTownFile{
				{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
				{kept, "kept\n"},
			})
			report, err := Rewrite(root, rewriteQuery(), true)
			if err != nil {
				t.Fatalf("Rewrite with a kept copy in %s = %v, want the run to proceed", kept, err)
			}
			if len(report.Files) != 1 {
				t.Errorf("Rewrite changed %d files, want settings/config.json alone", len(report.Files))
			}
			if got := readRewriteFile(t, root, "settings/config.json"); got != `{"town_root":"`+rewriteNewRoot+`"}` {
				t.Errorf("the run did not rewrite settings/config.json: %s", got)
			}
		})
	}
}

// TestRewriteDoesNotEnterASymlinkedDirectory reaches a kept copy only through a
// symlink to a directory outside the town. The search does not follow the link,
// so the run proceeds.
func TestRewriteDoesNotEnterASymlinkedDirectory(t *testing.T) {
	t.Parallel()
	root := writeRewriteTown(t, []rewriteTownFile{
		{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
	})
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "notes.pre-rewrite"), []byte("kept\n"), 0o644); err != nil {
		t.Fatalf("writing the kept copy outside the town: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Fatalf("planting the symlink: %v", err)
	}
	report, err := Rewrite(root, rewriteQuery(), true)
	if err != nil {
		t.Fatalf("Rewrite past a symlinked directory = %v, want the run to proceed", err)
	}
	if len(report.Files) != 1 {
		t.Errorf("Rewrite changed %d files, want settings/config.json alone", len(report.Files))
	}
}

// TestUndoRestoresAMissingRewrittenFile removes a rewritten file between the
// apply and the undo. The undo creates it again from its kept copy, with the
// original bytes and mode, reports it as recreated, and removes the copy.
func TestUndoRestoresAMissingRewrittenFile(t *testing.T) {
	t.Parallel()
	const (
		rel      = "settings/config.json"
		original = `{"town_root":"` + rewriteOldRoot + `"}`
	)
	root := writeRewriteTown(t, []rewriteTownFile{{rel, original}})
	full := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(full)
	if err != nil {
		t.Fatalf("reading the mode of %s: %v", rel, err)
	}
	if _, err := Rewrite(root, rewriteQuery(), true); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	if err := os.Remove(full); err != nil {
		t.Fatalf("removing the rewritten file: %v", err)
	}

	report, err := Undo(root)
	if err != nil {
		t.Fatalf("Undo: %v", err)
	}
	if want := []RewriteRestored{{Path: rel, Recreated: true}}; !slices.Equal(report.Undone, want) {
		t.Errorf("Undo reported %+v, want %+v", report.Undone, want)
	}
	if got := readRewriteFile(t, root, rel); got != original {
		t.Errorf("Undo recreated %s with %q, want the original bytes", rel, got)
	}
	after, err := os.Stat(full)
	if err != nil {
		t.Fatalf("reading the recreated %s: %v", rel, err)
	}
	if after.Mode().Perm() != info.Mode().Perm() {
		t.Errorf("Undo recreated %s with mode %v, want %v", rel, after.Mode().Perm(), info.Mode().Perm())
	}
	if backups := rewriteBackups(t, root); len(backups) != 0 {
		t.Errorf("Undo left the kept copies %v", backups)
	}
}

// TestRewriteOwnerMatchesOnlyAWholeAccountName runs the owner replacement over
// each repository form beside a longer name, a name that contains the owner, and
// a host that ends in the form's text. Only the whole owner in a form is replaced.
func TestRewriteOwnerMatchesOnlyAWholeAccountName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
		n    int
	}{
		{name: "forgejo form", in: `forgejo:3000/sloan/gt`, want: `forgejo:3000/gastown/gt`, n: 1},
		{name: "ssh form", in: `git@forgejo:3000/sloan/gt.git`, want: `git@forgejo:3000/gastown/gt.git`, n: 1},
		{name: "quoted form", in: `"sloan/gt"`, want: `"gastown/gt"`, n: 1},
		{name: "longer name after the forgejo form", in: `forgejo:3000/sloanx/gt`, want: `forgejo:3000/sloanx/gt`},
		{name: "name ending in the owner", in: `forgejo:3000/xsloan/gt`, want: `forgejo:3000/xsloan/gt`},
		{name: "longer name in the quoted form", in: `"sloanx/gt`, want: `"sloanx/gt`},
		{name: "name ending in the owner before a quote", in: `x"sloan/gt`, want: `x"sloan/gt`},
		{name: "host ending in the forgejo form", in: `myforgejo:3000/sloan/gt`, want: `myforgejo:3000/sloan/gt`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, n := rewriteContent([]byte(tc.in), rewriteQuery())
			if string(got) != tc.want || n != tc.n {
				t.Errorf("rewriteContent(%q) = %q, %d; want %q, %d", tc.in, got, n, tc.want, tc.n)
			}
		})
	}
}

// writeError is a file whose write fails, and closeError is one whose close
// fails after a good write; both stand in for a disk that stops taking bytes.
type writeError struct{ closeErr error }

func (w writeError) Write([]byte) (int, error) { return 0, errors.New("write refused") }
func (w writeError) Close() error              { return w.closeErr }

// closeOnlyError is a file that takes every byte and fails when it is closed.
type closeOnlyError struct{ err error }

func (w closeOnlyError) Write(p []byte) (int, error) { return len(p), nil }
func (w closeOnlyError) Close() error                { return w.err }

// atOpen returns a write seam edit that replaces the open of the one file whose
// path ends in suffix with open; every other open is the host's.
func atOpen(suffix string, open func() (io.WriteCloser, error)) func(*rewriteFiles) {
	return func(f *rewriteFiles) {
		host := f.openFile
		f.openFile = func(p string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
			if strings.HasSuffix(p, suffix) {
				return open()
			}
			return host(p, flag, perm)
		}
	}
}

// TestRewriteRefusesAWriteThatFails drives every write step of an apply: a
// failure at any of them is returned, and the class file keeps its old bytes.
// Each injected failure is keyed by path, so it reaches only the step it names.
func TestRewriteRefusesAWriteThatFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := []struct {
		name string
		edit func(*rewriteFiles, string)
		want string
	}{
		{
			name: "reading the file's mode",
			edit: func(f *rewriteFiles, root string) {
				target := filepath.Join(root, "settings", "config.json")
				host := f.lstat
				f.lstat = func(p string) (fs.FileInfo, error) {
					if p == target {
						return nil, boom
					}
					return host(p)
				}
			},
			want: "reading settings/config.json",
		},
		{
			name: "kept copy open",
			edit: func(f *rewriteFiles, _ string) {
				atOpen(RewriteBackupSuffix, func() (io.WriteCloser, error) { return nil, boom })(f)
			},
			want: "keeping a copy of settings/config.json as config.json.pre-rewrite",
		},
		{
			name: "kept copy write",
			edit: func(f *rewriteFiles, _ string) {
				atOpen(RewriteBackupSuffix, func() (io.WriteCloser, error) { return writeError{}, nil })(f)
			},
			want: "keeping a copy of settings/config.json as config.json.pre-rewrite",
		},
		{
			name: "kept copy write and close",
			edit: func(f *rewriteFiles, _ string) {
				atOpen(RewriteBackupSuffix, func() (io.WriteCloser, error) { return writeError{closeErr: boom}, nil })(f)
			},
			want: "keeping a copy of settings/config.json as config.json.pre-rewrite",
		},
		{
			name: "kept copy close",
			edit: func(f *rewriteFiles, _ string) {
				atOpen(RewriteBackupSuffix, func() (io.WriteCloser, error) { return closeOnlyError{err: boom}, nil })(f)
			},
			want: "keeping a copy of settings/config.json as config.json.pre-rewrite",
		},
		{
			name: "new bytes open",
			edit: func(f *rewriteFiles, _ string) {
				atOpen(rewriteTempSuffix, func() (io.WriteCloser, error) { return nil, boom })(f)
			},
			want: "writing settings/config.json",
		},
		{
			name: "new bytes write",
			edit: func(f *rewriteFiles, _ string) {
				atOpen(rewriteTempSuffix, func() (io.WriteCloser, error) { return writeError{}, nil })(f)
			},
			want: "writing settings/config.json",
		},
		{
			name: "new bytes close",
			edit: func(f *rewriteFiles, _ string) {
				atOpen(rewriteTempSuffix, func() (io.WriteCloser, error) { return closeOnlyError{err: boom}, nil })(f)
			},
			want: "writing settings/config.json",
		},
		{
			name: "rename over the file",
			edit: func(f *rewriteFiles, _ string) {
				f.rename = func(string, string) error { return boom }
			},
			want: "replacing settings/config.json",
		},
		{
			name: "removing the temp file",
			edit: func(f *rewriteFiles, _ string) {
				f.rename = func(string, string) error { return boom }
				f.remove = func(string) error { return boom }
			},
			want: "removing config.json.rewrite-tmp also failed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeRewriteTown(t, []rewriteTownFile{
				{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
			})
			files := hostRewriteFiles()
			tc.edit(&files, root)
			_, err := rewriteWith(os.DirFS(root), root, rewriteQuery(), true, files)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rewriteWith = %v, want a refusal naming %q", err, tc.want)
			}
			if got := readRewriteFile(t, root, "settings/config.json"); got != `{"town_root":"`+rewriteOldRoot+`"}` {
				t.Errorf("the failed write changed the class file: %s", got)
			}
		})
	}
}

// partialWriter is a created file that takes no bytes, as a disk that fills
// before the first write does.
type partialWriter struct{ io.WriteCloser }

func (partialWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

// TestRewriteRemovesAPartialFileItCreated drives a write that fails after the pass
// has created the temp file: the file is removed, so no partial copy is left, and
// the class file keeps its old bytes.
func TestRewriteRemovesAPartialFileItCreated(t *testing.T) {
	t.Parallel()
	root := writeRewriteTown(t, []rewriteTownFile{
		{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
	})
	files := hostRewriteFiles()
	host := files.openFile
	files.openFile = func(p string, flag int, perm fs.FileMode) (io.WriteCloser, error) {
		f, err := host(p, flag, perm)
		if err != nil || !strings.HasSuffix(p, rewriteTempSuffix) {
			return f, err
		}
		return partialWriter{f}, nil
	}
	_, err := rewriteWith(os.DirFS(root), root, rewriteQuery(), true, files)
	if err == nil || !strings.Contains(err.Error(), "writing settings/config.json") {
		t.Fatalf("rewriteWith = %v, want a refusal naming settings/config.json", err)
	}
	if _, err := os.Stat(filepath.Join(root, "settings", "config.json"+rewriteTempSuffix)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the failed write left its temp file: %v", err)
	}
	if got := readRewriteFile(t, root, "settings/config.json"); got != `{"town_root":"`+rewriteOldRoot+`"}` {
		t.Errorf("the failed write changed the class file: %s", got)
	}
}

// TestRewriteRefusesALinkAtAnExclusiveName plants a link where the pass creates a
// file. At the temp name the new bytes are built in, the apply refuses without
// writing through the link. At the kept name the run refuses before it writes.
// The link's target is never touched.
func TestRewriteRefusesALinkAtAnExclusiveName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		link string
		want string
	}{
		{name: "temp name", link: "settings/config.json.rewrite-tmp", want: "writing settings/config.json"},
		{name: "kept name", link: "settings/config.json.pre-rewrite", want: "kept copies"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeRewriteTown(t, []rewriteTownFile{
				{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
			})
			target := filepath.Join(t.TempDir(), "outside.txt")
			const outside = "outside the town\n"
			if err := os.WriteFile(target, []byte(outside), 0o644); err != nil {
				t.Fatalf("writing the link target: %v", err)
			}
			if err := os.Symlink(target, filepath.Join(root, filepath.FromSlash(tc.link))); err != nil {
				t.Fatalf("planting the link: %v", err)
			}
			if _, err := Rewrite(root, rewriteQuery(), true); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Rewrite = %v, want a refusal naming %q", err, tc.want)
			}
			if got, err := os.ReadFile(target); err != nil || string(got) != outside {
				t.Errorf("the link's target changed: %q, %v", got, err)
			}
			if got := readRewriteFile(t, root, "settings/config.json"); got != `{"town_root":"`+rewriteOldRoot+`"}` {
				t.Errorf("the refused run changed the class file: %s", got)
			}
		})
	}
}

// TestUndoRefusesAKeptNameThatIsNotARegularFile drives the undo over a kept name
// a link has replaced: the link is not restored over the class file, and its
// target is never touched.
func TestUndoRefusesAKeptNameThatIsNotARegularFile(t *testing.T) {
	t.Parallel()
	root := writeRewriteTown(t, []rewriteTownFile{
		{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
	})
	if _, err := Rewrite(root, rewriteQuery(), true); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	target := filepath.Join(t.TempDir(), "outside.txt")
	const outside = "outside the town\n"
	if err := os.WriteFile(target, []byte(outside), 0o644); err != nil {
		t.Fatalf("writing the link target: %v", err)
	}
	kept := filepath.Join(root, "settings", "config.json"+RewriteBackupSuffix)
	if err := os.Remove(kept); err != nil {
		t.Fatalf("removing the kept copy: %v", err)
	}
	if err := os.Symlink(target, kept); err != nil {
		t.Fatalf("planting the link: %v", err)
	}

	if _, err := Undo(root); err == nil || !strings.Contains(err.Error(), "is not a regular file") {
		t.Fatalf("Undo = %v, want a refusal naming the kept copy", err)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != outside {
		t.Errorf("the undo changed the link's target: %q, %v", got, err)
	}
	if got := readRewriteFile(t, root, "settings/config.json"); got != `{"town_root":"`+rewriteNewRoot+`"}` {
		t.Errorf("the refused undo restored over the class file: %s", got)
	}
}

// TestUndoRefusesARestoreThatFails drives the undo's rename step: a failure is
// returned, and the kept copy stays where it is.
func TestUndoRefusesARestoreThatFails(t *testing.T) {
	t.Parallel()
	root := writeRewriteTown(t, []rewriteTownFile{
		{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
	})
	if _, err := Rewrite(root, rewriteQuery(), true); err != nil {
		t.Fatalf("Rewrite: %v", err)
	}
	files := hostRewriteFiles()
	files.rename = func(string, string) error { return errors.New("boom") }
	_, err := undoWith(os.DirFS(root), root, files)
	if err == nil || !strings.Contains(err.Error(), "restoring settings/config.json") {
		t.Fatalf("undoWith = %v, want a refusal naming settings/config.json", err)
	}
	if _, err := os.Stat(filepath.Join(root, "settings", "config.json"+RewriteBackupSuffix)); err != nil {
		t.Errorf("the failed undo removed the kept copy: %v", err)
	}

	// A kept copy the undo cannot even stat is refused, not read as absent.
	files = hostRewriteFiles()
	files.lstat = func(string) (fs.FileInfo, error) { return nil, errors.New("boom") }
	if _, err := undoWith(os.DirFS(root), root, files); err == nil || !strings.Contains(err.Error(), "checking for a kept copy of settings/config.json") {
		t.Errorf("undoWith with an unreadable kept copy = %v, want a refusal naming it", err)
	}
}

// TestRewriteQueryValidate drives every refusal of a rewrite query.
func TestRewriteQueryValidate(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		edit func(*RewriteQuery)
		want string
	}{
		{name: "no from-root", edit: func(q *RewriteQuery) { q.FromRoot = "" }, want: "--from-root is required"},
		{name: "no to-root", edit: func(q *RewriteQuery) { q.ToRoot = "" }, want: "--to-root is required"},
		{name: "equal roots", edit: func(q *RewriteQuery) { q.ToRoot = q.FromRoot }, want: "are the same"},
		{name: "new root under the old", edit: func(q *RewriteQuery) { q.ToRoot = q.FromRoot + "/town" }, want: "is under --from-root"},
		{name: "relative from-root", edit: func(q *RewriteQuery) { q.FromRoot = "gt" }, want: "is not an absolute path"},
		{name: "slash on to-root", edit: func(q *RewriteQuery) { q.ToRoot = "/town/" }, want: "ends with a slash"},
		{name: "one owner", edit: func(q *RewriteQuery) { q.ToOwner = "" }, want: "given together"},
		{name: "owner shape", edit: func(q *RewriteQuery) { q.FromOwner = "Sloan" }, want: "not one lowercase account name"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			q := rewriteQuery()
			tc.edit(&q)
			if err := q.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Validate = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestRewriteRefusesWhatItCannotWalk drives the town and walk failures: a root
// that is missing, relative or not a directory, a root whose listing fails, a
// walk that fails inside a rig, a class file that cannot be read, and the bare
// repository's worktree listing when it cannot be read.
func TestRewriteRefusesWhatItCannotWalk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatalf("writing the file: %v", err)
	}
	boom := errors.New("boom")
	classTown := fstest.MapFS{"settings/config.json": {Data: []byte(`{"town_root":"` + rewriteOldRoot + `"}`)}}
	walkTown := fstest.MapFS{"rig/settings/config.json": {Data: []byte(`{}`)}}
	repoTown := fstest.MapFS{"rig/.repo.git/worktrees/w/gitdir": {Data: []byte(rewriteOldRoot + "\n")}}

	cases := []struct {
		name string
		root string
		fsys fs.FS
		want string
	}{
		{name: "no town", root: "", fsys: os.DirFS(dir), want: "--town is required"},
		{name: "relative town", root: "gt", fsys: os.DirFS(dir), want: "is not an absolute path"},
		{name: "missing town", root: filepath.Join(dir, "absent"), fsys: os.DirFS(dir), want: "reading the town root"},
		{name: "town is a file", root: file, fsys: os.DirFS(dir), want: "is not a directory"},
		{
			name: "unreadable root",
			root: dir,
			fsys: failingFS{FS: os.DirFS(dir), readDirErr: map[string]error{".": boom}},
			want: "reading the town root",
		},
		{
			name: "walk fails",
			root: dir,
			fsys: failingFS{FS: walkTown, readDirErr: map[string]error{"rig": boom}},
			want: "walking rig",
		},
		{
			name: "class file cannot be read",
			root: dir,
			fsys: failingFS{FS: classTown, openErr: map[string]error{"settings/config.json": boom}},
			want: "reading settings/config.json",
		},
		{
			name: "worktree list fails",
			root: dir,
			fsys: failingFS{FS: repoTown, readDirErr: map[string]error{"rig/.repo.git/worktrees": boom}},
			want: "walking rig/.repo.git/worktrees",
		},
		{
			name: "worktree pointer is visited",
			root: dir,
			fsys: fstest.MapFS{"rig/.repo.git/worktrees/w/gitdir": {Data: []byte(rewriteOldRoot + "\x00")}},
			want: "rig/.repo.git/worktrees/w/gitdir holds a NUL byte",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := rewriteWith(tc.fsys, tc.root, rewriteQuery(), false, hostRewriteFiles())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rewriteWith = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// pointerFS is a town whose entries fail at named paths: a stat fails at the
// lstatErr paths and an open at the openErr paths. It implements fs.ReadLinkFS,
// so fs.Lstat calls its Lstat rather than opening the file.
type pointerFS struct {
	fs.FS
	lstatErr map[string]error
	openErr  map[string]error
}

func (f pointerFS) Lstat(name string) (fs.FileInfo, error) {
	if err, ok := f.lstatErr[name]; ok {
		return nil, &fs.PathError{Op: "lstat", Path: name, Err: err}
	}
	return fs.Lstat(f.FS, name)
}

func (f pointerFS) ReadLink(name string) (string, error) {
	return fs.ReadLink(f.FS, name)
}

func (f pointerFS) Open(name string) (fs.File, error) {
	if err, ok := f.openErr[name]; ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: err}
	}
	return f.FS.Open(name)
}

// TestRewriteRefusesAWorktreePointerThatCannotBeRead drives the bare repository's
// worktree entries through each read the passes make: the kept-copy search lists
// the worktree directory, and the rewrite walk stats the pointer and then reads it. The
// control runs the same town with no failure, so a pass that errors for another
// reason cannot pass the injected rows. Each injected row names the path that
// failed.
func TestRewriteRefusesAWorktreePointerThatCannotBeRead(t *testing.T) {
	t.Parallel()
	const (
		dir     = "rig/.repo.git/worktrees/w"
		pointer = dir + "/gitdir"
	)
	town := fstest.MapFS{pointer: {Data: []byte(rewriteOldRoot + "\n")}}
	cases := []struct {
		name string
		fsys pointerFS
		want string
	}{
		{name: "control with no failure", fsys: pointerFS{FS: town}},
		{name: "worktree directory cannot be listed", fsys: pointerFS{FS: town, openErr: map[string]error{dir: errInjected}}, want: dir},
		{name: "pointer cannot be stat'd", fsys: pointerFS{FS: town, lstatErr: map[string]error{pointer: errInjected}}, want: pointer},
		{name: "pointer cannot be read", fsys: pointerFS{FS: town, openErr: map[string]error{pointer: errInjected}}, want: pointer},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := rewriteWith(tc.fsys, t.TempDir(), rewriteQuery(), false, hostRewriteFiles())
			if tc.want == "" {
				if err != nil {
					t.Fatalf("rewriteWith with no failure = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, errInjected) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("rewriteWith = %v, want the injected failure naming %s", err, tc.want)
			}
		})
	}
}

// TestRewriteAndUndoRefuseBeforeTheWalk drives the query and town checks both
// entry points make before they read the tree.
func TestRewriteAndUndoRefuseBeforeTheWalk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	boom := errors.New("boom")
	invalid := rewriteQuery()
	invalid.ToRoot = invalid.FromRoot

	if _, err := rewriteWith(os.DirFS(dir), dir, invalid, false, hostRewriteFiles()); err == nil || !strings.Contains(err.Error(), "are the same") {
		t.Errorf("Rewrite with an invalid query = %v, want a refusal naming the roots", err)
	}
	if _, err := undoWith(os.DirFS(dir), "", hostRewriteFiles()); err == nil || !strings.Contains(err.Error(), "--town is required") {
		t.Errorf("Undo without a town = %v, want a refusal naming --town", err)
	}
	unreadable := failingFS{FS: os.DirFS(dir), readDirErr: map[string]error{".": boom}}
	if _, err := undoWith(unreadable, dir, hostRewriteFiles()); err == nil || !strings.Contains(err.Error(), "reading the town root") {
		t.Errorf("Undo of an unreadable town = %v, want a refusal naming the read", err)
	}
}

// TestScanRefusesWhatItCannotRead drives every refusal of the post-apply scan:
// the query it judges, the town it reads, its walk and a class file it cannot
// read as text.
func TestScanRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	boom := errors.New("boom")
	invalid := rewriteQuery()
	invalid.ToRoot = invalid.FromRoot
	walkTown := fstest.MapFS{"rig/settings/config.json": {Data: []byte(`{}`)}}
	binaryTown := fstest.MapFS{"settings/config.json": {Data: []byte(rewriteOldRoot + "\x00")}}

	cases := []struct {
		name string
		root string
		fsys fs.FS
		q    RewriteQuery
		want string
	}{
		{name: "invalid query", root: dir, fsys: os.DirFS(dir), q: invalid, want: "are the same"},
		{name: "no town", root: "", fsys: os.DirFS(dir), q: rewriteQuery(), want: "--town is required"},
		{name: "missing town", root: filepath.Join(dir, "absent"), fsys: os.DirFS(dir), q: rewriteQuery(), want: "reading the town root"},
		{
			name: "unreadable root",
			root: dir,
			fsys: failingFS{FS: os.DirFS(dir), readDirErr: map[string]error{".": boom}},
			q:    rewriteQuery(),
			want: "reading the town root",
		},
		{
			name: "walk fails",
			root: dir,
			fsys: failingFS{FS: walkTown, readDirErr: map[string]error{"rig": boom}},
			q:    rewriteQuery(),
			want: "walking rig",
		},
		{name: "binary class file", root: dir, fsys: binaryTown, q: rewriteQuery(), want: "holds a NUL byte"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := scanWith(tc.fsys, tc.root, tc.q, nil); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("scanWith = %v, want a refusal naming %q", err, tc.want)
			}
		})
	}
}

// TestRewriteRefusesABinaryClassFile drives the one file the pass cannot judge:
// a listed-class file holding a NUL byte is refused, and its bytes are left as
// they were rather than guessed at.
func TestRewriteRefusesABinaryClassFile(t *testing.T) {
	t.Parallel()
	binary := rewriteOldRoot + "\x00gone"
	root := writeRewriteTown(t, []rewriteTownFile{
		{"settings/config.json", binary},
		{"gastown/config.json", `{"dir":"` + rewriteOldRoot + `"}`},
	})
	_, err := Rewrite(root, rewriteQuery(), true)
	if err == nil || !strings.Contains(err.Error(), "settings/config.json holds a NUL byte") {
		t.Fatalf("Rewrite of a binary class file = %v, want a refusal naming settings/config.json", err)
	}
	if got := readRewriteFile(t, root, "settings/config.json"); got != binary {
		t.Errorf("the refused file changed to %q", got)
	}
	if got := readRewriteFile(t, root, "gastown/config.json"); !strings.Contains(got, rewriteOldRoot) {
		t.Errorf("the refused run wrote another file: %s", got)
	}
}

// rewriteTree reads every file under root, keyed by slash path.
func rewriteTree(t *testing.T, root string) map[string]string {
	t.Helper()
	files := map[string]string{}
	err := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, full)
		if err != nil {
			return err
		}
		files[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("reading the scratch town: %v", err)
	}
	return files
}

// rewriteBackups lists the kept copies under root, sorted.
func rewriteBackups(t *testing.T, root string) []string {
	t.Helper()
	var backups []string
	for rel := range rewriteTree(t, root) {
		if strings.HasSuffix(rel, RewriteBackupSuffix) {
			backups = append(backups, rel)
		}
	}
	slices.Sort(backups)
	return backups
}

// mapsEqual reports whether two string-keyed maps hold the same entries.
func mapsEqual[V comparable](a, b map[string]V) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// keysOf returns the keys of m, for an error message.
func keysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
