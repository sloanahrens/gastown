package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
)

// The environment the entrypoint reads beyond the container-mode flag.
// GT_TOWN_ROOT is the bind mount the compose file names (templates
// daemon_compose.go); GIT_USER and GIT_EMAIL are the commit identity.
const (
	containerTownRootEnv = "GT_TOWN_ROOT"
	containerGitUserEnv  = "GIT_USER"
	containerGitEmailEnv = "GIT_EMAIL"

	// containerMountinfo is the table of mounts this process sees.
	containerMountinfo = "/proc/self/mountinfo"
)

var daemonContainerEntrypointCmd = &cobra.Command{
	Use:   "container-entrypoint",
	Short: "Start the daemon as the container's entrypoint (internal)",
	Long: `Start the Gas Town daemon as the container's entrypoint.

Runs only in container mode (GT_DAEMON_SUPERVISOR=container). It starts the
daemon on the town at GT_TOWN_ROOT and refuses everything else, so a missing
or wrong bind mount never turns into a fresh town:

  - mayor/town.json present: exec 'gt daemon run'.
  - mayor/town.json absent, GT_SCRATCH_TOWN unset: refuse.
  - mayor/town.json absent, GT_SCRATCH_TOWN=1: run 'gt install <townRoot> --git'
    once, only when the town root is a mount point, holds the scratch marker
    file, and holds nothing else; then exec 'gt daemon run'. Otherwise refuse.

Every check runs before the first command, so a refusal runs nothing.`,
	Hidden: true,
	Args:   cobra.NoArgs,
	RunE:   runDaemonContainerEntrypoint,
}

// containerEntrypoint is what the entrypoint reads and runs. Every command it
// runs goes through run or exec, so a test records the whole sequence and no
// process starts. runDaemonContainerEntrypoint wires the real ones.
type containerEntrypoint struct {
	self      string                    // the gt binary, argv[0] of each command it runs
	getenv    func(key string) string   // the environment
	run       func(argv []string) error // runs a command and waits for it
	exec      func(argv []string) error // replaces this process; returns only on error
	mountinfo func() (string, error)    // text of /proc/self/mountinfo
	dirNames  func(dir string) ([]string, error)
	exists    func(path string) (bool, error)
}

func runDaemonContainerEntrypoint(cmd *cobra.Command, args []string) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the gt binary to exec: %w", err)
	}
	return runContainerEntrypoint(containerEntrypoint{
		self:   self,
		getenv: os.Getenv,
		run:    runEntrypointCommand,
		exec:   execEntrypointArgv,
		mountinfo: func() (string, error) {
			b, err := os.ReadFile(containerMountinfo)
			return string(b), err
		},
		dirNames: func(dir string) ([]string, error) {
			entries, err := os.ReadDir(dir)
			if err != nil {
				return nil, err
			}
			names := make([]string, 0, len(entries))
			for _, entry := range entries {
				names = append(names, entry.Name())
			}
			return names, nil
		},
		exists: func(path string) (bool, error) {
			_, err := os.Stat(path)
			if err == nil {
				return true, nil
			}
			if errors.Is(err, os.ErrNotExist) {
				return false, nil
			}
			return false, err
		},
	})
}

// runContainerEntrypoint starts the daemon, or refuses. It decides first, with
// no command run, then sets the git identity, installs a scratch town if the
// decision says so, and execs the daemon last.
func runContainerEntrypoint(e containerEntrypoint) error {
	if e.getenv(config.ContainerModeEnv) != config.SupervisorContainer {
		return fmt.Errorf("gt daemon container-entrypoint runs only in container mode (%s=%s); nothing was started",
			config.ContainerModeEnv, config.SupervisorContainer)
	}
	townRoot := filepath.Clean(e.getenv(containerTownRootEnv))
	if !filepath.IsAbs(townRoot) {
		return fmt.Errorf("%s must name the town root as an absolute path; nothing was started", containerTownRootEnv)
	}
	install, err := e.decideStart(townRoot)
	if err != nil {
		return err
	}
	if err := e.setGitIdentity(); err != nil {
		return err
	}
	if install {
		if err := e.run([]string{e.self, "install", townRoot, "--git"}); err != nil {
			return fmt.Errorf("install the scratch town at %s: %w", townRoot, err)
		}
	}
	if err := e.exec([]string{e.self, "daemon", "run"}); err != nil {
		return fmt.Errorf("exec gt daemon run: %w", err)
	}
	return nil
}

