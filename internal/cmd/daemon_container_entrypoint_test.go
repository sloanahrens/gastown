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
	// brokenPath makes its stat fail, to test the fail-closed read.
	brokenPath string
	dir        []string
	dirErr     error
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
		dir:       []string{config.ScratchTownMarker},
		scratch:   true,
	}
}

// townRig is a container whose town root already holds mayor/town.json.
func townRig() *entrypointRig {
	r := scratchRig()
	r.scratch = false
	r.exists[entrypointTownCfg] = true
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
		exists: func(path string) (bool, error) {
			if path == r.brokenPath {
				return false, errors.New("stat failed")
			}
			return r.exists[path], nil
		},
		scratch: r.scratch,
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
			r.exists[filepath.Join(root, "mayor", "town.json")] = true
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

func TestContainerEntrypointAbsentTownRefusesWithoutScratchFlag(t *testing.T) {
	t.Parallel()
	// Every scratch condition holds, so the refusal is the only thing that
	// stops an install. This is the live project's shape: no flag, no town.
	r := scratchRig()
	r.scratch = false
	if err := runContainerEntrypoint(r.entrypoint()); err == nil {
		t.Fatal("expected a refusal with no mayor/town.json and no scratch flag")
	}
	if len(r.calls) != 0 {
		t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
	}
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
// mayor/town.json rather than refusing the directory.
func TestScratchTownInstallAcceptsTheMarkerOnlyDirectory(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, config.ScratchTownMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runInstallWith(installOptions{noBeads: true}, []string{townRoot}); err != nil {
		t.Fatalf("gt install into a marker-only directory was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "mayor", "town.json")); err != nil {
		t.Fatalf("mayor/town.json after install: %v", err)
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
