package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
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
	execEnv []string
	files   map[string]string
	readErr error
	// patrols is the daemon patrol settings the patrols seam returns, and
	// patrolErr fails it.
	patrols     *config.DaemonPatrolConfig
	patrolErr   error
	omTokenFile string
	scratch     bool
	// doltHost is the --dolt-host value and doltHostSet says the flag was given,
	// since an empty host is refused and differs from no flag.
	doltHost    string
	doltHostSet bool
	doltPort    int
	doltPortSet bool

	forgeHost         string
	forgeHostSet      bool
	forgeTokenFile    string
	forgeTokenFileSet bool
	// pluginTownRoots is the town root each plugin install was given, pluginRanAt
	// is len(calls) when the last one ran (so a test sees its place in the
	// command sequence without adding to the recorded commands), and pluginErr
	// fails it.
	pluginTownRoots []string
	pluginRanAt     int
	pluginErr       error
	// installs records the installer seams in the order the start-up ran them;
	// mangoTownRoots is the town root each mango install was given, and mangoErr
	// fails it.
	installs       []string
	mangoTownRoots []string
	mangoErr       error
	// deployTownRoots is the town root each directive deploy was given, deployRanAt
	// is len(calls) when the last one ran, and deployErr fails it. logs holds each
	// line the entrypoint logged.
	deployTownRoots []string
	deployRanAt     int
	deployErr       error
	logs            []string
	calls           [][]string
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
		patrols: func(string) (*config.DaemonPatrolConfig, error) {
			return r.patrols, r.patrolErr
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
		installPlugins: func(townRoot string) error {
			r.installs = append(r.installs, "plugins")
			r.pluginTownRoots = append(r.pluginTownRoots, townRoot)
			r.pluginRanAt = len(r.calls)
			return r.pluginErr
		},
		installMango: func(townRoot string) error {
			r.installs = append(r.installs, "mango")
			r.mangoTownRoots = append(r.mangoTownRoots, townRoot)
			return r.mangoErr
		},
		deployDirectives: func(townRoot string) error {
			r.installs = append(r.installs, "directives")
			r.deployTownRoots = append(r.deployTownRoots, townRoot)
			r.deployRanAt = len(r.calls)
			return r.deployErr
		},
		logf: func(format string, args ...any) {
			r.logs = append(r.logs, fmt.Sprintf(format, args...))
		},
		scratch:     r.scratch,
		omTokenFile: r.omTokenFile,
		doltHost:    r.doltHost,
		doltHostSet: r.doltHostSet,
		doltPort:    r.doltPort,
		doltPortSet: r.doltPortSet,

		forgeHost:         r.forgeHost,
		forgeHostSet:      r.forgeHostSet,
		forgeTokenFile:    r.forgeTokenFile,
		forgeTokenFileSet: r.forgeTokenFileSet,
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
		{"a host beginning with a dash", scratchRig, "-scratch-dolt", "begins with a dash"},
		{"a host beginning with a dash after spaces", scratchRig, "  -h", "begins with a dash"},
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

// A start that finds the town already installed runs no install, so the host
// --dolt-host names would be written nowhere. One the town already names is the
// host the install wrote, and the compose file's unchanged command line passes
// it on every restart, so that restart goes on; any other host is refused
// rather than dropped in silence. A town config that cannot be read or parsed
// refuses too, since taking it as naming no host would drop the flag.
func TestContainerEntrypointRefusesADoltHostTheInstalledTownDoesNotName(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		files   map[string]string
		readErr error
		wantMsg string
	}{
		{"a town naming another host", map[string]string{entrypointTownCfg: `{"dolt":{"host":"other-dolt","port":3306}}`}, nil, "is not the Dolt server"},
		{"a town naming no host", map[string]string{entrypointTownCfg: `{"type":"town"}`}, nil, "names none"},
		{"an unreadable town config", nil, errors.New("permission denied"), "read the town's"},
		{"a town config that does not parse", map[string]string{entrypointTownCfg: `{"dolt":`}, nil, "parse the town's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.kind[entrypointTownCfg] = entryRegular
			r.files, r.readErr = tc.files, tc.readErr
			r.doltHost, r.doltHostSet = "scratch-dolt", true

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

// The scratch compose file passes --dolt-host scratch-dolt on every start, so
// the restart after the install must go on: the host it passes is the one the
// install wrote. Hostnames are compared without case, as DNS reads them.
func TestContainerEntrypointTakesTheDoltHostTheInstalledTownNames(t *testing.T) {
	t.Parallel()
	for _, townCfg := range []string{
		`{"dolt":{"host":"scratch-dolt","port":3306}}`,
		`{"dolt":{"host":"Scratch-Dolt","port":3306}}`,
	} {
		t.Run(townCfg, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.kind[entrypointTownCfg] = entryRegular
			r.files = map[string]string{entrypointTownCfg: townCfg}
			r.doltHost, r.doltHostSet = "scratch-dolt", true
			r.doltPort, r.doltPortSet = 3306, true

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
				t.Errorf("recorded %d gt install argv, want none: the town is already there", n)
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

// The token file's contents, whitespace removed, become CLAUDE_CODE_OAUTH_TOKEN
// in the exec'd daemon's environment and nowhere else: not in an argv, not in
// the environment of the install or git commands, which run through the argv
// seam.
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

// A token saved hard-wrapped has a line break inside it, which the claude CLI
// rejects, so the loader joins the token's parts rather than trimming only the
// ends.
func TestEntrypointOMTokenStripsEmbeddedLineBreaks(t *testing.T) {
	t.Parallel()
	const path = "/run/secrets/om-token"
	const token = "hardwrapped-om-token-0123456789"
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"split by a newline", token[:8] + "\n" + token[8:]},
		{"split by a CRLF", token[:8] + "\r\n" + token[8:]},
		{"split twice", token[:5] + "\n" + token[5:12] + "\r\n" + token[12:]},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := townRig()
			r.omTokenFile = path
			r.files = map[string]string{path: tc.content}

			if err := runContainerEntrypoint(r.entrypoint()); err != nil {
				t.Fatalf("runContainerEntrypoint: %v", err)
			}
			if len(r.execEnv) != 1 || r.execEnv[0] != "CLAUDE_CODE_OAUTH_TOKEN="+token {
				t.Errorf("daemon environment additions = %d entries, want one value equal to the token with its line breaks removed", len(r.execEnv))
			}
		})
	}
}

// Whitespace around the token, including a trailing newline or CRLF, is not
// part of it and never reaches the environment.
func TestEntrypointOMTokenStripsTrailingNewlineAndSpaces(t *testing.T) {
	t.Parallel()
	const path = "/run/secrets/om-token"
	const token = "trimmed-om-token-0123456789"
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"a trailing newline", token + "\n"},
		{"a trailing CRLF", token + "\r\n"},
		{"surrounding spaces", "  " + token + "  "},
		{"leading and trailing tabs", "\t" + token + "\t\n"},
		{"trailing spaces then a CRLF", token + "  \r\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := townRig()
			r.omTokenFile = path
			r.files = map[string]string{path: tc.content}

			if err := runContainerEntrypoint(r.entrypoint()); err != nil {
				t.Fatalf("runContainerEntrypoint: %v", err)
			}
			if len(r.execEnv) != 1 || r.execEnv[0] != "CLAUDE_CODE_OAUTH_TOKEN="+token {
				t.Errorf("daemon environment additions = %d entries, want one value equal to the token", len(r.execEnv))
			}
		})
	}
}

