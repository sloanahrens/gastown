package forgejocut

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"
)

// memFiles is an in-memory Files: a map of path to content, directories implied
// by the paths and by dirs. It records every write and rename, so a test pins what
// the step wrote as well as what it left alone.
type memFiles struct {
	files   map[string][]byte
	modes   map[string]fs.FileMode
	dirs    map[string]bool
	writes  []string
	renames [][2]string
}

func newMemFiles() *memFiles {
	return &memFiles{files: map[string][]byte{}, modes: map[string]fs.FileMode{}, dirs: map[string]bool{}}
}

func (m *memFiles) addDir(p string) { m.dirs[p] = true }

func (m *memFiles) addFile(p, content string, mode fs.FileMode) {
	m.files[p] = []byte(content)
	m.modes[p] = mode
}

type memInfo struct {
	name string
	mode fs.FileMode
	size int64
}

func (i memInfo) Name() string       { return i.name }
func (i memInfo) Size() int64        { return i.size }
func (i memInfo) Mode() fs.FileMode  { return i.mode }
func (i memInfo) ModTime() time.Time { return time.Time{} }
func (i memInfo) IsDir() bool        { return i.mode.IsDir() }
func (i memInfo) Sys() any           { return nil }

func (m *memFiles) Stat(p string) (fs.FileInfo, error) {
	if c, ok := m.files[p]; ok {
		return memInfo{name: path.Base(p), mode: m.modes[p], size: int64(len(c))}, nil
	}
	if m.dirs[p] {
		return memInfo{name: path.Base(p), mode: fs.ModeDir | 0o755}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: p, Err: fs.ErrNotExist}
}

func (m *memFiles) ReadDir(p string) ([]fs.DirEntry, error) {
	seen := map[string]bool{}
	for q := range m.files {
		if path.Dir(q) == p {
			seen[q] = true
		}
	}
	for q := range m.dirs {
		if path.Dir(q) == p {
			seen[q] = true
		}
	}
	var names []string
	for q := range seen {
		names = append(names, q)
	}
	sort.Strings(names)
	var out []fs.DirEntry
	for _, q := range names {
		info, _ := m.Stat(q)
		out = append(out, fs.FileInfoToDirEntry(info))
	}
	return out, nil
}

func (m *memFiles) ReadFile(p string) ([]byte, error) {
	c, ok := m.files[p]
	if !ok {
		return nil, &fs.PathError{Op: "read", Path: p, Err: fs.ErrNotExist}
	}
	return slices.Clone(c), nil
}

func (m *memFiles) WriteFile(p string, data []byte, perm fs.FileMode) error {
	m.writes = append(m.writes, p)
	m.files[p] = slices.Clone(data)
	m.modes[p] = perm
	return nil
}

func (m *memFiles) Rename(from, to string) error {
	m.renames = append(m.renames, [2]string{from, to})
	m.files[to], m.modes[to] = m.files[from], m.modes[from]
	delete(m.files, from)
	delete(m.modes, from)
	return nil
}

func (m *memFiles) Chmod(p string, perm fs.FileMode) error {
	m.writes = append(m.writes, p)
	if _, ok := m.files[p]; ok {
		m.modes[p] = perm
	}
	return nil
}

func (m *memFiles) MkdirAll(p string, perm fs.FileMode) error {
	m.writes = append(m.writes, p)
	m.dirs[p] = true
	return nil
}

// repointGit is the git seam: a set of repositories with an origin each, answering
// the calls the step makes and recording every argv and every set-url. A directory
// in origins is a work tree of its own; one in bare is a bare repository; one in
// inside is a plain folder under the repository named as its value, which is what
// git answers for it when asked for the top level.
type repointGit struct {
	origins map[string]string // dir -> origin url; a dir absent here is not a work tree
	bare    map[string]bool
	inside  map[string]string // dir -> the top level of the repository it sits in
	inBare  map[string]bool   // dir -> a folder inside a bare repository: bare, but its git dir is above it
	calls   [][]string
	setURLs []string // "dir url", in call order
	setErr  error
	getErr  error // a get-url failure that is not a missing origin
}

