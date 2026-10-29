package cmd

import (
	"errors"
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
	calls      [][]string
}

// scratchRig is a container whose town root is the scratch town: every
// condition for an install holds, and GT_SCRATCH_TOWN=1 asks for one.
func scratchRig() *entrypointRig {
	return &entrypointRig{
		env: map[string]string{
			config.ContainerModeEnv: config.SupervisorContainer,
			containerTownRootEnv:    entrypointTown,
			config.ScratchTownEnv:   "1",
			containerGitUserEnv:     "daemon",
			containerGitEmailEnv:    "daemon@example.invalid",
		},
		mountinfo: entrypointGtMount,
		exists:    map[string]bool{entrypointMarker: true},
		dir:       []string{config.ScratchTownMarker},
	}
}

// townRig is a container whose town root already holds mayor/town.json.
func townRig() *entrypointRig {
	r := scratchRig()
	delete(r.env, config.ScratchTownEnv)
	r.exists[entrypointTownCfg] = true
	return r
}

func (r *entrypointRig) entrypoint() containerEntrypoint {
	record := func(argv []string) error {
		r.calls = append(r.calls, append([]string{}, argv...))
		return nil
	}
	return containerEntrypoint{
		self:      entrypointBin,
		getenv:    func(key string) string { return r.env[key] },
		run:       record,
		exec:      record,
		mountinfo: func() (string, error) { return r.mountinfo, r.mountErr },
		dirNames:  func(string) ([]string, error) { return r.dir, nil },
		exists: func(path string) (bool, error) {
			if path == r.brokenPath {
				return false, errors.New("stat failed")
			}
			return r.exists[path], nil
		},
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

func TestContainerEntrypointRefusesOutsideContainerMode(t *testing.T) {
	for _, mode := range []string{"", "launchd"} {
		t.Run("mode="+mode, func(t *testing.T) {
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

func TestContainerEntrypointTownPresentExecsDaemonWithoutInstall(t *testing.T) {
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
	for _, flag := range []string{"", "0"} {
		t.Run("GT_SCRATCH_TOWN="+flag, func(t *testing.T) {
			// Every scratch condition holds, so the refusal is the only thing
			// that stops an install.
			r := scratchRig()
			r.env[config.ScratchTownEnv] = flag
			if err := runContainerEntrypoint(r.entrypoint()); err == nil {
				t.Fatal("expected a refusal with no mayor/town.json and no scratch flag")
			}
			if len(r.calls) != 0 {
				t.Errorf("recorded %d commands, want none: %v", len(r.calls), r.calls)
			}
		})
	}
}

func TestContainerEntrypointScratchTownRefusesWhenAConditionFails(t *testing.T) {
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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
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

func TestContainerEntrypointRunErrorStopsBeforeDaemonExec(t *testing.T) {
	r := scratchRig()
	e := r.entrypoint()
	e.run = func(argv []string) error {
		r.calls = append(r.calls, append([]string{}, argv...))
		return errors.New("install failed")
	}
	if err := runContainerEntrypoint(e); err == nil {
		t.Fatal("expected the install failure to surface")
	}
	for _, argv := range r.calls {
		if len(argv) > 1 && argv[1] == "daemon" {
			t.Fatalf("daemon exec ran after a failed install: %v", r.calls)
		}
	}
}

func TestIsMountPoint(t *testing.T) {
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
			if got := isMountPoint(table, tc.path); got != tc.want {
				t.Errorf("isMountPoint(%q) = %v, want %v", tc.path, got, tc.want)
			}
		})
	}
}
