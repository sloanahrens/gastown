package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

const (
	entrypointTown    = "/gt"
	entrypointBin     = "/usr/bin/gt"
	entrypointMarker  = "/gt/.gt-scratch-town"
	entrypointTownCfg = "/gt/mayor/town.json"
	// entrypointIncomplete is the install-incomplete marker in the test town.
	entrypointIncomplete = "/gt/.gt-install-incomplete"
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
	// created records the paths the create seam made; createErr fails it.
	created   []string
	createErr error
	// execEnv is the environment the daemon exec was handed; files are the
	// files readFile can read (readErr fails every read); omTokenFile is the
	// --om-token-file value.
	execEnv     []string
	files       map[string]string
	readErr     error
	omTokenFile string
	scratch     bool
	// doltHost is the --dolt-host value and doltHostSet says the flag was given,
	// since an empty host is refused and differs from no flag.
	doltHost    string
	doltHostSet bool
	calls       [][]string
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
		self:   func() (string, error) { return entrypointBin, nil },
		getenv: func(key string) string { return r.env[key] },
		run:    record,
		exec: func(argv []string, env []string) error {
			r.calls = append(r.calls, append([]string{}, argv...))
			r.execEnv = env
			return nil
		},
		readFile: func(path string) (string, error) {
			if r.readErr != nil {
				return "", r.readErr
			}
			text, ok := r.files[path]
			if !ok {
				return "", errors.New("no such file")
			}
			return text, nil
		},
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
		create: func(path string) error {
			if r.createErr != nil {
				return r.createErr
			}
			r.created = append(r.created, path)
			return nil
		},
		remove: func(path string) error {
			r.removed = append(r.removed, path)
			return r.removeErr
		},
		scratch:     r.scratch,
		omTokenFile: r.omTokenFile,
		doltHost:    r.doltHost,
		doltHostSet: r.doltHostSet,
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
		{"scratch project passes the flag", []string{"--" + config.ScratchTownFlag}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got bool
			c := newDaemonContainerEntrypointCmd(func(flags entrypointFlags) error {
				got = flags.scratch
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

// The entrypoint parses --dolt-host into the flags it acts on, and it tells a
// host given as the empty string apart from no flag at all: the empty one is
// refused, so the two cannot be one value.
func TestContainerEntrypointDoltHostFlagIsWired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    []string
		want    string
		wantSet bool
	}{
		{"no flag", []string{}, "", false},
		{"the flag", []string{"--dolt-host", "scratch-dolt"}, "scratch-dolt", true},
		{"an empty value", []string{"--dolt-host="}, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got entrypointFlags
			c := newDaemonContainerEntrypointCmd(func(flags entrypointFlags) error {
				got = flags
				return nil
			})
			c.SetArgs(tc.args)
			c.SilenceUsage, c.SilenceErrors = true, true
			if err := c.Execute(); err != nil {
				t.Fatalf("execute container-entrypoint %v: %v", tc.args, err)
			}
			if got.doltHost != tc.want || got.doltHostSet != tc.wantSet {
				t.Errorf("--dolt-host parsed as %q (set %v), want %q (set %v)", got.doltHost, got.doltHostSet, tc.want, tc.wantSet)
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

// A Dolt host belongs to the scratch town's install alone: given without
// --scratch-town it is never used, and an empty one names nothing. Each row
// refuses before any command runs; removing its check lets the start go on with
// a host no command acted on, so the row goes red.
func TestContainerEntrypointRefusesADoltHostItCannotUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rig     func() *entrypointRig
		host    string
		wantMsg string
	}{
		{"a host without the scratch flag", townRig, "scratch-dolt", "without --" + config.ScratchTownFlag},
		{"an empty host in the scratch town", scratchRig, "", "is empty"},
		{"an empty host with no scratch flag", townRig, "", "is empty"},
		{"a host of spaces", scratchRig, "  ", "is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.rig()
			r.doltHost, r.doltHostSet = tc.host, true

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil || !strings.Contains(err.Error(), config.DoltHostFlag) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %v, want a refusal naming --%s and saying %q", err, config.DoltHostFlag, tc.wantMsg)
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

func TestContainerEntrypointReturnsDaemonExecError(t *testing.T) {
	t.Parallel()
	r := townRig()
	e := r.entrypoint()
	e.exec = func([]string, []string) error { return errors.New("exec failed") }
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
	// The marker brackets the install: created before it, removed after it
	// succeeded.
	if !reflect.DeepEqual(r.created, []string{entrypointIncomplete}) || !reflect.DeepEqual(r.removed, []string{entrypointIncomplete}) {
		t.Errorf("incomplete marker created %v and removed %v, want it created and then removed once", r.created, r.removed)
	}
}

// With --dolt-host the scratch install is given the same flag and value, so the
// town it writes reaches the scratch project's Dolt server. The flag's name is
// the one the render passes, both read from config.DoltHostFlag. Without the
// flag the install is what it always was
// (TestContainerEntrypointScratchTownInstallsOnceBeforeDaemon).
func TestContainerEntrypointScratchTownPassesTheDoltHostToTheInstall(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.doltHost, r.doltHostSet = "scratch-dolt", true
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	want := [][]string{
		{"git", "config", "--global", "user.name", "daemon"},
		{"git", "config", "--global", "user.email", "daemon@example.invalid"},
		{entrypointBin, "install", entrypointTown, "--git", "--" + config.DoltHostFlag, "scratch-dolt"},
		{entrypointBin, "daemon", "run"},
	}
	if !reflect.DeepEqual(r.calls, want) {
		t.Errorf("commands = %v, want %v", r.calls, want)
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
// seams over a t.TempDir, so the removal is what the disk shows. It is hermetic:
// the town root is a t.TempDir, the mountinfo text is a fixture naming it, and
// run is a recording fake whose install branch writes the temp directory itself,
// so no gt install and no other process runs.
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
	e.create = entrypointCreateExclusive
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
	if _, err := os.Lstat(filepath.Join(townRoot, config.ScratchInstallIncompleteMarker)); err != nil {
		t.Errorf("the install-incomplete marker was not kept after a failed install: %v", err)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("recorded %d daemon exec argv after a failed install, want none: %v", n, r.calls)
	}
}

// A failed install whose cleanup also fails leaves mayor/town.json on disk, and
// the next start must still refuse: the incomplete marker, not town.json, decides.
// Both starts run the real filesystem seams over a t.TempDir with a recording
// fake for every command, so nothing outside the temp directory is touched; the
// first start's removal is made to fail for everything but the marker, the way a
// read-only or busy entry would.
func TestContainerEntrypointFailedInstallWithFailedCleanupLeavesTheTownUnstartable(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(townRoot, config.ScratchTownMarker), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	r := scratchRig()
	r.env[containerTownRootEnv] = townRoot
	r.mountinfo = entrypointMountinfo(townRoot)
	realSeams := func(scratch bool) containerEntrypoint {
		e := r.entrypoint()
		e.lstat = entrypointLstat
		e.exists = entrypointPathExists
		e.create = entrypointCreateExclusive
		e.dirNames = listDirNames
		e.remove = os.RemoveAll
		e.scratch = scratch
		return e
	}

	first := realSeams(true)
	first.run = func(argv []string) error {
		r.calls = append(r.calls, append([]string{}, argv...))
		if len(argv) > 1 && argv[1] == "install" {
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
	first.remove = func(string) error { return errors.New("remove failed") }
	if err := runContainerEntrypoint(first); err == nil {
		t.Fatal("expected the failed install to surface")
	}
	if _, err := os.Lstat(filepath.Join(townRoot, "mayor", "town.json")); err != nil {
		t.Fatalf("the failed removal should have left mayor/town.json on disk: %v", err)
	}

	for _, scratch := range []bool{false, true} {
		before := len(r.calls)
		err := runContainerEntrypoint(realSeams(scratch))
		if err == nil || !strings.Contains(err.Error(), config.ScratchInstallIncompleteMarker) {
			t.Errorf("scratch=%v: next start error = %v, want a refusal naming the %s marker", scratch, err, config.ScratchInstallIncompleteMarker)
		}
		if len(r.calls) != before {
			t.Errorf("scratch=%v: the next start ran %v, want nothing", scratch, r.calls[before:])
		}
	}
}

// The marker is created before the install runs, and the install is not run at
// all when it cannot be created: an install never starts unbracketed.
func TestContainerEntrypointCreateFailureRunsNoInstall(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.createErr = errors.New("file exists")
	err := runContainerEntrypoint(r.entrypoint())
	if err == nil || !strings.Contains(err.Error(), config.ScratchInstallIncompleteMarker) {
		t.Fatalf("error = %v, want a refusal naming the incomplete marker", err)
	}
	if n := r.installCalls(); n != 0 {
		t.Errorf("recorded %d gt install argv after the marker could not be created, want none", n)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("recorded %d daemon exec argv, want none", n)
	}
}

// Every install failure keeps the marker, whether the cleanup worked or not:
// it is removed only after a good install.
func TestContainerEntrypointFailedInstallKeepsTheIncompleteMarker(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		removeErr error
	}{
		{"cleanup succeeds", nil},
		{"cleanup fails", errors.New("remove failed")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			e := r.entrypoint()
			e.run = r.failingInstall(func() {
				r.dir = []string{config.ScratchTownMarker, config.ScratchInstallIncompleteMarker, "mayor"}
				r.removeErr = tc.removeErr
			})
			if err := runContainerEntrypoint(e); err == nil {
				t.Fatal("expected the failed install to surface")
			}
			if !reflect.DeepEqual(r.created, []string{entrypointIncomplete}) {
				t.Errorf("created %v, want the incomplete marker once", r.created)
			}
			for _, path := range r.removed {
				if path == entrypointIncomplete {
					t.Errorf("a failed install removed the incomplete marker: removed %v", r.removed)
				}
			}
			if n := r.execCalls(); n != 0 {
				t.Errorf("recorded %d daemon exec argv, want none", n)
			}
		})
	}
}

// A good install whose marker cannot be removed refuses: the next start would
// refuse on the marker anyway, and starting a daemon beside it is not the state
// the marker allows.
func TestContainerEntrypointMarkerRemovalFailureAfterAGoodInstallRefuses(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.removeErr = errors.New("remove failed")
	err := runContainerEntrypoint(r.entrypoint())
	if err == nil || !strings.Contains(err.Error(), config.ScratchInstallIncompleteMarker) {
		t.Fatalf("error = %v, want a refusal naming the incomplete marker", err)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("recorded %d daemon exec argv, want none", n)
	}
}

// With the incomplete marker present the entrypoint refuses and runs nothing,
// whatever mayor/town.json is, whether --scratch-town is given, and however the
// marker is spelled on disk (a link counts: it is not followed or ignored).
// Without the marker a good town.json execs the daemon.
func TestContainerEntrypointIncompleteMarkerDecides(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		rig     func() *entrypointRig
		marker  entryKind // what Lstat finds at the incomplete marker
		wantRun bool      // the daemon is exec'd
	}{
		{"marker with a good town.json refuses", townRig, entryRegular, false},
		{"symlinked marker with a good town.json refuses", townRig, entryOther, false},
		{"marker with the scratch flag and no town.json refuses", scratchRig, entryRegular, false},
		{"no marker with a good town.json execs", townRig, entryAbsent, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.rig()
			r.kind[entrypointIncomplete] = tc.marker
			err := runContainerEntrypoint(r.entrypoint())
			if tc.wantRun {
				if err != nil {
					t.Fatalf("runContainerEntrypoint: %v", err)
				}
				if n := r.execCalls(); n != 1 {
					t.Errorf("recorded %d daemon exec argv, want 1", n)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), config.ScratchInstallIncompleteMarker) {
				t.Fatalf("error = %v, want a refusal naming the incomplete marker", err)
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %v, want nothing", r.calls)
			}
		})
	}
}

// A read of the marker that fails refuses: a marker that cannot be ruled out
// counts as present.
func TestContainerEntrypointUnreadableIncompleteMarkerRefuses(t *testing.T) {
	t.Parallel()
	r := townRig()
	r.brokenPath = entrypointIncomplete
	if err := runContainerEntrypoint(r.entrypoint()); err == nil {
		t.Fatal("expected a refusal when the marker cannot be read")
	}
	if len(r.calls) != 0 {
		t.Errorf("a refusal ran %v, want nothing", r.calls)
	}
}

// The real exclusive create makes an empty file, and refuses a path that
// exists, a dangling symbolic link included. A t.TempDir only.
func TestEntrypointCreateExclusive(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "marker")
	if err := entrypointCreateExclusive(path); err != nil {
		t.Fatalf("first create: %v", err)
	}
	if err := entrypointCreateExclusive(path); err == nil {
		t.Error("second create succeeded over an existing file")
	}
	link := filepath.Join(dir, "link")
	if err := os.Symlink(filepath.Join(dir, "absent"), link); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := entrypointCreateExclusive(link); err == nil {
		t.Error("create succeeded through a dangling symbolic link")
	}
	if _, err := os.Lstat(filepath.Join(dir, "absent")); err == nil {
		t.Error("create followed the link and made its target")
	}
}

// The root command's persistent pre-run runs before this command, and the
// town-running gate in it refuses a town whose config does not parse, which a
// scratch town does not yet have. The gate is keyed by the exact command path
// (bdHandshakeGatedCommands), and this command is not in it, so the gate does
// not run here. The other hooks it runs are read-only or write outside the town
// root: the usage log goes to gtDataDir, the registry loads only from a town
// found by town.json, and the branch check needs a .git the scratch directory
// does not have until the install runs.
func TestContainerEntrypointIsNotGatedByTheTownHandshake(t *testing.T) {
	t.Parallel()
	if requiresBDHandshake(daemonContainerEntrypointCmd) {
		t.Fatalf("%q is in bdHandshakeGatedCommands: the gate would refuse the scratch town before the entrypoint ran",
			daemonContainerEntrypointCmd.CommandPath())
	}
	if got, want := daemonContainerEntrypointCmd.CommandPath(), "gt daemon container-entrypoint"; got != want {
		t.Errorf("command path = %q, want %q", got, want)
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

// omTokenFixture stands in for a subscription token: it is not a secret, and no
// failure message below prints it or any other environment value.
const omTokenFixture = "fixture-om-token-0000"

// The entrypoint parses --om-token-file into the flags it acts on, and with the
// flag absent it names no token file.
func TestContainerEntrypointOMTokenFlagIsWired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"no flag", []string{}, ""},
		{"the flag", []string{"--om-token-file", "/run/secrets/om-token"}, "/run/secrets/om-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got string
			c := newDaemonContainerEntrypointCmd(func(flags entrypointFlags) error {
				got = flags.omTokenFile
				return nil
			})
			c.SetArgs(tc.args)
			c.SilenceUsage, c.SilenceErrors = true, true
			if err := c.Execute(); err != nil {
				t.Fatalf("execute container-entrypoint %v: %v", tc.args, err)
			}
			if got != tc.want {
				t.Errorf("--om-token-file parsed as %q, want %q", got, tc.want)
			}
		})
	}
}

// An API credential in the container environment outranks the subscription
// token and bills API rates, so the entrypoint refuses before anything runs,
// scratch or not, naming the variable and never its value. An empty value is
// not set (a compose pass-through of an unset host variable arrives empty).
func TestContainerEntrypointRefusesAPICredentials(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
		for _, rigOf := range []struct {
			kind string
			rig  func() *entrypointRig
		}{{"a town", townRig}, {"a scratch town", scratchRig}} {
			t.Run(name+" in "+rigOf.kind, func(t *testing.T) {
				t.Parallel()
				r := rigOf.rig()
				r.env[name] = "credential-fixture-value"
				r.omTokenFile = "/run/secrets/om-token"
				r.files = map[string]string{"/run/secrets/om-token": omTokenFixture}

				err := runContainerEntrypoint(r.entrypoint())

				if err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("error = %v, want a refusal naming %s", err, name)
				}
				if strings.Contains(err.Error(), "credential-fixture-value") {
					t.Error("the refusal printed the credential's value")
				}
				if len(r.calls) != 0 {
					t.Errorf("a refusal ran %d commands, want none", len(r.calls))
				}
			})
		}
	}
	t.Run("an empty value is not set", func(t *testing.T) {
		t.Parallel()
		r := townRig()
		r.env["ANTHROPIC_API_KEY"] = ""
		r.env["ANTHROPIC_AUTH_TOKEN"] = ""
		if err := runContainerEntrypoint(r.entrypoint()); err != nil {
			t.Fatalf("an empty credential variable refused the start: %v", err)
		}
	})
}

// The token file's trimmed contents become CLAUDE_CODE_OAUTH_TOKEN in the exec'd
// daemon's environment and nowhere else: not in an argv, not in the environment
// of the install or git commands, which run through the argv seam.
func TestContainerEntrypointLoadsTheOMToken(t *testing.T) {
	t.Parallel()
	for _, rigOf := range []struct {
		kind string
		rig  func() *entrypointRig
	}{{"a town", townRig}, {"a scratch town", scratchRig}} {
		t.Run(rigOf.kind, func(t *testing.T) {
			t.Parallel()
			r := rigOf.rig()
			r.omTokenFile = "/run/secrets/om-token"
			r.files = map[string]string{"/run/secrets/om-token": "  " + omTokenFixture + " \n"}

			if err := runContainerEntrypoint(r.entrypoint()); err != nil {
				t.Fatalf("runContainerEntrypoint: %v", err)
			}

			if len(r.execEnv) != 1 || r.execEnv[0] != "CLAUDE_CODE_OAUTH_TOKEN="+omTokenFixture {
				t.Errorf("daemon environment additions = %d entries, want exactly CLAUDE_CODE_OAUTH_TOKEN set to the trimmed token (values not printed)", len(r.execEnv))
			}
			for _, argv := range r.calls {
				if strings.Contains(strings.Join(argv, " "), omTokenFixture) {
					t.Errorf("an argv carries the token: %v", argv[:2])
				}
			}
			if n := r.execCalls(); n != 1 {
				t.Errorf("recorded %d daemon exec argv, want 1", n)
			}
		})
	}
}

// With the flag unset no token is loaded and nothing refuses.
func TestContainerEntrypointWithoutTheTokenFileLoadsNoToken(t *testing.T) {
	t.Parallel()
	r := townRig()
	r.files = map[string]string{"/run/secrets/om-token": omTokenFixture}
	r.readErr = errors.New("a read was attempted")

	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	if len(r.execEnv) != 0 {
		t.Errorf("daemon environment additions = %d entries, want none", len(r.execEnv))
	}
}

// An unreadable file, an empty one and a whitespace-only one each refuse,
// naming the path alone; nothing runs; and neither the read error's own text
// nor the token reaches the message.
func TestContainerEntrypointRefusesAnUnusableTokenFile(t *testing.T) {
	t.Parallel()
	const path = "/run/secrets/om-token"
	for _, tc := range []struct {
		name    string
		files   map[string]string
		readErr error
		want    string
	}{
		{"unreadable", map[string]string{path: omTokenFixture}, errors.New("boom " + omTokenFixture), "could not be read"},
		{"missing", map[string]string{}, nil, "could not be read"},
		{"empty", map[string]string{path: ""}, nil, "is empty"},
		{"whitespace only", map[string]string{path: " \n\t "}, nil, "is empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.omTokenFile = path
			r.files, r.readErr = tc.files, tc.readErr

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want a refusal naming the path and saying %q", err, tc.want)
			}
			if strings.Contains(err.Error(), omTokenFixture) {
				t.Error("the refusal carries the token or the read error's own text")
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none", len(r.calls))
			}
		})
	}
}

// mergeEnv replaces a variable the additions name, so no name appears twice for
// the exec to choose between, and keeps everything else in order.
func TestMergeEnv(t *testing.T) {
	t.Parallel()
	got := mergeEnv(
		[]string{"A=1", "CLAUDE_CODE_OAUTH_TOKEN=old", "B=2"},
		[]string{"CLAUDE_CODE_OAUTH_TOKEN=new", "C=3"},
	)
	want := []string{"A=1", "B=2", "CLAUDE_CODE_OAUTH_TOKEN=new", "C=3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mergeEnv = %v, want %v", got, want)
	}
	if got := mergeEnv([]string{"A=1"}, nil); !reflect.DeepEqual(got, []string{"A=1"}) {
		t.Errorf("mergeEnv with no additions = %v, want the base", got)
	}
}