func (g *repointGit) run(_ context.Context, argv []string) ([]byte, error) {
	g.calls = append(g.calls, slices.Clone(argv))
	if len(argv) < 5 || argv[0] != "git" || argv[1] != "-C" {
		return nil, errors.New("unexpected git call: " + strings.Join(argv, " "))
	}
	dir, rest := argv[2], argv[3:]
	origin, isTree := g.origins[dir]
	switch strings.Join(rest, " ") {
	case "rev-parse --show-toplevel":
		if top, ok := g.inside[dir]; ok {
			return []byte(top + "\n"), nil
		}
		if !isTree || g.bare[dir] {
			return nil, errors.New("fatal: this operation must be run in a work tree")
		}
		return []byte(dir + "\n"), nil
	case "rev-parse --is-bare-repository --git-dir":
		if g.bare[dir] {
			return []byte("true\n.\n"), nil
		}
		if g.inBare[dir] {
			return []byte("true\n../..\n"), nil
		}
		if top, ok := g.inside[dir]; ok && top != "" {
			return []byte("false\n" + top + "/.git\n"), nil
		}
		return nil, errors.New("fatal: not a git repository")
	}
	switch strings.Join(rest[:2], " ") {
	case "remote get-url":
		if g.getErr != nil {
			return nil, g.getErr
		}
		if (!isTree && !g.bare[dir]) || origin == "" {
			return nil, exitStatus(noSuchRemoteExit)
		}
		return []byte(origin + "\n"), nil
	case "remote set-url":
		g.setURLs = append(g.setURLs, dir+" "+rest[3])
		if g.setErr != nil {
			return nil, g.setErr
		}
		g.origins[dir] = rest[3]
		return nil, nil
	}
	return nil, errors.New("unexpected git call: " + strings.Join(argv, " "))
}

const (
	townRoot   = "/town"
	forgejoURL = "http://forge.example/acme/rig.git"
	oldURL     = "https://github.example/acme/rig.git"
)

// repointTown is a rig with a bare repository, a mayor clone and crew clones, a
// hidden crew entry and a file in crew/, each clone at the old origin.
func repointTown() (*memFiles, *repointGit) {
	fsys := newMemFiles()
	git := &repointGit{origins: map[string]string{}, bare: map[string]bool{"/town/rig/.repo.git": true}, inside: map[string]string{}}
	for _, d := range []string{"/town/rig/.repo.git", "/town/rig/mayor/rig", "/town/rig/crew/ann", "/town/rig/crew/bob"} {
		fsys.addDir(d)
		git.origins[d] = oldURL
	}

	fsys.addDir("/town/rig/crew/.cache")
	git.origins["/town/rig/crew/.cache"] = oldURL
	fsys.addDir("/town/rig/crew")
	fsys.addFile("/town/rig/crew/NOTES.md", "notes", 0o644)
	return fsys, git
}

func repointCutover(fsys *memFiles, git *repointGit, dry bool) (*Cutover, *[]string) {
	var log []string
	opts := &Options{
		Rig: "rig", TownRoot: townRoot, RigRoot: "/town/rig", BareRepo: "/town/rig/.repo.git",
		ForgejoURL: forgejoURL, GitHubURL: oldURL, AdminGitURL: "http://127.0.0.1:3000/acme/rig.git",
		DryRun: dry,
	}
	return &Cutover{
		Opts: opts, Git: git.run, Files: fsys, Log: func(l string) { log = append(log, l) },
		Now: func() time.Time { return time.Date(2026, 11, 1, 14, 5, 9, 0, time.UTC) },
	}, &log
}

// The step sets the bare repository, the mayor clone and every crew clone, in that
// order, and nothing else: not a hidden crew entry, not a file. Skipping the crew
// clones turns this red.
func TestRepointVisitsBareMayorAndEveryCrew(t *testing.T) {
	t.Parallel()
	fsys, git := repointTown()
	c, _ := repointCutover(fsys, git, false)

	if err := c.RepointRemotes(); err != nil {
		t.Fatalf("RepointRemotes: %v", err)
	}

	want := []string{
		"/town/rig/.repo.git " + forgejoURL,
		"/town/rig/mayor/rig " + forgejoURL,
		"/town/rig/crew/ann " + forgejoURL,
		"/town/rig/crew/bob " + forgejoURL,
	}
	if !slices.Equal(git.setURLs, want) {
		t.Errorf("set-url calls = %q, want %q", git.setURLs, want)
	}
}

