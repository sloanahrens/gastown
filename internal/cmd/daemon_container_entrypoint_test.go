package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

const (
	entrypointTown    = "/gt"
	entrypointBin     = "/usr/bin/gt"
	entrypointMarker  = "/gt/.gt-scratch-town"
	entrypointTownCfg = "/gt/mayor/town.json"
	// entrypointGtMount is a mountinfo table in which /gt is a bind mount.
	entrypointGtMount = "36 35 98:0 /srv/scratch /gt rw,noatime - ext4 /dev/root rw\n"
)

// entrypointRig is the host the entrypoint sees in a test: its environment,
// the mountinfo text, which paths exist, what the town root holds, and every
// command handed to run or exec. No process starts.
type entrypointRig struct {
	env       map[string]string
	mountinfo string
	mountErr  error
	exists    map[string]bool
	// kind is what Lstat finds at mayor/town.json; absent when not listed.
	kind map[string]entryKind
	// brokenPath makes its lstat and stat fail, to test the fail-closed read.
	brokenPath string
	dir        []string
	dirErr     error
	removed    []string
	removeErr  error
	scratch    bool
	calls      [][]string
}

// scratchRig is a container whose town root is the scratch town: every
// condition for an install holds, and --scratch-town asks for one.
func scratchRig() *entrypointRig {
	return &entrypointRig{
		env: map[string]string{
			config.ContainerModeEnv: config.SupervisorContainer,
			containerTownRootEnv:    entrypointTown,
			containerGitUserEnv:     "daemon",
			containerGitEmailEnv:    "daemon@example.invalid",
		},
		mountinfo: entrypointGtMount,
		exists:    map[string]bool{entrypointMarker: true},
		kind:      map[string]entryKind{},
		dir:       []string{config.ScratchTownMarker},
		scratch:   true,
	}
}

// townRig is a container whose town root already holds mayor/town.json.
func townRig() *entrypointRig {
	r := scratchRig()
	r.scratch = false
	r.kind[entrypointTownCfg] = entryRegular
	return r
}

func (r *entrypointRig) entrypoint() containerEntrypoint {
	record := func(argv []string) error {
		r.calls = append(r.calls, append([]string{}, argv...))
		return nil
	}
	return containerEntrypoint{
		self:      func() (string, error) { return entrypointBin, nil },
		getenv:    func(key string) string { return r.env[key] },
		run:       record,
		exec:      record,
		mountinfo: func() (string, error) { return r.mountinfo, r.mountErr },
		dirNames:  func(string) ([]string, error) { return r.dir, r.dirErr },
		lstat: func(path string) (entryKind, error) {
			if path == r.brokenPath {
				return entryAbsent, errors.New("lstat failed")
			}
			return r.kind[path], nil
		},
		exists: func(path string) (bool, error) {
			if path == r.brokenPath {
				return false, errors.New("stat failed")
			}
			return r.exists[path], nil
		},
		remove: func(path string) error {
			r.removed = append(r.removed, path)
			return r.removeErr
		},
		scratch: r.scratch,
	}
}

// execCalls counts the daemon exec argv recorded.
func (r *entrypointRig) execCalls() int {
	n := 0
	for _, argv := range r.calls {
		if len(argv) > 1 && argv[1] == "daemon" {
			n++
		}
	}
	return n
}

// failingInstall is a run seam whose gt install fails after after has changed
// the rig, the way a partial install leaves the town root.
func (r *entrypointRig) failingInstall(after func()) func(argv []string) error {
	return func(argv []string) error {
		r.calls = append(r.calls, append([]string{}, argv...))
		if len(argv) > 1 && argv[1] == "install" {
			after()
			return errors.New("install failed")
		}
		return nil
	}
}

// installCalls counts the gt install argv recorded.
func (r *entrypointRig) installCalls() int {
	n := 0
	for _, argv := range r.calls {
		if len(argv) > 1 && argv[1] == "install" {
			n++
		}
	}
	return n
}