// A file holding only whitespace is empty once the whitespace is removed: it
// refuses with the empty-file message and runs nothing.
func TestEntrypointOMTokenEmptyAfterStripIsRefused(t *testing.T) {
	t.Parallel()
	const path = "/run/secrets/om-token"
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"a newline", "\n"},
		{"a CRLF", "\r\n"},
		{"spaces and tabs", " \t "},
		{"every whitespace character", " \t\r\n \t\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.omTokenFile = path
			r.files = map[string]string{path: tc.content}

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil || !strings.Contains(err.Error(), path) || !strings.Contains(err.Error(), "is empty") {
				t.Fatalf("error = %v, want a refusal naming the path and saying the file is empty", err)
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none", len(r.calls))
			}
		})
	}
}

// No returned error carries the token or any 8-character run of it: not when
// the underlying read error's text holds it, and not when a later refusal
// happens after the token was loaded.
func TestEntrypointOMTokenErrorsNeverContainTheToken(t *testing.T) {
	t.Parallel()
	const path = "/run/secrets/om-token"
	const token = "leakchecked-xyzzy-0123456789"
	for _, tc := range []struct {
		name  string
		setup func(*entrypointRig)
	}{
		{"the read error embeds the token", func(r *entrypointRig) {
			r.readErr = errors.New("open " + path + ": boom " + token)
		}},
		{"a later refusal after the token loaded", func(r *entrypointRig) {
			r.files = map[string]string{path: "  " + token + "\n"}
			r.kind[filepath.Join(entrypointTown, config.ScratchInstallIncompleteMarker)] = entryRegular
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := townRig()
			r.omTokenFile = path
			tc.setup(r)

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil {
				t.Fatal("runContainerEntrypoint: want a refusal, got none")
			}
			for i := 0; i+8 <= len(token); i++ {
				if strings.Contains(err.Error(), token[i:i+8]) {
					t.Errorf("the refusal carries an 8-character run of the token at offset %d", i)
				}
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

// The scratch Dolt serves a port other than gt's default 3307, so the install
// is given the port beside the host. Dropping the append in installArgv turns
// this red (gt-i2ekc).
func TestContainerEntrypointInstallArgv(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.doltHost, r.doltHostSet = "scratch-dolt", true
	r.doltPort, r.doltPortSet = 3306, true
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	want := []string{entrypointBin, "install", entrypointTown, "--git",
		"--" + config.DoltHostFlag, "scratch-dolt", "--" + config.DoltPortFlag, "3306"}
	var got []string
	for _, c := range r.calls {
		if len(c) > 1 && c[1] == "install" {
			got = c
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("install command = %v, want %v", got, want)
	}
}

// A port with no host to belong to, outside 1..65535, or without the scratch
// flag is refused before the first command.
func TestContainerEntrypointRefusesADoltPortItCannotUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rig     func() *entrypointRig
		host    bool
		port    int
		wantMsg string
	}{
		{"a port without a host", scratchRig, false, 3306, "without --" + config.DoltHostFlag},
		{"port zero", scratchRig, true, 0, "is not a port"},
		{"a port above 65535", scratchRig, true, 70000, "is not a port"},
		{"a port with a host but no scratch flag", townRig, true, 3306, "without --" + config.ScratchTownFlag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := tc.rig()
			if tc.host {
				r.doltHost, r.doltHostSet = "scratch-dolt", true
			}
			r.doltPort, r.doltPortSet = tc.port, true

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %v, want a refusal saying %q", err, tc.wantMsg)
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

// --dolt-port reaches the flags the entrypoint acts on, and port zero given on
// purpose is told apart from no flag.
func TestContainerEntrypointDoltPortFlagIsWired(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		args    []string
		want    int
		wantSet bool
	}{
		{"no flag", []string{}, 0, false},
		{"the flag", []string{"--dolt-port", "3306"}, 3306, true},
		{"zero on purpose", []string{"--dolt-port=0"}, 0, true},
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
			if got.doltPort != tc.want || got.doltPortSet != tc.wantSet {
				t.Errorf("--dolt-port parsed as %d (set %v), want %d (set %v)", got.doltPort, got.doltPortSet, tc.want, tc.wantSet)
			}
		})
	}
}

// The restart after the install passes --dolt-port again. The port the town
// already names goes on; a different one, or none, is refused rather than
// dropped in silence, as a different host is (gt-i2ekc).
func TestContainerEntrypointChecksTheDoltPortTheInstalledTownNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     string
		wantMsg string // "" means the start goes on
	}{
		{"the same port", `{"dolt":{"host":"scratch-dolt","port":3306}}`, ""},
		{"another port", `{"dolt":{"host":"scratch-dolt","port":3307}}`, "is not the port"},
		{"no port", `{"dolt":{"host":"scratch-dolt"}}`, "is not the port"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.kind[entrypointTownCfg] = entryRegular
			r.files = map[string]string{entrypointTownCfg: tc.cfg}
			r.doltHost, r.doltHostSet = "scratch-dolt", true
			r.doltPort, r.doltPortSet = 3306, true

			err := runContainerEntrypoint(r.entrypoint())

			if tc.wantMsg == "" {
				if err != nil {
					t.Fatalf("runContainerEntrypoint: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), config.DoltPortFlag) || !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("error = %v, want a refusal naming --%s and saying %q", err, config.DoltPortFlag, tc.wantMsg)
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

// A restart that leaves --dolt-port out is checked against the port an install
// would have used, gt's default 3307, so dropping the flag does not skip the
// check (om finding on gt-i2ekc).
func TestContainerEntrypointChecksTheDefaultDoltPortWhenTheFlagIsOmitted(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		cfg     string
		wantErr bool
	}{
		{"the town names the default port", `{"dolt":{"host":"scratch-dolt","port":3307}}`, false},
		{"the town names another port", `{"dolt":{"host":"scratch-dolt","port":3306}}`, true},
		{"the town names no port", `{"dolt":{"host":"scratch-dolt"}}`, true},
		{"the town names no dolt block", `{"type":"town"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.kind[entrypointTownCfg] = entryRegular
			r.files = map[string]string{entrypointTownCfg: tc.cfg}
			r.doltHost, r.doltHostSet = "scratch-dolt", true

			err := runContainerEntrypoint(r.entrypoint())

			if tc.wantErr != (err != nil) {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if tc.wantErr && len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

const (
	forgeTestHost  = "scratch-forgejo:3000"
	forgeTestToken = "/towns/scratch/forge-tokens/forgejo-landing.env"
)

// With both forge flags the container's git is told to ask the credential
// helper for the forge host, before the install, so the landing worker's push
// carries the bot's token. The config line carries the token file's path, never
// a token. Dropping the git config call turns this red (gt-2d6f2).
func TestContainerEntrypointWritesTheForgeCredentialHelper(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.forgeHost, r.forgeHostSet = forgeTestHost, true
	r.forgeTokenFile, r.forgeTokenFileSet = forgeTestToken, true
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	helper := "!" + entrypointBin + " " + config.GitCredentialForgejoCommand + " --host " + forgeTestHost + " --token-file " + forgeTestToken
	want := []string{"git", "config", "--global", "credential.http://" + forgeTestHost + ".helper", helper}
	at, install := -1, -1
	for i, c := range r.calls {
		if reflect.DeepEqual(c, want) {
			at = i
		}
		if len(c) > 1 && c[1] == "install" {
			install = i
		}
	}
	if at < 0 {
		t.Fatalf("commands = %v, want one running %v", r.calls, want)
	}
	if install >= 0 && at > install {
		t.Errorf("the credential helper was set at command %d, after the install at %d; the install's own git use needs it first", at, install)
	}
}

// Without the flags the entrypoint sets no credential helper at all.
func TestContainerEntrypointWithoutForgeFlagsSetsNoCredentialHelper(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), "credential") {
			t.Errorf("a command mentions a credential helper without the flags: %v", c)
		}
	}
}

// The live container takes the pair too, so the same flags are accepted
// without --scratch-town; a scratch-only requirement returning turns this red
// (gt-rpx80).
func TestContainerEntrypointAcceptsTheForgeCredentialWithoutTheScratchFlag(t *testing.T) {
	t.Parallel()
	r := townRig()
	r.forgeHost, r.forgeHostSet = forgeTestHost, true
	r.forgeTokenFile, r.forgeTokenFileSet = forgeTestToken, true

	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("error = %v, want the live container to accept the pair", err)
	}
	helper := "!" + entrypointBin + " " + config.GitCredentialForgejoCommand + " --host " + forgeTestHost + " --token-file " + forgeTestToken
	want := []string{"git", "config", "--global", "credential.http://" + forgeTestHost + ".helper", helper}
	if !slices.ContainsFunc(r.calls, func(c []string) bool { return reflect.DeepEqual(c, want) }) {
		t.Errorf("commands = %v, want one running %v", r.calls, want)
	}
	if r.execCalls() != 1 {
		t.Errorf("daemon execs = %d, want 1 after the helper was set", r.execCalls())
	}
}

// A flag no start could use is refused before the first command runs.
func TestContainerEntrypointRefusesAForgeCredentialItCannotUse(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		host      string
		hostSet   bool
		token     string
		tokenSet  bool
		wantInErr string
	}{
		{"a host without a token file", forgeTestHost, true, "", false, "together or not at all"},
		{"a token file without a host", "", false, forgeTestToken, true, "together or not at all"},
		{"an empty host", "", true, forgeTestToken, true, "not a bare host:port"},
		{"a host with a scheme", "http://scratch-forgejo:3000", true, forgeTestToken, true, "not a bare host:port"},
		{"a host with no port", "scratch-forgejo", true, forgeTestToken, true, "not a bare host:port"},
		{"a host beginning with a dash", "-evil:3000", true, forgeTestToken, true, "not a bare host:port"},
		{"a host with port zero", "scratch-forgejo:0", true, forgeTestToken, true, "not a port"},
		{"a host with a port above 65535", "scratch-forgejo:99999", true, forgeTestToken, true, "not a port"},
		{"a relative token file", forgeTestHost, true, "forge-tokens/forgejo-landing.env", true, "absolute path"},
		{"an empty token file", forgeTestHost, true, "", true, "absolute path"},
		{"a token file path with a shell metacharacter", forgeTestHost, true, "/towns/$(id)/forgejo-landing.env", true, "absolute path"},
		{"a token file path with a space", forgeTestHost, true, "/towns/my town/forgejo-landing.env", true, "absolute path"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := scratchRig()
			r.forgeHost, r.forgeHostSet = tc.host, tc.hostSet
			r.forgeTokenFile, r.forgeTokenFileSet = tc.token, tc.tokenSet

			err := runContainerEntrypoint(r.entrypoint())

			if err == nil || !strings.Contains(err.Error(), tc.wantInErr) {
				t.Fatalf("error = %v, want a refusal saying %q", err, tc.wantInErr)
			}
			if len(r.calls) != 0 {
				t.Errorf("a refusal ran %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

// The entrypoint parses both flags into the values it acts on.
func TestContainerEntrypointForgeFlagsAreWired(t *testing.T) {
	t.Parallel()
	var got entrypointFlags
	c := newDaemonContainerEntrypointCmd(func(flags entrypointFlags) error {
		got = flags
		return nil
	})
	c.SetArgs([]string{"--forge-host", forgeTestHost, "--forge-token-file", forgeTestToken})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute container-entrypoint: %v", err)
	}
	if got.forgeHost != forgeTestHost || !got.forgeHostSet || got.forgeTokenFile != forgeTestToken || !got.forgeTokenFileSet {
		t.Errorf("parsed %+v, want the host and token file with both marked set", got)
	}
}

// A helper command failing is an error, not a start without credentials.
func TestContainerEntrypointReportsAFailedCredentialHelperSetup(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.forgeHost, r.forgeHostSet = forgeTestHost, true
	r.forgeTokenFile, r.forgeTokenFileSet = forgeTestToken, true
	e := r.entrypoint()
	base := e.run
	e.run = func(argv []string) error {
		if len(argv) > 3 && argv[0] == "git" && strings.HasPrefix(argv[3], "credential.") {
			return errors.New("git: cannot write config")
		}
		return base(argv)
	}
	err := runContainerEntrypoint(e)
	if err == nil || !strings.Contains(err.Error(), "credential helper") {
		t.Errorf("error = %v, want one naming the credential helper", err)
	}
}

// The helper line is run by git through a shell, so a gt binary path with a
// space or a metacharacter is refused rather than quoted into the config.
func TestContainerEntrypointRefusesAGtPathAShellWouldRead(t *testing.T) {
	t.Parallel()
	r := scratchRig()
	r.forgeHost, r.forgeHostSet = forgeTestHost, true
	r.forgeTokenFile, r.forgeTokenFileSet = forgeTestToken, true
	e := r.entrypoint()
	e.self = func() (string, error) { return "/opt/my tools/gt", nil }

	err := runContainerEntrypoint(e)

	if err == nil || !strings.Contains(err.Error(), "path") || !strings.Contains(err.Error(), "shell") {
		t.Fatalf("error = %v, want a refusal naming the gt path and the shell", err)
	}
	for _, c := range r.calls {
		if strings.Contains(strings.Join(c, " "), "credential") {
			t.Errorf("a credential helper was set despite the refusal: %v", c)
		}
	}
}

// writeEntrypointFile writes one file under a fresh parent directory.
func writeEntrypointFile(t *testing.T, path string, data []byte, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, perm); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// pluginTownDir makes the town's plugin directory for name with the plugin.md
// the scanner loads, and returns the destination the image's binary takes.
func pluginTownDir(t *testing.T, townRoot, name string) string {
	t.Helper()
	writeEntrypointFile(t, filepath.Join(townRoot, "plugins", name, "plugin.md"),
		[]byte("+++\nname = \""+name+"\"\n+++\n"), 0o644)
	return filepath.Join(townRoot, "plugins", name, "bin", runtime.GOOS+"-"+runtime.GOARCH, name)
}

// The image's binary for a plugin the town carries lands in this platform's
// directory, executable, with the image's bytes.
func TestEntrypointPluginsInstallsIntoPerOSDir(t *testing.T) {
	t.Parallel()
	imageDir := t.TempDir()
	townRoot := t.TempDir()
	writeEntrypointFile(t, filepath.Join(imageDir, "app-promote", "app-promote"), []byte("linux binary"), 0o644)
	dest := pluginTownDir(t, townRoot, "app-promote")

	if err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {}); err != nil {
		t.Fatalf("installPluginBinaries: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "linux binary" {
		t.Errorf("installed binary = %q, want the image's bytes", got)
	}
	info, err := os.Stat(dest)
	if err != nil {
		t.Fatalf("stat installed binary: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o755 {
		t.Errorf("installed binary mode = %o, want 0755", perm)
	}
}

// An older per-platform binary is replaced in place, and the atomic write
// leaves no temporary file behind.
func TestEntrypointPluginsReplacesAnExistingBinary(t *testing.T) {
	t.Parallel()
	imageDir := t.TempDir()
	townRoot := t.TempDir()
	writeEntrypointFile(t, filepath.Join(imageDir, "tool-updater", "tool-updater"), []byte("new"), 0o644)
	dest := pluginTownDir(t, townRoot, "tool-updater")
	writeEntrypointFile(t, dest, []byte("old"), 0o755)

	if err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {}); err != nil {
		t.Fatalf("installPluginBinaries: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read replaced binary: %v", err)
	}
	if string(got) != "new" {
		t.Errorf("replaced binary = %q, want the image's bytes", got)
	}
	entries, err := os.ReadDir(filepath.Dir(dest))
	if err != nil {
		t.Fatalf("read the platform directory: %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"tool-updater"}) {
		t.Errorf("the platform directory holds %v, want only the installed binary", names)
	}
}

// The town's plugin set comes from the shared volume: a plugin the town carries
// no plugin.md for is skipped, logged once, and nothing is written for it.
func TestEntrypointPluginsSkipsAPluginTheTownLacks(t *testing.T) {
	t.Parallel()
	imageDir := t.TempDir()
	townRoot := t.TempDir()
	writeEntrypointFile(t, filepath.Join(imageDir, "intruder", "intruder"), []byte("linux binary"), 0o755)
	pluginTownDir(t, townRoot, "known")
	var logged []string
	logf := func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	if err := installPluginBinaries(imageDir, townRoot, logf); err != nil {
		t.Fatalf("installPluginBinaries: %v", err)
	}

	if _, err := os.Stat(filepath.Join(townRoot, "plugins", "intruder")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the plugin the town lacks was written: stat error = %v", err)
	}
	if len(logged) != 1 {
		t.Fatalf("logged %d lines, want exactly one: %v", len(logged), logged)
	}
	if !strings.Contains(logged[0], "intruder") {
		t.Errorf("log line %q does not name the skipped plugin", logged[0])
	}
}

// The image may carry no plugins at all, and so may have no plugin directory.
func TestEntrypointPluginsMissingSourceIsNotAnError(t *testing.T) {
	t.Parallel()
	if err := installPluginBinaries(filepath.Join(t.TempDir(), "absent"), t.TempDir(), func(string, ...any) {}); err != nil {
		t.Fatalf("installPluginBinaries with an absent source root: %v, want nil", err)
	}
}

// An image plugin directory with no regular binary of its own name is a broken
// image: the start refuses rather than leave the daemon to run the Mac binary
// the town volume also holds.
func TestEntrypointPluginsRefusesAnImagePluginWithoutABinary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, pluginDir string)
	}{
		{"no binary", func(*testing.T, string) {}},
		{"a directory named as the binary", func(t *testing.T, pluginDir string) {
			if err := os.MkdirAll(filepath.Join(pluginDir, "broken"), 0o755); err != nil {
				t.Fatalf("mkdir: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			imageDir := t.TempDir()
			townRoot := t.TempDir()
			pluginDir := filepath.Join(imageDir, "broken")
			if err := os.MkdirAll(pluginDir, 0o755); err != nil {
				t.Fatalf("mkdir the image's plugin dir: %v", err)
			}
			tc.write(t, pluginDir)
			pluginTownDir(t, townRoot, "broken")

			err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {})
			if err == nil || !strings.Contains(err.Error(), "broken") {
				t.Fatalf("error = %v, want a refusal naming the plugin", err)
			}
		})
	}
}

// The source root and each plugin directory are read as the directories they
// must be: a file in their place is a refusal, not an empty plugin set.
func TestEntrypointPluginsRefusesAnUnusableSource(t *testing.T) {
	t.Parallel()
	t.Run("the source root is a file", func(t *testing.T) {
		t.Parallel()
		imageDir := filepath.Join(t.TempDir(), "plugins")
		writeEntrypointFile(t, imageDir, []byte("not a directory"), 0o644)

		err := installPluginBinaries(imageDir, t.TempDir(), func(string, ...any) {})
		if err == nil || !strings.Contains(err.Error(), imageDir) {
			t.Fatalf("error = %v, want a refusal naming %s", err, imageDir)
		}
	})
	t.Run("the town's plugin path is a file", func(t *testing.T) {
		t.Parallel()
		imageDir := t.TempDir()
		writeEntrypointFile(t, filepath.Join(imageDir, "broken", "broken"), []byte("binary"), 0o755)
		townRoot := t.TempDir()
		writeEntrypointFile(t, filepath.Join(townRoot, "plugins", "broken"), []byte("not a directory"), 0o644)

		err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {})
		if err == nil || !strings.Contains(err.Error(), "plugin.md") {
			t.Fatalf("error = %v, want a refusal naming the town's plugin.md", err)
		}
	})
}

// A town plugin directory whose bin path cannot be a directory, or whose
// binary path is an existing directory, refuses the start.
func TestEntrypointPluginsRefusesADestinationItCannotWrite(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		write func(t *testing.T, townRoot string)
	}{
		{"a file where the platform directory belongs", func(t *testing.T, townRoot string) {
			writeEntrypointFile(t, filepath.Join(townRoot, "plugins", "broken", "bin"), []byte("not a directory"), 0o644)
		}},
		{"a directory where the binary belongs", func(t *testing.T, townRoot string) {
			if err := os.MkdirAll(pluginTownDir(t, townRoot, "broken"), 0o755); err != nil {
				t.Fatalf("mkdir the binary path: %v", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			imageDir := t.TempDir()
			writeEntrypointFile(t, filepath.Join(imageDir, "broken", "broken"), []byte("binary"), 0o755)
			townRoot := t.TempDir()
			pluginTownDir(t, townRoot, "broken")
			tc.write(t, townRoot)

			if err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {}); err == nil {
				t.Fatal("installPluginBinaries wrote a binary it cannot place, want a refusal")
			}
		})
	}
}

// A file in the image's plugin root is not a plugin directory, even when the
// town has a plugin of that name: it is skipped rather than read as a binary.
func TestEntrypointPluginsIgnoresAFileInTheSourceRoot(t *testing.T) {
	t.Parallel()
	imageDir := t.TempDir()
	townRoot := t.TempDir()
	writeEntrypointFile(t, filepath.Join(imageDir, "app-promote"), []byte("not a directory"), 0o644)
	pluginTownDir(t, townRoot, "app-promote")

	if err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {}); err != nil {
		t.Fatalf("installPluginBinaries: %v", err)
	}
	if _, err := os.Stat(filepath.Join(townRoot, "plugins", "app-promote", "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a binary was installed from a file: stat error = %v", err)
	}
}

// A plugin.md that is not a regular file is not one the scanner loads, so the
// image's binary is skipped and logged exactly once.
func TestEntrypointPluginsSkipsAPluginWhoseDefinitionIsNotAFile(t *testing.T) {
	t.Parallel()
	imageDir := t.TempDir()
	townRoot := t.TempDir()
	writeEntrypointFile(t, filepath.Join(imageDir, "broken", "broken"), []byte("linux binary"), 0o755)
	if err := os.MkdirAll(filepath.Join(townRoot, "plugins", "broken", "plugin.md"), 0o755); err != nil {
		t.Fatalf("mkdir the plugin.md directory: %v", err)
	}
	var logged []string
	logf := func(format string, args ...any) {
		logged = append(logged, fmt.Sprintf(format, args...))
	}

	if err := installPluginBinaries(imageDir, townRoot, logf); err != nil {
		t.Fatalf("installPluginBinaries: %v", err)
	}

	if _, err := os.Stat(filepath.Join(townRoot, "plugins", "broken", "bin")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a binary was installed for a plugin the scanner skips: stat error = %v", err)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "broken") {
		t.Errorf("logged %v, want one line naming the skipped plugin", logged)
	}
}

// A plugin directory the image links in is installed like a real one: the
// directory entry is a symlink, so the check must follow it.
func TestEntrypointPluginsFollowsASymlinkedPluginDirectory(t *testing.T) {
	t.Parallel()
	imageDir := t.TempDir()
	townRoot := t.TempDir()
	target := t.TempDir()
	writeEntrypointFile(t, filepath.Join(target, "app-promote", "app-promote"), []byte("linux binary"), 0o755)
	if err := os.Symlink(filepath.Join(target, "app-promote"), filepath.Join(imageDir, "app-promote")); err != nil {
		t.Fatalf("symlink the plugin directory: %v", err)
	}
	dest := pluginTownDir(t, townRoot, "app-promote")

	if err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {}); err != nil {
		t.Fatalf("installPluginBinaries: %v", err)
	}

	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("read installed binary: %v", err)
	}
	if string(got) != "linux binary" {
		t.Errorf("installed binary = %q, want the linked plugin's bytes", got)
	}
}

// A dangling link in the image is a broken image, not a plugin to skip: the
// start refuses and names the entry.
func TestEntrypointPluginsRefusesADanglingSymlink(t *testing.T) {
	t.Parallel()
	imageDir := t.TempDir()
	townRoot := t.TempDir()
	if err := os.Symlink(filepath.Join(t.TempDir(), "gone"), filepath.Join(imageDir, "app-promote")); err != nil {
		t.Fatalf("symlink the missing target: %v", err)
	}

	err := installPluginBinaries(imageDir, townRoot, func(string, ...any) {})
	if err == nil || !strings.Contains(err.Error(), "app-promote") {
		t.Fatalf("error = %v, want a refusal naming the dangling entry", err)
	}
}

// The plugin binaries are in place before the daemon starts: the install runs
// for the town root, immediately before the exec.
func TestEntrypointPluginsInstallBeforeTheDaemonExec(t *testing.T) {
	t.Parallel()
	r := townRig()
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	if !slices.Equal(r.pluginTownRoots, []string{entrypointTown}) {
		t.Errorf("plugin installs ran for %v, want the town root once", r.pluginTownRoots)
	}
	if want := len(r.calls) - 1; r.pluginRanAt != want {
		t.Errorf("the plugin install ran after %d commands, want %d (just before the daemon exec)", r.pluginRanAt, want)
	}
}

// A failed plugin copy refuses and the daemon is never exec'd.
func TestEntrypointPluginsFailureStopsBeforeTheDaemonExec(t *testing.T) {
	t.Parallel()
	r := townRig()
	r.pluginErr = errors.New("copy failed")
	err := runContainerEntrypoint(r.entrypoint())
	if err == nil || !strings.Contains(err.Error(), "plugin") {
		t.Fatalf("error = %v, want a refusal naming the plugin install", err)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("the daemon was exec'd %d times after a failed plugin install, want none", n)
	}
}

// townMangoFile is the path a file of the image's mango directory takes under
// the town's runtime directory for this platform, where the metrics analysis
// reads it.
func townMangoFile(townRoot, name string) string {
	return filepath.Join(townRoot, ".runtime", "metrics", "bin", runtime.GOOS+"-"+runtime.GOARCH, name)
}

// mangoImage makes an image directory holding the mango binary and its commit
// stamp.
func mangoImage(t *testing.T, binary, stamp string) string {
	t.Helper()
	imageDir := t.TempDir()
	writeEntrypointFile(t, filepath.Join(imageDir, mangoBinName), []byte(binary), 0o644)
	writeEntrypointFile(t, filepath.Join(imageDir, mangoShaName), []byte(stamp), 0o644)
	return imageDir
}

func TestInstallMangoBinary(t *testing.T) {
	t.Parallel()

	t.Run("installs the binary and its stamp", func(t *testing.T) {
		t.Parallel()
		imageDir := mangoImage(t, "linux mango", "abc123\n")
		townRoot := t.TempDir()

		if err := installMangoBinary(imageDir, townRoot); err != nil {
			t.Fatalf("installMangoBinary: %v", err)
		}

		bin := townMangoFile(townRoot, mangoBinName)
		got, err := os.ReadFile(bin)
		if err != nil {
			t.Fatalf("read the installed binary: %v", err)
		}
		if string(got) != "linux mango" {
			t.Errorf("installed binary = %q, want the image's bytes", got)
		}
		info, err := os.Stat(bin)
		if err != nil {
			t.Fatalf("stat the installed binary: %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o755 {
			t.Errorf("installed binary mode = %o, want 0755", perm)
		}
		stamp, err := os.ReadFile(townMangoFile(townRoot, mangoShaName))
		if err != nil {
			t.Fatalf("read the installed stamp: %v", err)
		}
		if string(stamp) != "abc123\n" {
			t.Errorf("installed stamp = %q, want the image's bytes", stamp)
		}
	})

	t.Run("replaces a previous install", func(t *testing.T) {
		t.Parallel()
		imageDir := mangoImage(t, "new mango", "new123\n")
		townRoot := t.TempDir()
		bin := townMangoFile(townRoot, mangoBinName)
		writeEntrypointFile(t, bin, []byte("old mango"), 0o755)
		writeEntrypointFile(t, townMangoFile(townRoot, mangoShaName), []byte("old123\n"), 0o644)

		if err := installMangoBinary(imageDir, townRoot); err != nil {
			t.Fatalf("installMangoBinary: %v", err)
		}

		got, err := os.ReadFile(bin)
		if err != nil {
			t.Fatalf("read the replaced binary: %v", err)
		}
		if string(got) != "new mango" {
			t.Errorf("replaced binary = %q, want the image's bytes", got)
		}
		stamp, err := os.ReadFile(townMangoFile(townRoot, mangoShaName))
		if err != nil {
			t.Fatalf("read the replaced stamp: %v", err)
		}
		if string(stamp) != "new123\n" {
			t.Errorf("replaced stamp = %q, want the image's bytes", stamp)
		}
		entries, err := os.ReadDir(filepath.Dir(bin))
		if err != nil {
			t.Fatalf("read the platform directory: %v", err)
		}
		var names []string
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		if !slices.Equal(names, []string{mangoBinName, mangoShaName}) {
			t.Errorf("the platform directory holds %v, want only the binary and its stamp", names)
		}
	})

	t.Run("puts the installed binary back when its stamp cannot be written", func(t *testing.T) {
		t.Parallel()
		imageDir := mangoImage(t, "new mango", "new123\n")
		townRoot := t.TempDir()
		bin := townMangoFile(townRoot, mangoBinName)
		writeEntrypointFile(t, bin, []byte("old mango"), 0o755)
		// A directory where the stamp belongs refuses the second write after the
		// binary has already been replaced.
		stampPath := townMangoFile(townRoot, mangoShaName)
		if err := os.MkdirAll(stampPath, 0o755); err != nil {
			t.Fatalf("mkdir the stamp path: %v", err)
		}

		err := installMangoBinary(imageDir, townRoot)
		if err == nil || !strings.Contains(err.Error(), stampPath) {
			t.Fatalf("error = %v, want a refusal naming the stamp path", err)
		}
		got, readErr := os.ReadFile(bin)
		if readErr != nil {
			t.Fatalf("read the installed binary: %v", readErr)
		}
		if string(got) != "old mango" {
			t.Errorf("installed binary = %q, want the binary the town had before the failed install", got)
		}
	})

	t.Run("leaves no binary when its stamp cannot be written", func(t *testing.T) {
		t.Parallel()
		imageDir := mangoImage(t, "new mango", "new123\n")
		townRoot := t.TempDir()
		stampPath := townMangoFile(townRoot, mangoShaName)
		if err := os.MkdirAll(stampPath, 0o755); err != nil {
			t.Fatalf("mkdir the stamp path: %v", err)
		}

		if err := installMangoBinary(imageDir, townRoot); err == nil {
			t.Fatal("installMangoBinary installed a binary whose stamp it could not write, want a refusal")
		}
		if _, err := os.Stat(townMangoFile(townRoot, mangoBinName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("a failed install left the image's binary behind: stat error = %v", err)
		}
	})

	t.Run("refuses an installed binary it cannot read", func(t *testing.T) {
		t.Parallel()
		imageDir := mangoImage(t, "new mango", "new123\n")
		townRoot := t.TempDir()
		bin := townMangoFile(townRoot, mangoBinName)
		writeEntrypointFile(t, bin, []byte("old mango"), 0o755)
		if err := os.Chmod(bin, 0o000); err != nil {
			t.Fatalf("chmod the installed binary: %v", err)
		}

		err := installMangoBinary(imageDir, townRoot)
		if err == nil || !strings.Contains(err.Error(), bin) {
			t.Fatalf("error = %v, want a refusal naming the installed binary", err)
		}
	})

	t.Run("refuses an installed stamp it cannot read", func(t *testing.T) {
		t.Parallel()
		imageDir := mangoImage(t, "new mango", "new123\n")
		townRoot := t.TempDir()
		stampPath := townMangoFile(townRoot, mangoShaName)
		writeEntrypointFile(t, stampPath, []byte("old123\n"), 0o644)
		if err := os.Chmod(stampPath, 0o000); err != nil {
			t.Fatalf("chmod the installed stamp: %v", err)
		}

		err := installMangoBinary(imageDir, townRoot)
		if err == nil || !strings.Contains(err.Error(), stampPath) {
			t.Fatalf("error = %v, want a refusal naming the installed stamp", err)
		}
		if _, statErr := os.Stat(townMangoFile(townRoot, mangoBinName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("an install that could not read the stamp wrote a binary: stat error = %v", statErr)
		}
	})

	t.Run("reports the previous binary it cannot put back", func(t *testing.T) {
		t.Parallel()
		imageDir := mangoImage(t, "new mango", "new123\n")
		townRoot := t.TempDir()
		bin := townMangoFile(townRoot, mangoBinName)
		writeEntrypointFile(t, bin, []byte("old mango"), 0o644)
		dir := filepath.Dir(bin)
		//testpolicy:allow no-exec-files — the target is the platform directory, and its search bit must stay to reach the binary inside: the write bit is what the test drops
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("chmod the platform directory: %v", err)
		}
		t.Cleanup(func() {
			//testpolicy:allow no-exec-files — the same directory, and traversal needs its search bit back so the test's cleanup can walk it
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Errorf("restore the platform directory mode: %v", err)
			}
		})

		err := installMangoBinary(imageDir, townRoot)
		if err == nil || !strings.Contains(err.Error(), bin) || !strings.Contains(err.Error(), "put the installed mango back") {
			t.Fatalf("error = %v, want the failed install and the failed restore", err)
		}
	})

	t.Run("an absent image directory is not an error", func(t *testing.T) {
		t.Parallel()
		townRoot := t.TempDir()
		if err := installMangoBinary(filepath.Join(t.TempDir(), "absent"), townRoot); err != nil {
			t.Fatalf("installMangoBinary with an absent image directory: %v, want nil", err)
		}
		if _, err := os.Stat(townMangoFile(townRoot, mangoBinName)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("an older image installed a binary: stat error = %v", err)
		}
	})

	t.Run("refuses an image directory without mango", func(t *testing.T) {
		t.Parallel()
		imageDir := t.TempDir()
		writeEntrypointFile(t, filepath.Join(imageDir, mangoShaName), []byte("abc123\n"), 0o644)
		townRoot := t.TempDir()

		err := installMangoBinary(imageDir, townRoot)
		if err == nil || !strings.Contains(err.Error(), filepath.Join(imageDir, mangoBinName)) {
			t.Fatalf("error = %v, want a refusal naming the missing mango", err)
		}
		if _, statErr := os.Stat(townMangoFile(townRoot, mangoBinName)); !errors.Is(statErr, os.ErrNotExist) {
			t.Errorf("a broken image wrote a binary: stat error = %v", statErr)
		}
	})

	t.Run("refuses an image directory without its stamp", func(t *testing.T) {
		t.Parallel()
		imageDir := t.TempDir()
		writeEntrypointFile(t, filepath.Join(imageDir, mangoBinName), []byte("linux mango"), 0o755)

		err := installMangoBinary(imageDir, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), filepath.Join(imageDir, mangoShaName)) {
			t.Fatalf("error = %v, want a refusal naming the missing stamp", err)
		}
	})

	t.Run("refuses a destination it cannot write", func(t *testing.T) {
		t.Parallel()
		for _, tc := range []struct {
			name  string
			write func(t *testing.T, townRoot string)
		}{
			{"a file where the platform directory belongs", func(t *testing.T, townRoot string) {
				writeEntrypointFile(t, filepath.Dir(townMangoFile(townRoot, mangoBinName)), []byte("not a directory"), 0o644)
			}},
			{"a directory where the binary belongs", func(t *testing.T, townRoot string) {
				if err := os.MkdirAll(townMangoFile(townRoot, mangoBinName), 0o755); err != nil {
					t.Fatalf("mkdir the binary path: %v", err)
				}
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				t.Parallel()
				imageDir := mangoImage(t, "linux mango", "abc123\n")
				townRoot := t.TempDir()
				tc.write(t, townRoot)

				if err := installMangoBinary(imageDir, townRoot); err == nil {
					t.Fatal("installMangoBinary wrote an install it cannot place, want a refusal")
				}
			})
		}
	})

	t.Run("refuses an image path it cannot read", func(t *testing.T) {
		t.Parallel()
		// A regular file in the middle of the path makes stat fail with ENOTDIR,
		// which is neither an absent image nor a broken file inside one.
		parent := t.TempDir()
		writeEntrypointFile(t, filepath.Join(parent, "mango"), []byte("not a directory"), 0o644)
		imageDir := filepath.Join(parent, "mango", "inner")

		err := installMangoBinary(imageDir, t.TempDir())
		if err == nil || !strings.Contains(err.Error(), imageDir) {
			t.Fatalf("error = %v, want a refusal naming %s", err, imageDir)
		}
	})
}

// TestInstalledMango drives the read and restore failures whose paths a town
// directory the install can write cannot produce.
func TestInstalledMango(t *testing.T) {
	t.Parallel()

	t.Run("read reports a path under a file", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		writeEntrypointFile(t, filepath.Join(parent, "mango"), []byte("not a directory"), 0o644)
		path := filepath.Join(parent, "mango", "outer", "mango")

		if _, err := readInstalledMango(path); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("readInstalledMango(%s) error = %v, want a refusal naming the path", path, err)
		}
	})

	t.Run("putBack reports a path it cannot read", func(t *testing.T) {
		t.Parallel()
		parent := t.TempDir()
		writeEntrypointFile(t, filepath.Join(parent, "mango"), []byte("not a directory"), 0o644)
		path := filepath.Join(parent, "mango", "outer", "mango")

		if err := (installedMango{path: path}).putBack(); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("putBack(%s) error = %v, want a refusal naming the path", path, err)
		}
	})

	t.Run("putBack reports a file it cannot remove", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		path := filepath.Join(dir, mangoBinName)
		writeEntrypointFile(t, path, []byte("the image's binary"), 0o755)
		//testpolicy:allow no-exec-files — the target is the directory, and its search bit must stay to reach the file inside: the write bit is what the test drops
		if err := os.Chmod(dir, 0o555); err != nil {
			t.Fatalf("chmod the directory: %v", err)
		}
		t.Cleanup(func() {
			//testpolicy:allow no-exec-files — the same directory, and traversal needs its search bit back so the test's cleanup can walk it
			if err := os.Chmod(dir, 0o755); err != nil {
				t.Errorf("restore the directory mode: %v", err)
			}
		})

		if err := (installedMango{path: path}).putBack(); err == nil || !strings.Contains(err.Error(), path) {
			t.Fatalf("putBack(%s) error = %v, want a refusal naming the path", path, err)
		}
	})
}

// The mango install is in place before the daemon starts: it runs for the town
// root, after the plugin binaries and immediately before the exec.
func TestEntrypointMangoInstallsAfterThePluginsBeforeTheDaemonExec(t *testing.T) {
	t.Parallel()
	r := townRig()
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	if !slices.Equal(r.installs, []string{"plugins", "mango"}) {
		t.Errorf("installers ran in order %v, want the plugins and then mango", r.installs)
	}
	if !slices.Equal(r.mangoTownRoots, []string{entrypointTown}) {
		t.Errorf("mango installs ran for %v, want the town root once", r.mangoTownRoots)
	}
	if n := r.execCalls(); n != 1 {
		t.Errorf("recorded %d daemon exec argv, want one after the installs", n)
	}
}

// A failed mango copy refuses and the daemon is never exec'd.
func TestEntrypointMangoFailureStopsBeforeTheDaemonExec(t *testing.T) {
	t.Parallel()
	r := townRig()
	r.mangoErr = errors.New("copy failed")
	err := runContainerEntrypoint(r.entrypoint())
	if err == nil || !strings.Contains(err.Error(), "mango") {
		t.Fatalf("error = %v, want a refusal naming the mango install", err)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("the daemon was exec'd %d times after a failed mango install, want none", n)
	}
}

// The central directives deploy from the gastown bare repo after the mango
// install and before the daemon is exec'd: the call order is plugins, mango,
// directives, and the exec is the next command run after the deploy.
func TestEntrypointDirectivesDeployAfterTheMangoBeforeTheDaemonExec(t *testing.T) {
	t.Parallel()
	r := townRig()
	r.exists[config.CentralDirectiveRepo(entrypointTown)] = true
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	if !slices.Equal(r.installs, []string{"plugins", "mango", "directives"}) {
		t.Errorf("installers ran in order %v, want plugins, mango and then directives", r.installs)
	}
	if !slices.Equal(r.deployTownRoots, []string{entrypointTown}) {
		t.Errorf("directive deploys ran for %v, want the town root once", r.deployTownRoots)
	}
	if r.deployRanAt >= len(r.calls) || len(r.calls[r.deployRanAt]) < 2 || r.calls[r.deployRanAt][1] != "daemon" {
		t.Errorf("the command after the directive deploy is not the daemon exec; calls from %d: %v", r.deployRanAt, r.calls[r.deployRanAt:])
	}
	if n := r.execCalls(); n != 1 {
		t.Errorf("recorded %d daemon exec argv, want one", n)
	}
}

// A commit the bare repo does not hold yet is a warning that names it, and the
// daemon still starts. Any other deploy error refuses before the exec.
func TestEntrypointDirectivesDeployFailurePolicy(t *testing.T) {
	t.Parallel()
	const commit = "2222222"
	tests := []struct {
		name      string
		deployErr error
		wantErr   bool
		wantExec  int
		wantWarn  bool
	}{
		{
			name:      "a commit the repo lacks warns and the daemon starts",
			deployErr: fmt.Errorf("central directives: commit %s is not in the repo: %w", commit, config.ErrDirectiveCommitMissing),
			wantExec:  1,
			wantWarn:  true,
		},
		{
			name:      "a write failure refuses before the exec",
			deployErr: errors.New("central directives: writing directives/a.md: permission denied"),
			wantErr:   true,
			wantExec:  0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := townRig()
			r.exists[config.CentralDirectiveRepo(entrypointTown)] = true
			r.deployErr = tc.deployErr
			err := runContainerEntrypoint(r.entrypoint())
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "the daemon was not started") {
					t.Errorf("error = %v, want a refusal ending in the daemon was not started", err)
				}
			} else if err != nil {
				t.Errorf("runContainerEntrypoint: %v, want nil", err)
			}
			if n := r.execCalls(); n != tc.wantExec {
				t.Errorf("recorded %d daemon exec argv, want %d", n, tc.wantExec)
			}
			warned := 0
			for _, line := range r.logs {
				if strings.Contains(line, "warning") && strings.Contains(line, commit) {
					warned++
				}
			}
			if tc.wantWarn && warned != 1 {
				t.Errorf("logs = %q, want one warning naming commit %s", r.logs, commit)
			}
			if !tc.wantWarn && warned != 0 {
				t.Errorf("logs = %q, want no commit warning", r.logs)
			}
		})
	}
}

// The real deployDirectives value deploys at the empty commit for the town root
// and returns the deploy's error unchanged: the entrypoint, not the seam, decides
// what a failure means.
func TestDeployImageDirectivesReturnsTheDeployError(t *testing.T) {
	t.Parallel()
	deployErr := errors.New("central directives: writing directives/a.md: permission denied")
	var gotTown, gotCommit string
	deploy := func(townRoot, commit string, buildCommit func() string) ([]string, error) {
		gotTown, gotCommit = townRoot, commit
		return nil, deployErr
	}
	err := deployImageDirectives(deploy, entrypointTown)
	if !errors.Is(err, deployErr) {
		t.Errorf("deployImageDirectives = %v, want the deploy error", err)
	}
	if gotTown != entrypointTown || gotCommit != "" {
		t.Errorf("deploy ran for town %q at commit %q, want town %q at the empty commit", gotTown, gotCommit, entrypointTown)
	}
}

// A repo that cannot be checked refuses: the entrypoint does not guess that the
// repo is absent, and the deploy does not run.
func TestEntrypointDirectivesRefuseAnUnreadableRepo(t *testing.T) {
	t.Parallel()
	r := townRig()
	r.brokenPath = config.CentralDirectiveRepo(entrypointTown)
	err := runContainerEntrypoint(r.entrypoint())
	if err == nil || !strings.Contains(err.Error(), "the daemon was not started") {
		t.Fatalf("error = %v, want a refusal ending in the daemon was not started", err)
	}
	if len(r.deployTownRoots) != 0 {
		t.Errorf("directive deploy ran for %v, want none", r.deployTownRoots)
	}
	if n := r.execCalls(); n != 0 {
		t.Errorf("the daemon was exec'd %d times after an unreadable repo, want none", n)
	}
}

// A town with no gastown rig skips the deploy with one log line and starts.
func TestEntrypointDirectivesSkipATownWithNoGastownRig(t *testing.T) {
	t.Parallel()
	r := townRig()
	if err := runContainerEntrypoint(r.entrypoint()); err != nil {
		t.Fatalf("runContainerEntrypoint: %v", err)
	}
	if len(r.deployTownRoots) != 0 {
		t.Errorf("directive deploy ran for %v, want none without a gastown rig", r.deployTownRoots)
	}
	if len(r.logs) != 1 {
		t.Errorf("logs = %q, want exactly one skip line", r.logs)
	}
	if n := r.execCalls(); n != 1 {
		t.Errorf("recorded %d daemon exec argv, want one", n)
	}
}