// A directory already at the URL is reported and not touched, a directory that is
// not a repository is skipped, and a second run changes nothing.
func TestRepointSkipsAlreadyCorrect(t *testing.T) {
	t.Parallel()
	fsys, git := repointTown()
	git.origins["/town/rig/mayor/rig"] = forgejoURL
	delete(git.origins, "/town/rig/crew/bob")
	c, log := repointCutover(fsys, git, false)

	if err := c.RepointRemotes(); err != nil {
		t.Fatalf("RepointRemotes: %v", err)
	}
	if want := []string{"/town/rig/.repo.git " + forgejoURL, "/town/rig/crew/ann " + forgejoURL}; !slices.Equal(git.setURLs, want) {
		t.Errorf("set-url calls = %q, want %q", git.setURLs, want)
	}
	joined := strings.Join(*log, "\n")
	for _, line := range []string{"/town/rig/mayor/rig: origin is already " + forgejoURL, "skip /town/rig/crew/bob (not a git repository of its own)"} {
		if !strings.Contains(joined, line) {
			t.Errorf("log lacks %q:\n%s", line, joined)
		}
	}

	git.setURLs = nil
	if err := c.RepointRemotes(); err != nil {
		t.Fatalf("second RepointRemotes: %v", err)
	}
	if len(git.setURLs) != 0 {
		t.Errorf("a second run set %q, want nothing", git.setURLs)
	}
}

// A rig with no mayor clone and no crew directory repoints its bare repository
// alone; a clone with no origin is set, not skipped.
func TestRepointNeedsOnlyTheBareRepository(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addDir("/town/rig/.repo.git")
	git := &repointGit{origins: map[string]string{"/town/rig/.repo.git": ""}, bare: map[string]bool{"/town/rig/.repo.git": true}}
	c, _ := repointCutover(fsys, git, false)

	if err := c.RepointRemotes(); err != nil {
		t.Fatalf("RepointRemotes: %v", err)
	}
	if want := []string{"/town/rig/.repo.git " + forgejoURL}; !slices.Equal(git.setURLs, want) {
		t.Errorf("set-url calls = %q, want %q", git.setURLs, want)
	}
}

// A dry run prints each command, with the old origin for the undo, and runs none;
// town.json is read and neither edited nor backed up.
func TestRepointDryRunChangesNothing(t *testing.T) {
	t.Parallel()
	fsys, git := repointTown()
	fsys.addFile("/town/mayor/town.json", townJSON(oldURL), 0o600)
	c, log := repointCutover(fsys, git, true)

	if err := c.RepointRemotes(); err != nil {
		t.Fatalf("RepointRemotes: %v", err)
	}
	if err := c.SetTownGitURL(); err != nil {
		t.Fatalf("SetTownGitURL: %v", err)
	}

	if len(git.setURLs) != 0 {
		t.Errorf("a dry run set %q", git.setURLs)
	}
	if len(fsys.writes) != 0 || len(fsys.renames) != 0 {
		t.Errorf("a dry run wrote %q and renamed %q", fsys.writes, fsys.renames)
	}
	joined := strings.Join(*log, "\n")
	for _, want := range []string{
		"+ git -C /town/rig/crew/ann remote set-url origin " + forgejoURL + " (origin was " + oldURL + ")",
		"would back up /town/mayor/town.json to /town/mayor/town.json.bak-20261101-140509",
		"would set registry.rigs.rig.git_url to " + forgejoURL,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("log lacks %q:\n%s", want, joined)
		}
	}
}

