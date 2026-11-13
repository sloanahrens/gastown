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
// every file outside the classes alone, and a second apply changes nothing.
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

	// A second apply finds nothing, writes nothing, and keeps no new copy.
	before := rewriteTree(t, root)
	second, err := Rewrite(root, rewriteQuery(), true)
	if err != nil {
		t.Fatalf("second Rewrite: %v", err)
	}
	if len(second.Files) != 0 {
		t.Errorf("the second apply changed %v, want nothing", second.Files)
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

	// A kept copy beside a file a new run would change refuses the run, and the
	// other file the run would change is not written either.
	stray := filepath.Join(root, "settings", "config.json"+RewriteBackupSuffix)
	if err := os.WriteFile(stray, []byte("kept by hand\n"), 0o644); err != nil {
		t.Fatalf("writing the kept copy: %v", err)
	}
	_, err = Rewrite(root, rewriteQuery(), true)
	if err == nil || !strings.Contains(err.Error(), "settings/config.json already has a kept copy") {
		t.Fatalf("Rewrite with a kept copy = %v, want a refusal naming settings/config.json", err)
	}
	if got := readRewriteFile(t, root, "gastown/config.json"); got != `{"dir":"`+rewriteOldRoot+`"}` {
		t.Errorf("the refused run wrote gastown/config.json: %s", got)
	}
	if got, err := os.ReadFile(stray); err != nil || string(got) != "kept by hand\n" {
		t.Errorf("the refused run overwrote the kept copy: %q, %v", got, err)
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

// TestRewriteRefusesAWriteThatFails drives every write step of an apply: a
// failure at any of them is returned, and the class file keeps its old bytes.
func TestRewriteRefusesAWriteThatFails(t *testing.T) {
	t.Parallel()
	boom := errors.New("boom")
	cases := []struct {
		name string
		edit func(*rewriteFiles)
		want string
	}{
		{
			name: "kept copy check",
			edit: func(f *rewriteFiles) { f.lstat = func(string) (fs.FileInfo, error) { return nil, boom } },
			want: "checking for a kept copy of settings/config.json",
		},
		{
			name: "kept copy open",
			edit: func(f *rewriteFiles) {
				f.openFile = func(string, int, fs.FileMode) (io.WriteCloser, error) { return nil, boom }
			},
			want: "keeping a copy of settings/config.json as config.json.pre-rewrite",
		},
		{
			name: "kept copy write",
			edit: func(f *rewriteFiles) {
				f.openFile = func(string, int, fs.FileMode) (io.WriteCloser, error) {
					return writeError{closeErr: nil}, nil
				}
			},
			want: "keeping a copy of config.json.pre-rewrite",
		},
		{
			name: "kept copy write and close",
			edit: func(f *rewriteFiles) {
				f.openFile = func(string, int, fs.FileMode) (io.WriteCloser, error) {
					return writeError{closeErr: boom}, nil
				}
			},
			want: "keeping a copy of config.json.pre-rewrite",
		},
		{
			name: "kept copy close",
			edit: func(f *rewriteFiles) {
				f.openFile = func(string, int, fs.FileMode) (io.WriteCloser, error) {
					return closeOnlyError{err: boom}, nil
				}
			},
			want: "keeping a copy of config.json.pre-rewrite",
		},
		{
			name: "new bytes write",
			edit: func(f *rewriteFiles) {
				f.writeFile = func(string, []byte, fs.FileMode) error { return boom }
			},
			want: "writing",
		},
		{
			name: "rename over the file",
			edit: func(f *rewriteFiles) {
				f.rename = func(string, string) error { return boom }
			},
			want: "replacing",
		},
		{
			name: "removing the temp file",
			edit: func(f *rewriteFiles) {
				f.rename = func(string, string) error { return boom }
				f.remove = func(string) error { return boom }
			},
			want: "removing config.json.rewrite-tmp also failed",
		},
		{
			name: "reading the file's mode",
			edit: func(f *rewriteFiles) {
				calls := 0
				host := os.Lstat
				f.lstat = func(p string) (fs.FileInfo, error) {
					calls++
					if calls > 1 {
						return nil, boom
					}
					return host(p)
				}
			},
			want: "reading settings/config.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root := writeRewriteTown(t, []rewriteTownFile{
				{"settings/config.json", `{"town_root":"` + rewriteOldRoot + `"}`},
			})
			files := hostRewriteFiles()
			tc.edit(&files)
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
// repository's worktree pointers when they cannot be listed or read.
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
			want: "reading rig/.repo.git/worktrees",
		},
		{
			name: "worktree pointer cannot be read",
			root: dir,
			fsys: failingFS{FS: repoTown, statErr: map[string]error{"rig/.repo.git/worktrees/w/gitdir": boom}},
			want: "reading rig/.repo.git/worktrees/w/gitdir",
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