// decideStart says whether this start installs the town first, or returns the
// refusal. It runs no command, so every refusal records nothing.
func (e containerEntrypoint) decideStart(townRoot string) (bool, error) {
	hasTown, err := e.exists(filepath.Join(townRoot, "mayor", "town.json"))
	if err != nil {
		return false, fmt.Errorf("read mayor/town.json under %s: %w; nothing was started", townRoot, err)
	}
	if hasTown {
		return false, nil
	}
	if e.getenv(config.ScratchTownEnv) != "1" {
		return false, fmt.Errorf("%s has no mayor/town.json, so the bind mount is missing or the path is wrong; a town is not installed here, and nothing was started", townRoot)
	}

	mountinfo, err := e.mountinfo()
	if err != nil {
		return false, fmt.Errorf("read %s: %w; nothing was started", containerMountinfo, err)
	}
	if !isMountPoint(mountinfo, townRoot) {
		return false, fmt.Errorf("%s is not a mount point, so it is not the scratch town's bind mount; nothing was started", townRoot)
	}

	hasMarker, err := e.exists(filepath.Join(townRoot, config.ScratchTownMarker))
	if err != nil {
		return false, fmt.Errorf("read the %s marker under %s: %w; nothing was started", config.ScratchTownMarker, townRoot, err)
	}
	if !hasMarker {
		return false, fmt.Errorf("%s holds no %s marker, which the runbook creates in the scratch town; nothing was started", townRoot, config.ScratchTownMarker)
	}

	names, err := e.dirNames(townRoot)
	if err != nil {
		return false, fmt.Errorf("list %s: %w; nothing was started", townRoot, err)
	}
	for _, name := range names {
		if name != config.ScratchTownMarker {
			return false, fmt.Errorf("%s holds files besides %s, so it is not an empty scratch town; nothing was started", townRoot, config.ScratchTownMarker)
		}
	}
	return true, nil
}

// setGitIdentity sets the commit identity the daemon's git work uses. An empty
// variable leaves git's own setting as it is.
func (e containerEntrypoint) setGitIdentity() error {
	for _, id := range []struct{ key, env string }{
		{"user.name", containerGitUserEnv},
		{"user.email", containerGitEmailEnv},
	} {
		value := e.getenv(id.env)
		if value == "" {
			continue
		}
		if err := e.run([]string{"git", "config", "--global", id.key, value}); err != nil {
			return fmt.Errorf("set git %s from %s: %w", id.key, id.env, err)
		}
	}
	return nil
}

// runEntrypointCommand runs one command and waits for it. Its output goes to
// this process's streams and never into an error, since output can hold a secret.
func runEntrypointCommand(argv []string) error {
	c := exec.Command(argv[0], argv[1:]...) //nolint:gosec // argv is built in this file from fixed names and the bind-mount path
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// isMountPoint reports whether path is a mount point in the text of
// /proc/self/mountinfo. The mount point is field 5 of each line, with space,
// tab, newline and backslash written as octal escapes. A parent of a mount
// point is a mount point only if the table lists it.
func isMountPoint(mountinfo string, path string) bool {
	if !filepath.IsAbs(path) {
		return false
	}
	want := filepath.Clean(path)
	for _, line := range strings.Split(mountinfo, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescapeMountPoint(fields[4]) == want {
			return true
		}
	}
	return false
}

// unescapeMountPoint decodes the \ooo octal escapes mountinfo writes into a
// mount point. A backslash that starts no valid escape is kept as it is.
func unescapeMountPoint(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