// A failed set-url stops the step, names the directory, and keeps a credential in
// the URL out of the message, the log and every argv the git seam received.
func TestRepointFailureNamesTheDirectoryAndHidesTheCredential(t *testing.T) {
	t.Parallel()
	const secret = "s3cret-token"
	fsys, git := repointTown()
	git.setErr = errors.New("fatal: unable to access 'http://bot:" + secret + "@forge.example/acme/rig.git'")
	c, log := repointCutover(fsys, git, false)
	c.Opts.GitHubURL = "http://bot:" + secret + "@forge.example/acme/rig.git"

	err := c.RepointRemotes()

	if err == nil {
		t.Fatal("RepointRemotes passed a failing set-url")
	}
	if !strings.Contains(err.Error(), "/town/rig/.repo.git") {
		t.Errorf("error %q does not name the directory", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("error %q carries the credential", err)
	}
	for _, line := range *log {
		if strings.Contains(line, secret) {
			t.Errorf("log line %q carries the credential", line)
		}
	}
	for _, call := range git.calls {
		if strings.Contains(strings.Join(call, " "), secret) {
			t.Errorf("git argv %q carries the credential", call)
		}
	}
	if len(git.setURLs) != 1 {
		t.Errorf("the step went on after the failure: %q", git.setURLs)
	}
}

// townJSON is a town file with keys in a deliberate non-sorted order, a second
// rig, a number, and text a re-encoder would change (an HTML character, a
// non-ASCII letter), in the 2-space layout the script wrote.
func townJSON(gitURL string) string {
	return `{
  "type": "town",
  "version": 2,
  "name": "caf\u00e9 <town>",
  "registry": {
    "rigs": {
      "zeta": {
        "git_url": "https://other.example/z.git",
        "weight": 1.50
      },
      "rig": {
        "prefix": "gt",
        "git_url": "` + gitURL + `",
        "tags": [
          "a",
          "b"
        ]
      }
    }
  },
  "empty": {},
  "list": []
}
`
}

// Only the one value changes: key order, other rigs, numbers and escapes survive, the old file is kept byte for byte in a dated backup, and the
// replacement is written beside the file and renamed over it.
func TestTownGitURLEditsOnlyThatField(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	before := townJSON(oldURL)
	fsys.addFile("/town/mayor/town.json", before, 0o600)
	c, _ := repointCutover(fsys, &repointGit{}, false)

	if err := c.SetTownGitURL(); err != nil {
		t.Fatalf("SetTownGitURL: %v", err)
	}

	if got, want := string(fsys.files["/town/mayor/town.json"]), townJSON(forgejoURL); got != want {
		t.Errorf("town.json =\n%s\nwant\n%s", got, want)
	}
	if got := string(fsys.files["/town/mayor/town.json.bak-20261101-140509"]); got != before {
		t.Errorf("backup =\n%s\nwant the original", got)
	}
	if got := fsys.modes["/town/mayor/town.json"]; got != 0o600 {
		t.Errorf("town.json mode = %v, want 0600 kept", got)
	}
	if want := [][2]string{{"/town/mayor/town.json.new-20261101-140509", "/town/mayor/town.json"}}; !slices.Equal(fsys.renames, want) {
		t.Errorf("renames = %v, want %v", fsys.renames, want)
	}
}

// A value that already matches is neither rewritten nor backed up. Always
// rewriting turns this red.
func TestTownGitURLAlreadyMatchingWritesNothing(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addFile("/town/mayor/town.json", townJSON(forgejoURL), 0o600)
	c, log := repointCutover(fsys, &repointGit{}, false)

	if err := c.SetTownGitURL(); err != nil {
		t.Fatalf("SetTownGitURL: %v", err)
	}

	if len(fsys.writes) != 0 {
		t.Errorf("wrote %q for a matching value", fsys.writes)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "git_url is already "+forgejoURL) {
		t.Errorf("log does not say the value matched: %q", *log)
	}
}

// A rig the registry does not list, and a file that is not JSON, are refused
// before anything is written.
func TestTownGitURLRefusesWhatItCannotEdit(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ content, want string }{
		"unknown rig":   {`{"registry": {"rigs": {"other": {}}}}`, "has no registry.rigs.rig"},
		"no registry":   {`{"type": "town"}`, "has no registry.rigs.rig"},
		"not an object": {`["a"]`, "not an object"},
		"not json":      {`{"registry": `, "is not usable"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fsys := newMemFiles()
			fsys.addFile("/town/mayor/town.json", tc.content, 0o600)
			c, _ := repointCutover(fsys, &repointGit{}, false)

			err := c.SetTownGitURL()

			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("SetTownGitURL error = %v, want one containing %q", err, tc.want)
			}
			if len(fsys.writes) != 0 {
				t.Errorf("wrote %q for a file it refused", fsys.writes)
			}
		})
	}
}