// The compose project's command is the only way in: the live project passes
// none, so the default is off, and --scratch-town turns it on.
func TestContainerEntrypointScratchFlagIsWired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want bool
	}{
		{"live project passes no command", []string{}, false},
		{"scratch project passes the flag", []string{"--scratch-town"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got bool
			c := newDaemonContainerEntrypointCmd(func(scratch bool) error {
				got = scratch
				return nil
			})
			c.SetArgs(tc.args)
			c.SilenceUsage, c.SilenceErrors = true, true
			if err := c.Execute(); err != nil {
				t.Fatalf("execute container-entrypoint %v: %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("scratch = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestContainerEntrypointRefusesOutsideContainerMode(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"", "launchd"} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Parallel()
			r := townRig()
			r.env[config.ContainerModeEnv] = mode
			if err := runContainerEntrypoint(r.entrypoint()); err == nil {
				t.Fatal("expected a refusal outside container mode")
			}
			if len(r.calls) != 0 {
				t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

func TestContainerEntrypointRefusesTownRootThatIsNotAbsolute(t *testing.T) {
	t.Parallel()
	for _, root := range []string{"", "gt"} {
		t.Run("GT_TOWN_ROOT="+root, func(t *testing.T) {
			t.Parallel()
			// The town is present under the relative path, so only the
			// absolute-path check stands between this start and the exec.
			r := townRig()
			r.env[containerTownRootEnv] = root
			r.kind[filepath.Join(root, "mayor", "town.json")] = entryRegular
			if err := runContainerEntrypoint(r.entrypoint()); err == nil {
				t.Fatal("expected a refusal for a town root that is not absolute")
			}
			if len(r.calls) != 0 {
				t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

func TestContainerEntrypointReturnsDaemonExecError(t *testing.T) {
	t.Parallel()
	r := townRig()
	e := r.entrypoint()
	e.exec = func(argv []string) error { return errors.New("exec failed") }
	if err := runContainerEntrypoint(e); err == nil {
		t.Fatal("expected the exec failure to surface")
	}
}

func TestContainerEntrypointTownPresentExecsDaemonWithoutInstall(t *testing.T) {
	t.Parallel()
	r := townRig()
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	want := [][]string{
		{"git", "config", "--global", "user.name", "daemon"},
		{"git", "config", "--global", "user.email", "daemon@example.invalid"},
		{entrypointBin, "daemon", "run"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("commands = %v, want %v", r.calls, want)
	}
	if n := r.installCalls(); n != 0 {
		t.Errorf("recorded %d gt install argv, want none", n)
	}
}

func TestContainerEntrypointEmptyGitIdentityLeavesGitConfigAlone(t *testing.T) {
	t.Parallel()
	r := townRig()
	delete(r.env, containerGitUserEnv)
	delete(r.env, containerGitEmailEnv)
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	want := [][]string{{entrypointBin, "daemon", "run"}}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("commands = %v, want %v", r.calls, want)
	}
}

// With mayor/town.json absent, only the flag and the marker together start an
// install. Each row names its outcome; a refusal records no command.
func TestContainerEntrypointAbsentTownTriggerIsTheFlagAndMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		scratch bool
		marker  bool
		install bool
	}{
		{"neither flag nor marker refuses", false, false, false},
		{"marker without the flag refuses", false, true, false},
		{"flag without the marker refuses", true, false, false},
		{"flag with the marker installs", true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.scratch = tc.scratch
			if !tc.marker {
				delete(r.exists, entrypointMarker)
				r.dir = nil
			}
			err := runContainerEntrypoint(r.entrypoint())
			if !tc.install {
				if err == nil {
					t.Fatal("expected a refusal")
				}
				if len(r.calls) != 0 {
					t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
				}
				return
			}
			if err != nil {
				t.Fatalf("runContainerEntrypoint: %v", err)
			}
			if n := r.installCalls(); n != 1 {
				t.Errorf("recorded %d gt install argv, want exactly 1", n)
			}
		})
	}
}

// A town file that is not a regular file is refused, whatever it is, and runs
// nothing: no install argv, no daemon exec.
func TestContainerEntrypointNonRegularTownJSONRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		scratch bool
	}{
		{"with no scratch flag", false},
		{"with the scratch flag and marker", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.scratch = tc.scratch
			r.kind[entrypointTownCfg] = entryOther
			if err := runContainerEntrypoint(r.entrypoint()); err == nil {
				t.Fatal("expected a refusal for a mayor/town.json that is not a regular file")
			}
			if len(r.calls) != 0 {
				t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

// A symbolic link at mayor/town.json is refused, not followed. This runs the
// real Lstat over a real directory: a check that follows links would see the
// regular file the link names, start the daemon, and fail this test.
func TestContainerEntrypointSymlinkedTownJSONRefuses(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(townRoot, "elsewhere.json")
	if err := os.WriteFile(target, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(townRoot, "mayor", "town.json")); err != nil {
		t.Fatalf("make the symlinked mayor/town.json: %v", err)
	}
	r := townRig()
	r.env[containerTownRootEnv] = townRoot
	e := r.entrypoint()
	e.lstat = entrypointLstat
	if err := runContainerEntrypoint(e); err == nil {
		t.Fatal("expected a refusal for a symlinked mayor/town.json")
	}
	if len(r.calls) != 0 {
		t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
	}
}

// TestContainerEntrypointLstat drives each outcome of the real Lstat seam over a
// t.TempDir: absent, a regular file, a link, and a path that fails for a reason
// other than absence (a component is a regular file), which must be an error.
func TestContainerEntrypointLstat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular")
	if err := os.WriteFile(regular, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(regular, link); err != nil {
		t.Fatalf("make the symlink: %v", err)
	}
	for _, tc := range []struct {
		name    string
		path    string
		want    entryKind
		wantErr bool
	}{
		{"absent", filepath.Join(dir, "absent"), entryAbsent, false},
		{"regular file", regular, entryRegular, false},
		{"symbolic link is not followed", link, entryOther, false},
		{"beneath a regular file is an error, not absent", filepath.Join(regular, "child"), entryAbsent, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := entrypointLstat(tc.path)
			if (err != nil) != tc.wantErr {
				t.Fatalf("entrypointLstat(%q) error = %v, want error %v", tc.path, err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("entrypointLstat(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}

// entrypointMountinfo is a mountinfo table in which dir is a bind mount.
func entrypointMountinfo(dir string) string {
	return "36 35 98:0 /srv/scratch " + dir + " rw,noatime - ext4 /dev/root rw\n"
}

func TestContainerEntrypointScratchTownRefusesWhenAConditionFails(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		mutate func(r *entrypointRig)
	}{
		{"not a mount point", func(r *entrypointRig) {
			// The parent of the mount point is not itself one.
			r.mountinfo = "36 35 98:0 /srv /srv rw - ext4 /dev/root rw\n"
		}},
		{"mountinfo unreadable", func(r *entrypointRig) {
			r.mountErr = errors.New("read failed")
		}},
		{"no marker", func(r *entrypointRig) {
			delete(r.exists, entrypointMarker)
			r.dir = nil
		}},
		{"other files present", func(r *entrypointRig) {
			r.dir = []string{config.ScratchTownMarker, "stray.txt"}
		}},
		{"mayor/town.json unreadable", func(r *entrypointRig) {
			r.brokenPath = entrypointTownCfg
		}},
		{"marker unreadable", func(r *entrypointRig) {
			r.brokenPath = entrypointMarker
		}},
		{"town root unlistable", func(r *entrypointRig) {
			r.dirErr = errors.New("list failed")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			tc.mutate(r)
			if err := runContainerEntrypoint(r.entrypoint()); err == nil {
				t.Fatal("expected a refusal")
			}
			if len(r.calls) != 0 {
				t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

func TestContainerEntrypointScratchTownInstallsOnceBeforeDaemon(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	want := [][]string{
		{"git", "config", "--global", "user.name", "daemon"},
		{"git", "config", "--global", "user.email", "daemon@example.invalid"},
		{entrypointBin, "install", entrypointTown, "--git"},
		{entrypointBin, "daemon", "run"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("commands = %v, want %v", r.calls, want)
	}
	if n := r.installCalls(); n != 1 {
		t.Errorf("recorded %d gt install argv, want exactly 1", n)
	}
}

// The scratch gate admits a directory holding only the marker, so the install
// the entrypoint runs must accept exactly that directory. This drives the real
// install path at the seams the other install unit tests use (no beads and no
// git, which need a live Dolt and a subprocess) and checks it writes
// mayor/town.json rather than refusing the directory. The owner is passed, so
// the install does not ask the host's git config for one; the directory is a
// t.TempDir and nothing outside it is written.
func TestScratchTownInstallAcceptsTheMarkerOnlyDirectory(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, config.ScratchTownMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInstallWith(installOptions{noBeads: true, owner: "daemon@example.invalid"}, []string{townRoot}); err != nil {
		t.Fatalf("gt install into a marker-only directory was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "mayor", "town.json")); err != nil {
		t.Fatalf("mayor/town.json after install: %v", err)
	}
}

// A failed scratch install that wrote mayor/town.json must leave none behind:
// the next start would exec a half-written town. This runs the real filesystem
// seams over a t.TempDir, so the removal is what the disk shows.
func TestContainerEntrypointFailedScratchInstallLeavesNoTownJSON(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	marker := filepath.Join(townRoot, config.ScratchTownMarker)
	if err := os.WriteFile(marker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := scratchRig()
	r.env[containerTownRootEnv] = townRoot
	r.mountinfo = entrypointMountinfo(townRoot)
	e := r.entrypoint()
	e.lstat = entrypointLstat
	e.exists = entrypointPathExists
	e.dirNames = listDirNames
	e.remove = os.RemoveAll
	e.run = func(argv []string) error {
		r.calls = append(r.calls, append([]string{}, argv...))
		if len(argv) > 1 && argv[1] == "install" {
			// The install writes its town, then fails.
			if err := os.Mkdir(filepath.Join(townRoot, "mayor"), 0o755); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte("{}"), 0o600); err != nil {
				return err
			}
			return errors.New("install failed")
		}
		return nil
	}
	if err := runContainerEntrypoint(e); err == nil {
		t.Fatal("expected the failed install to surface")
	}
	if _, err := os.Lstat(filepath.Join(townRoot, "mayor", "town.json")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("mayor/town.json after a failed install: err = %v, want it removed", err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the scratch marker was not kept: %v", err)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("recorded %d daemon exec argv after a failed install, want none: %v", n, r.calls)
	}
}

// Each failure while cleaning up a failed install refuses, and the daemon is
// not exec'd. The install here wrote mayor/ into the town root.
func TestContainerEntrypointFailedInstallCleanupErrorsRefuse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		mutate func(r *entrypointRig)
	}{
		{"the town root cannot be listed after the install", func(r *entrypointRig) {
			r.dirErr = errors.New("list failed")
		}},
		{"a leftover entry cannot be removed", func(r *entrypointRig) {
			r.removeErr = errors.New("remove failed")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			e := r.entrypoint()
			e.run = r.failingInstall(func() {
				r.dir = []string{config.ScratchTownMarker, "mayor"}
				tc.mutate(r)
			})
			if err := runContainerEntrypoint(e); err == nil {
				t.Fatal("expected the failed install to surface")
			}
			if n := r.execCalls(); n != 0 {
				t.Errorf("recorded %d daemon exec argv, want none: %v", n, r.calls)
			}
		})
	}
}

// A failed install removes each entry the install wrote and keeps the marker.
func TestContainerEntrypointFailedInstallRemovesWhatItWrote(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	e := r.entrypoint()
	e.run = r.failingInstall(func() {
		r.dir = []string{config.ScratchTownMarker, "mayor", ".git"}
	})
	if err := runContainerEntrypoint(e); err == nil {
		t.Fatal("expected the failed install to surface")
	}
	want := []string{filepath.Join(entrypointTown, "mayor"), filepath.Join(entrypointTown, ".git")}
	if !reflect.DeepEqual(r.removed, want) {
		t.Errorf("removed %v, want %v (the marker is kept)", r.removed, want)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("recorded %d daemon exec argv, want none: %v", n, r.calls)
	}
}

func TestContainerEntrypointRunFailureStopsBeforeDaemonExec(t *testing.T) {
	t.Parallel()
	for _, failing := range []string{"git", "install"} {
		t.Run("fails="+failing, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			e := r.entrypoint()
			e.run = func(argv []string) error {
				r.calls = append(r.calls, append([]string{}, argv...))
				if argv[0] == failing || (len(argv) > 1 && argv[1] == failing) {
					return errors.New("command failed")
				}
				return nil
			}
			if err := runContainerEntrypoint(e); err == nil {
				t.Fatalf("expected the %s failure to surface", failing)
			}
			for _, argv := range r.calls {
				if len(argv) > 1 && argv[1] == "daemon" {
					t.Fatalf("daemon exec ran after a failed %s command: %v", failing, r.calls)
				}
			}
		})
	}
}

func TestContainerEntrypointRefusesWhenTheGtBinaryIsUnknown(t *testing.T) {
	t.Parallel()
	r := townRig()
	e := r.entrypoint()
	e.self = func() (string, error) { return "", errors.New("no executable") }
	if err := runContainerEntrypoint(e); err == nil {
		t.Fatal("expected a refusal when the gt binary cannot be found")
	}
	if len(r.calls) != 0 {
		t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
	}
}

func TestReadTextFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "mountinfo")
	if err := os.WriteFile(path, []byte("text\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readTextFile(path)
	if err != nil || got != "text\n" {
		t.Errorf("readTextFile = %q, %v; want the file's text", got, err)
	}
	if _, err := readTextFile(filepath.Join(dir, "missing")); err == nil {
		t.Error("readTextFile of a missing file returned no error")
	}
}

func TestListDirNames(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	got, err := listDirNames(dir)
	if err != nil || !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Errorf("listDirNames = %v, %v; want [a b]", got, err)
	}
	if _, err := listDirNames(filepath.Join(dir, "missing")); err == nil {
		t.Error("listDirNames of a missing directory returned no error")
	}
}

func TestEntrypointPathExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	file := filepath.Join(dir, "present")
	if err := os.WriteFile(file, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if ok, err := entrypointPathExists(file); !ok || err != nil {
		t.Errorf("entrypointPathExists(present) = %v, %v; want true, nil", ok, err)
	}
	if ok, err := entrypointPathExists(filepath.Join(dir, "absent")); ok || err != nil {
		t.Errorf("entrypointPathExists(absent) = %v, %v; want false, nil", ok, err)
	}
	// A path beneath a regular file fails to stat with a reason other than
	// absence, which must come back as an error rather than as "no".
	if _, err := entrypointPathExists(filepath.Join(file, "child")); err == nil {
		t.Error("entrypointPathExists beneath a regular file returned no error")
	}
}

func TestIsMountPoint(t *testing.T) {
	t.Parallel()
	const table = "21 1 0:20 / / rw - tmpfs tmpfs rw\n" +
		"36 35 98:0 /srv/scratch /gt/data rw,noatime - ext4 /dev/root rw\n" +
		"40 35 98:0 /srv/my\\040dir /srv/my\\040dir rw - ext4 /dev/root rw\n"
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"a mount target", "/gt/data", true},
		{"the parent of a mount target", "/gt", false},
		{"an escaped mount point", "/srv/my dir", true},
		{"a trailing slash is the same path", "/gt/data/", true},
		{"a relative path is never a mount point", "gt/data", false},
		{"an empty table", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isMountPoint(table, tc.path); got != tc.want {
				t.Errorf("isMountPoint(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