// A rig entry with no git_url gets one, after the keys it has.
func TestTownGitURLAddsAMissingKey(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addFile("/town/mayor/town.json", `{"registry":{"rigs":{"rig":{"prefix":"gt"}}}}`, 0o644)
	c, _ := repointCutover(fsys, &repointGit{}, false)

	if err := c.SetTownGitURL(); err != nil {
		t.Fatalf("SetTownGitURL: %v", err)
	}

	want := "{\n  \"registry\": {\n    \"rigs\": {\n      \"rig\": {\n        \"prefix\": \"gt\",\n        \"git_url\": \"" + forgejoURL + "\"\n      }\n    }\n  }\n}\n"
	if got := string(fsys.files["/town/mayor/town.json"]); got != want {
		t.Errorf("town.json =\n%s\nwant\n%s", got, want)
	}
}

// The backup is named for the clock's second, holds the same bytes and keeps the
// permission bits.
func TestBackupFileNameAndContent(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	fsys.addFile("/town/settings/config.json", "{\"a\": 1}\n", 0o640)
	c, log := repointCutover(fsys, &repointGit{}, false)

	if err := c.BackupFile("/town/settings/config.json"); err != nil {
		t.Fatalf("BackupFile: %v", err)
	}

	backup := "/town/settings/config.json.bak-20261101-140509"
	if got := string(fsys.files[backup]); got != "{\"a\": 1}\n" {
		t.Errorf("backup content = %q", got)
	}
	if fsys.modes[backup] != 0o640 {
		t.Errorf("backup mode = %v, want 0640", fsys.modes[backup])
	}
	if !strings.Contains(strings.Join(*log, "\n"), "backed up /town/settings/config.json to "+backup) {
		t.Errorf("log = %q", *log)
	}
}

// A file that is not there has nothing to back up: the step says so and writes
// nothing.
func TestBackupFileOfAnAbsentFileIsLogged(t *testing.T) {
	t.Parallel()
	fsys := newMemFiles()
	c, log := repointCutover(fsys, &repointGit{}, false)

	if err := c.BackupFile("/town/settings/config.json"); err != nil {
		t.Fatalf("BackupFile: %v", err)
	}
	if len(fsys.writes) != 0 {
		t.Errorf("wrote %q", fsys.writes)
	}
	if !strings.Contains(strings.Join(*log, "\n"), "no /town/settings/config.json to back up") {
		t.Errorf("log = %q", *log)
	}
}

// A path that names no key is refused rather than read as the whole document.
func TestJSONPathRefusesAnEmptyPath(t *testing.T) {
	t.Parallel()
	if _, _, err := jsonGet([]byte(`{"a":1}`), nil); !errors.Is(err, errEmptyPath) {
		t.Errorf("jsonGet error = %v, want errEmptyPath", err)
	}
	if _, err := jsonSet([]byte(`{"a":1}`), nil, jsonString("x")); !errors.Is(err, errEmptyPath) {
		t.Errorf("jsonSet error = %v, want errEmptyPath", err)
	}
}

// failFiles is memFiles with one operation made to fail: the key is "<op> <path>"
// (op is stat, readdir, read, write, rename, chmod or mkdir), so a test drives
// each refusal.
type failFiles struct {
	*memFiles
	fail map[string]error
}

func (f failFiles) Stat(p string) (fs.FileInfo, error) {
	if err := f.fail["stat "+p]; err != nil {
		return nil, err
	}
	return f.memFiles.Stat(p)
}

func (f failFiles) ReadDir(p string) ([]fs.DirEntry, error) {
	if err := f.fail["readdir "+p]; err != nil {
		return nil, err
	}
	return f.memFiles.ReadDir(p)
}

func (f failFiles) ReadFile(p string) ([]byte, error) {
	if err := f.fail["read "+p]; err != nil {
		return nil, err
	}
	return f.memFiles.ReadFile(p)
}

func (f failFiles) WriteFile(p string, d []byte, m fs.FileMode) error {
	if err := f.fail["write "+p]; err != nil {
		return err
	}
	return f.memFiles.WriteFile(p, d, m)
}

func (f failFiles) Rename(a, b string) error {
	if err := f.fail["rename "+b]; err != nil {
		return err
	}
	return f.memFiles.Rename(a, b)
}

func (f failFiles) Chmod(p string, m fs.FileMode) error {
	if err := f.fail["chmod "+p]; err != nil {
		return err
	}
	return f.memFiles.Chmod(p, m)
}

func (f failFiles) MkdirAll(p string, m fs.FileMode) error {
	if err := f.fail["mkdir "+p]; err != nil {
		return err
	}
	return f.memFiles.MkdirAll(p, m)
}

var errDisk = errors.New("disk said no")

// Each filesystem failure ends its step with an error that names the file and the
// cause, and the step writes no half result: a failed write of the replacement
// leaves town.json whole.
func TestFilesystemFailuresAreRefusedByName(t *testing.T) {
	t.Parallel()
	const town = "/town/mayor/town.json"
	for name, tc := range map[string]struct {
		fail map[string]error
		run  func(c *Cutover) error
		want string
	}{
		"crew listing":    {map[string]error{"readdir /town/rig/crew": errDisk}, (*Cutover).RepointRemotes, "could not list the crew clones in /town/rig/crew"},
		"read town.json":  {map[string]error{"read " + town: errDisk}, (*Cutover).SetTownGitURL, "could not read " + town},
		"stat for backup": {map[string]error{"stat " + town: errDisk}, (*Cutover).SetTownGitURL, "could not look at " + town},
		"write backup":    {map[string]error{"write " + town + ".bak-20261101-140509": errDisk}, (*Cutover).SetTownGitURL, "could not write the backup"},
		"write temp":      {map[string]error{"write " + town + ".new-20261101-140509": errDisk}, (*Cutover).SetTownGitURL, "could not write " + town + ".new-20261101-140509"},
		"rename":          {map[string]error{"rename " + town: errDisk}, (*Cutover).SetTownGitURL, "could not replace " + town + " (the new text is left in " + town + ".new-20261101-140509)"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mem, git := repointTown()
			mem.addFile(town, townJSON(oldURL), 0o600)
			c, _ := repointCutover(mem, git, false)
			c.Files = failFiles{memFiles: mem, fail: tc.fail}

			err := tc.run(c)

			if err == nil || !strings.Contains(err.Error(), tc.want) || !errors.Is(err, errDisk) {
				t.Errorf("error = %v, want one containing %q wrapping the disk error", err, tc.want)
			}
			if got := string(mem.files[town]); got != townJSON(oldURL) {
				t.Errorf("town.json changed to\n%s", got)
			}
		})
	}
}

// BackupFile reads the file it copies; a read that fails is named.
func TestBackupFileReadFailureIsRefused(t *testing.T) {
	t.Parallel()
	mem := newMemFiles()
	mem.addFile("/town/x.json", "{}", 0o644)
	c, _ := repointCutover(mem, &repointGit{}, false)
	c.Files = failFiles{memFiles: mem, fail: map[string]error{"read /town/x.json": errDisk}}

	err := c.BackupFile("/town/x.json")

	if err == nil || !strings.Contains(err.Error(), "could not read /town/x.json to back it up") {
		t.Errorf("BackupFile error = %v", err)
	}
}

// A JSON document the reader cannot walk is refused with a cause, not a guess: no
// text, an unclosed object, a bad value, text after the document, a non-object,
// and a path through a value that is not an object.
func TestJSONPathRefusesWhatItCannotWalk(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"empty":         ``,
		"unclosed":      `{`,
		"key only":      `{"a"`,
		"bad value":     `{"a": tru}`,
		"no end":        `{"a": 1`,
		"no end string": `{"a": "x"`,
		"wrong closer":  `{"a": 1]`,
		"trailing text": `{"a": 1} x`,
		"array":         `[1]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if _, _, err := jsonGet([]byte(doc), []string{"a"}); err == nil {
				t.Errorf("jsonGet read %q", doc)
			}
			if _, err := jsonSet([]byte(doc), []string{"a"}, jsonString("x")); err == nil {
				t.Errorf("jsonSet edited %q", doc)
			}
		})
	}
	if _, err := jsonSet([]byte(`{"a": 1}`), []string{"a", "b"}, jsonString("x")); err == nil {
		t.Error("jsonSet went through a value that is not an object")
	}
	if _, err := jsonSet([]byte(`{"a": 1}`), []string{"b", "c"}, jsonString("x")); err == nil || !strings.Contains(err.Error(), `no key "b"`) {
		t.Errorf("jsonSet through a missing key: error = %v", err)
	}
	if _, err := jsonSet([]byte(`{"a": 1}`), []string{"a"}, json.RawMessage(`{`)); err == nil {
		t.Error("jsonSet wrote a value that is not JSON")
	}
}

// The bare repository and the mayor's clone must be repositories: a step that
// skipped them would report success with origin still at GitHub. A crew entry that
// is not a repository is still skipped (TestRepointSkipsAlreadyCorrect).
func TestRepointFailsWhenTheBareOrMayorIsNotARepository(t *testing.T) {
	t.Parallel()
	for name, dir := range map[string]string{"bare": "/town/rig/.repo.git", "mayor": "/town/rig/mayor/rig"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fsys, git := repointTown()
			delete(git.origins, dir)
			delete(git.bare, dir)
			c, _ := repointCutover(fsys, git, false)

			err := c.RepointRemotes()

			if err == nil || !strings.Contains(err.Error(), dir+" is not a git repository of its own") {
				t.Errorf("RepointRemotes error = %v, want one naming %s", err, dir)
			}
		})
	}
}

// A get-url that fails for any reason but a missing origin stops the step: the
// undo line would otherwise name a blank old URL.
func TestRepointFailsWhenTheOriginCannotBeRead(t *testing.T) {
	t.Parallel()
	fsys, git := repointTown()
	git.getErr = errors.New("fatal: detected dubious ownership")
	c, _ := repointCutover(fsys, git, false)

	err := c.RepointRemotes()

	if err == nil || !strings.Contains(err.Error(), "could not read the origin of /town/rig/.repo.git") {
		t.Errorf("RepointRemotes error = %v", err)
	}
	if len(git.setURLs) != 0 {
		t.Errorf("set %q after a failed read", git.setURLs)
	}
}

// A backup name that is already taken is refused rather than replaced, and the
// file is left alone.
func TestBackupFileRefusesToReplaceABackup(t *testing.T) {
	t.Parallel()
	mem := newMemFiles()
	mem.addFile("/town/x.json", "new", 0o644)
	mem.addFile("/town/x.json.bak-20261101-140509", "older backup", 0o644)
	c, _ := repointCutover(mem, &repointGit{}, false)

	err := c.BackupFile("/town/x.json")

	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("BackupFile error = %v", err)
	}
	if got := string(mem.files["/town/x.json.bak-20261101-140509"]); got != "older backup" {
		t.Errorf("the earlier backup became %q", got)
	}
}

// A town file in another layout is re-indented at two spaces with its keys in the
// same order and its value text unchanged: the layout is not kept, the content is.
func TestTownGitURLReindentsACompactFile(t *testing.T) {
	t.Parallel()
	mem := newMemFiles()
	mem.addFile("/town/mayor/town.json", "{\"name\":\"caf\\u00e9\",\"registry\":{\"rigs\":{\"rig\":{\"git_url\":\""+oldURL+"\",\"prefix\":\"gt\"}}}}", 0o600)
	c, _ := repointCutover(mem, &repointGit{}, false)

	if err := c.SetTownGitURL(); err != nil {
		t.Fatalf("SetTownGitURL: %v", err)
	}

	want := "{\n  \"name\": \"caf\\u00e9\",\n  \"registry\": {\n    \"rigs\": {\n      \"rig\": {\n        \"git_url\": \"" + forgejoURL + "\",\n        \"prefix\": \"gt\"\n      }\n    }\n  }\n}\n"
	if got := string(mem.files["/town/mayor/town.json"]); got != want {
		t.Errorf("town.json =\n%s\nwant\n%s", got, want)
	}
}

// A folder that sits inside another repository answers rev-parse for that
// repository, so the step must not read it as a clone: the crew folder is skipped,
// the mayor folder fails the step, and the enclosing repository's origin is never
// read or set. Checking only that git finds some repository turns this red.
func TestRepointDoesNotReachIntoAnEnclosingRepository(t *testing.T) {
	t.Parallel()
	const town = "/town"
	for name, dir := range map[string]string{"crew": "/town/rig/crew/bob", "mayor": "/town/rig/mayor/rig"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			fsys, git := repointTown()
			delete(git.origins, dir)
			git.inside[dir] = town
			git.origins[town] = oldURL
			c, _ := repointCutover(fsys, git, false)

			err := c.RepointRemotes()

			if name == "mayor" && (err == nil || !strings.Contains(err.Error(), dir+" is not a git repository of its own")) {
				t.Errorf("RepointRemotes error = %v, want the mayor refused", err)
			}
			if name == "crew" && err != nil {
				t.Errorf("RepointRemotes: %v", err)
			}
			for _, call := range git.calls {
				if call[2] == dir && slices.Contains(call, "set-url") {
					t.Errorf("set-url ran on %s: %q", dir, call)
				}
			}
			if git.origins[town] != oldURL {
				t.Errorf("the enclosing repository's origin became %q", git.origins[town])
			}
		})
	}
}

// A temp name left by an earlier run in the same second is refused, not replaced.
func TestTownGitURLRefusesToReplaceATempFile(t *testing.T) {
	t.Parallel()
	mem := newMemFiles()
	mem.addFile("/town/mayor/town.json", townJSON(oldURL), 0o600)
	mem.addFile("/town/mayor/town.json.new-20261101-140509", "left behind", 0o600)
	c, _ := repointCutover(mem, &repointGit{}, false)

	err := c.SetTownGitURL()

	if err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("SetTownGitURL error = %v", err)
	}
	if got := string(mem.files["/town/mayor/town.json.new-20261101-140509"]); got != "left behind" {
		t.Errorf("the leftover file became %q", got)
	}
	if got := string(mem.files["/town/mayor/town.json"]); got != townJSON(oldURL) {
		t.Errorf("town.json changed to\n%s", got)
	}
}

// A folder inside a bare repository answers that it is bare, with a git directory
// above it: it is not the bare repository the step was given, and the step fails.
func TestRepointRefusesAFolderInsideABareRepository(t *testing.T) {
	t.Parallel()
	fsys, git := repointTown()
	const dir = "/town/rig/.repo.git"
	delete(git.bare, dir)
	delete(git.origins, dir)
	git.inBare = map[string]bool{dir: true}
	c, _ := repointCutover(fsys, git, false)

	err := c.RepointRemotes()

	if err == nil || !strings.Contains(err.Error(), dir+" is not a git repository of its own") {
		t.Errorf("RepointRemotes error = %v", err)
	}
	if len(git.setURLs) != 0 {
		t.Errorf("set %q", git.setURLs)
	}
}

// A crew folder reached through a symlink is the real clone: git reports the
// resolved top level, which differs from the path as given, and the folder must
// still be repointed, not skipped as "not a repository of its own".
func TestRepointFollowsASymlinkedCrewFolder(t *testing.T) {
	t.Parallel()
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "bob")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	fsys, git := repointTown()
	// The step visits the link; git answers for it with the resolved top level.
	git.origins[link] = oldURL
	git.inside[link] = real
	if !samePath(real, link) {
		t.Fatalf("samePath(%q, %q) = false, want true", real, link)
	}
	if samePath(real, t.TempDir()) {
		t.Error("samePath of two different directories = true")
	}
	c, _ := repointCutover(fsys, git, false)

	if !c.isRepositoryRoot(link) {
		t.Errorf("isRepositoryRoot(%q) = false for a symlink to its own top level %q", link, real)
	}
}
