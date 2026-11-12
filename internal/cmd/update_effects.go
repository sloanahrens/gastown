package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/polecat"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/version"
)

// seatProbeEnv is where the seat probe reads the town: the rigs, each rig's
// polecats, the prefix that names a polecat's session, and that session's
// heartbeat. The update command supplies the real readings and tests supply
// fakes.
type seatProbeEnv struct {
	// Rigs lists the town's rig names, the way `gt polecat list --all` finds
	// them.
	Rigs func() ([]string, error)
	// Polecats lists the directory names under <town>/<rig>/polecats.
	Polecats func(rig string) ([]string, error)
	// Prefix is townRegistry().PrefixForRig.
	Prefix func(rig string) string
	// Heartbeat reads a session's heartbeat: (nil, nil) when the session wrote
	// none, and an error when one exists but cannot be read or parsed.
	// polecat.ReadSessionHeartbeat answers nil for both, so it cannot be this
	// reading: a drain that took an unreadable file for no file would install
	// over a working polecat.
	Heartbeat func(session string) (*polecat.SessionHeartbeat, error)
}

// newSeatProbe returns the probe update.Drain watches. It reads each polecat's
// heartbeat from the files the daemon's workingPolecats reads: Working is the
// agent-reported state, and LastActive is when the heartbeat was last
// written, so the drain applies its own freshness bound to the same reading.
// A rig whose polecats directory is absent has no polecats; every other read
// error is returned, so the drain never reads a town it could not list as
// drained.
func newSeatProbe(env seatProbeEnv) update.SeatProbe {
	return seatProbeFunc(func(ctx context.Context) ([]update.Seat, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rigs, err := env.Rigs()
		if err != nil {
			return nil, fmt.Errorf("listing the town's rigs: %w", err)
		}
		var seats []update.Seat
		for _, rig := range rigs {
			names, err := env.Polecats(rig)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("listing rig %s's polecats: %w", rig, err)
			}
			for _, name := range names {
				seat, err := seatOf(env, rig, name)
				if err != nil {
					return nil, fmt.Errorf("reading %s/%s's heartbeat: %w", rig, name, err)
				}
				seats = append(seats, seat)
			}
		}
		return seats, nil
	})
}

// seatOf reads one polecat's seat. A session that wrote no heartbeat is not
// working with a zero LastActive; the drain's freshness bound then decides.
// A heartbeat that exists but cannot be read is an error — never "idle".
func seatOf(env seatProbeEnv, rig, name string) (update.Seat, error) {
	seat := update.Seat{Name: rig + "/" + name}
	hb, err := env.Heartbeat(session.PolecatSessionName(env.Prefix(rig), name))
	if err != nil {
		return update.Seat{}, err
	}
	if hb == nil {
		return seat, nil
	}
	seat.Working = hb.EffectiveState() == polecat.HeartbeatWorking
	seat.LastActive = hb.Timestamp
	return seat, nil
}

// polecatDirs lists the polecat worktree directory names under dir, skipping
// files and dot entries as `gt polecat list` does. A symlinked worktree is a
// worktree and is followed, so a town that links a polecat's directory in is
// still counted. A symlink that leads nowhere cannot be a seat, and an error
// reading it (unlike a plain file, which is not a seat either) is returned: a
// drain may not read a directory it could not check as empty. A missing dir
// keeps its fs.ErrNotExist, which newSeatProbe reads as a rig with no
// polecats.
func polecatDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		isDir := entry.IsDir()
		if !isDir && entry.Type()&fs.ModeSymlink != 0 {
			info, err := os.Stat(filepath.Join(dir, entry.Name()))
			if err != nil {
				// The stat error is not wrapped: fs.ErrNotExist here would
				// be read by newSeatProbe as "this rig has no polecats",
				// and a link to a missing worktree is a seat the probe
				// could not check, not an empty rig.
				return nil, fmt.Errorf("following %s: %v", filepath.Join(dir, entry.Name()), err)
			}
			isDir = info.IsDir()
		}
		if !isDir {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// heartbeatReader returns the heartbeat reading for townRoot. A session that
// wrote no heartbeat is (nil, nil); a file that exists but cannot be read or
// parsed is an error. The path is polecat.HeartbeatPath, the one
// TouchSessionHeartbeat writes, so the two readings cannot drift apart.
func heartbeatReader(townRoot string) func(string) (*polecat.SessionHeartbeat, error) {
	return func(sessionName string) (*polecat.SessionHeartbeat, error) {
		path := polecat.HeartbeatPath(townRoot, sessionName)
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		var hb polecat.SessionHeartbeat
		if err := json.Unmarshal(data, &hb); err != nil {
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		return &hb, nil
	}
}

// seatProbeFunc adapts a function to update.SeatProbe.
type seatProbeFunc func(ctx context.Context) ([]update.Seat, error)

func (f seatProbeFunc) Seats(ctx context.Context) ([]update.Seat, error) {
	return f(ctx)
}

// commandOutput runs cmd and returns its combined output. It is the process
// seam realRunFunc runs on, so a test answers a command without starting one.
type commandOutput func(cmd *exec.Cmd) ([]byte, error)

// realRunFunc is the production update.RunFunc the installer wrapper uses. It
// appends env to the process environment, so the wrapper's own entries reach
// the command without the caller repeating os.Environ(). A nonzero exit is a
// status reported with no error; a process that could not start, and one its
// canceled context stopped, are errors.
func realRunFunc() update.RunFunc {
	return newRunFunc(combinedOutput)
}

// combinedOutput runs cmd and returns everything it wrote, stdout and stderr
// interleaved.
func combinedOutput(cmd *exec.Cmd) ([]byte, error) {
	return cmd.CombinedOutput()
}

// commandWaitDelay is how long a command is given, after it exits or its
// context is canceled, to release an output pipe a child still holds. Without
// it, Wait blocks on the pipe after the command's own process is gone: a
// canceled install whose build left a compiler or container client running
// would never return the context's error, so the update would hang instead of
// stopping. Ten seconds, as the land gate gives a step for the same reason.
const commandWaitDelay = 10 * time.Second

// buildCommand is the exec.Cmd newRunFunc runs: the caller's directory, the
// process environment with env appended, and a WaitDelay so a canceled
// command returns even when a child holds the output pipe.
func buildCommand(ctx context.Context, dir string, env []string, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	cmd.WaitDelay = commandWaitDelay
	return cmd
}

// newRunFunc returns a RunFunc over output. A command that ran is read by what
// it exited with: zero is success, nonzero is a status carrying the output,
// each even when its context was canceled after it exited. Only a command that
// reached no status is an error — it could not start, or its context stopped
// it. A canceled context returns the context's error, never a -1 exit with a
// nil error.
func newRunFunc(output commandOutput) update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if err := ctx.Err(); err != nil {
			return "", -1, err
		}
		out, err := output(buildCommand(ctx, dir, env, name, args...))
		if err == nil {
			return string(out), 0, nil
		}
		// A process that exited with a real status is read by it: the cancel
		// landing just after the exit did not change what the command did. A
		// process killed by its context carries no status (-1), so it falls
		// through to the context's error.
		var exitCoder interface{ ExitCode() int }
		if errors.As(err, &exitCoder) && exitCoder.ExitCode() >= 0 {
			return string(out), exitCoder.ExitCode(), nil
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			return string(out), -1, ctxErr
		}
		return string(out), -1, err
	}
}

// newExecInstaller returns the real installer the update job runs. The
// installer builds and runs in a gt source checkout on main and fast-forwards
// it, so it takes the branch checkout GetRepoRootForTownBranch returns, not the
// detached build checkout the drift and staleness readers use. It also hands
// the installer the polecat-image trim context the daemon passes when the tier
// sweep runs off-host. An unreadable root or an invalid sweep context is an
// error, never an installer that trims the wrong engine.
func newExecInstaller(townRoot string, run update.RunFunc, cfg *daemon.DaemonPatrolConfig) (update.Installer, error) {
	repoRoot, err := version.GetRepoRootForTownBranch(townRoot)
	if err != nil {
		return nil, err
	}
	extraEnv, err := daemon.PolecatImageTrimEnv(cfg)
	if err != nil {
		return nil, err
	}
	return &update.ExecInstaller{RepoRoot: repoRoot, Run: run, ExtraEnv: extraEnv}, nil
}

// daemonServiceKey and daemonServiceImagePrefix find the daemon service's image
// line in the compose file the render writes: the service key at two spaces,
// and its image at four.
const (
	daemonServiceKey         = "  daemon:"
	daemonServiceImagePrefix = "    image:"
)

// setDaemonServiceImage returns compose with the daemon service's image set to
// ref. The line is found inside the daemon service's block; a file with no such
// line, or a ref that is not a digest reference, is refused, so a swap never
// writes a file it did not understand.
func setDaemonServiceImage(compose []byte, ref string) ([]byte, error) {
	if !strings.Contains(ref, "@sha256:") {
		return nil, fmt.Errorf("the daemon image %q is not a digest reference", ref)
	}
	lines := strings.SplitAfter(string(compose), "\n")
	inDaemon := false
	for i, line := range lines {
		text := strings.TrimRight(line, "\r\n")
		if !inDaemon {
			inDaemon = text == daemonServiceKey
			continue
		}
		if text != "" && !strings.HasPrefix(text, "    ") {
			break
		}
		if strings.HasPrefix(text, daemonServiceImagePrefix) {
			lines[i] = fmt.Sprintf("%s %q\n", daemonServiceImagePrefix, ref)
			return []byte(strings.Join(lines, "")), nil
		}
	}
	return nil, errors.New("the compose file's daemon service names no image line")
}

// composeDaemonSwap puts a digest install's pair in force on the container
// daemon. The compose file's daemon service takes the pair's daemon image, the
// daemon settings pin the pair's polecat image, and the service is recreated on
// both. The halves move together or not at all: the compose file is written
// first and the settings last, and a step that fails after a half has moved puts
// that half back from the values Apply held in memory. Apply refuses a town
// whose settings name no runner before it changes anything: the polecat half of
// the pair would have nothing to pin on.
type composeDaemonSwap struct {
	// ComposeFile is the container compose file the render writes.
	ComposeFile string
	// Project is the compose project the daemon service runs in.
	Project string
	// Context is the Docker CLI context the compose command runs in.
	Context string
	// ConfigPath is the daemon patrol settings file, which pins the polecat image.
	ConfigPath string
	// Run runs the compose command.
	Run update.RunFunc
	// writeCompose and writeSettings are the two writes Apply makes; nil means
	// the real write. A test replaces one to fail that step.
	writeCompose  func(path string, content []byte) error
	writeSettings func(path string, mutate func(cfg *config.DaemonPatrolConfig, exists bool) error) error
}

// Apply moves the daemon service and the polecat pin to pair and recreates the
// daemon. A failure names the step, never the command's output, which can hold
// a secret. When a step fails after a half has moved, both halves are put back
// before Apply returns; a half that cannot be put back is named in the error, so
// the caller escalates rather than reading the pair as whole.
func (s composeDaemonSwap) Apply(ctx context.Context, pair update.ImagePair) error {
	compose, err := os.ReadFile(s.ComposeFile)
	if err != nil {
		return fmt.Errorf("reading the compose file: %w", err)
	}
	next, err := setDaemonServiceImage(compose, pair.Daemon)
	if err != nil {
		return err
	}
	if _, err := s.readRunner(); err != nil {
		return err
	}
	if err := s.compose(s.ComposeFile, next); err != nil {
		return fmt.Errorf("writing the compose file: %w", err)
	}
	prevPin, err := s.pinPolecat(pair.Polecat)
	if err != nil {
		return s.undo(compose, "", false, fmt.Errorf("pinning the polecat image in the daemon settings: %w", err))
	}
	if err := s.recreate(ctx); err != nil {
		return s.undo(compose, prevPin, true, err)
	}
	return nil
}

// readRunner reads the tier sweep runner the polecat pin is set on. A settings
// file that is missing, or a town with no runner, is refused by name.
func (s composeDaemonSwap) readRunner() (*config.TierSweepRunnerConfig, error) {
	cfg, err := config.LoadDaemonPatrolConfig(s.ConfigPath)
	if errors.Is(err, config.ErrNotFound) {
		return nil, fmt.Errorf("the daemon settings %s do not exist", s.ConfigPath)
	}
	if err != nil {
		return nil, fmt.Errorf("reading the daemon settings: %w", err)
	}
	return tierSweepRunner(cfg)
}

// tierSweepRunner returns the runner the polecat pin is set on, or the reason
// the town has none.
func tierSweepRunner(cfg *config.DaemonPatrolConfig) (*config.TierSweepRunnerConfig, error) {
	if cfg.Patrols == nil || cfg.Patrols.TierSweep == nil || cfg.Patrols.TierSweep.Runner == nil {
		return nil, errors.New("the daemon settings name no tier_sweep runner, so the polecat image of the pair has nothing to pin on")
	}
	return cfg.Patrols.TierSweep.Runner, nil
}

// pinPolecat sets the runner's polecat image to image under the settings file's
// lock, and returns the image it replaced.
func (s composeDaemonSwap) pinPolecat(image string) (string, error) {
	var prev string
	err := s.settings(func(cfg *config.DaemonPatrolConfig, exists bool) error {
		if !exists {
			return fmt.Errorf("the daemon settings %s do not exist", s.ConfigPath)
		}
		runner, err := tierSweepRunner(cfg)
		if err != nil {
			return err
		}
		prev = runner.Image
		runner.Image = image
		return nil
	})
	return prev, err
}

// recreate recreates the daemon service on the compose file as it now reads.
func (s composeDaemonSwap) recreate(ctx context.Context) error {
	argv := []string{"compose", "-p", s.Project, "-f", s.ComposeFile, "up", "-d", "--force-recreate", "daemon"}
	_, exit, err := s.Run(ctx, filepath.Dir(s.ComposeFile), nil, "docker", append([]string{"--context", s.Context}, argv...)...)
	if err != nil {
		return fmt.Errorf("running docker compose for the daemon: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("docker compose for the daemon exited %d", exit)
	}
	return nil
}

// undo puts back each half Apply moved, after cause stopped it: the settings pin
// when it was written, then the compose file from compose. Both are attempted,
// and a half that cannot be put back is named with cause.
func (s composeDaemonSwap) undo(compose []byte, prevPin string, pinned bool, cause error) error {
	var failed []error
	if pinned {
		if _, err := s.pinPolecat(prevPin); err != nil {
			failed = append(failed, fmt.Errorf("restoring the polecat pin: %w", err))
		}
	}
	if err := s.compose(s.ComposeFile, compose); err != nil {
		failed = append(failed, fmt.Errorf("restoring the compose file: %w", err))
	}

	if len(failed) > 0 {
		return fmt.Errorf("%w; the pair is split and could not be put back: %w", cause, errors.Join(failed...))
	}
	return fmt.Errorf("%w; both halves of the pair were put back", cause)
}

// compose writes the compose file, through writeCompose when a test set one.
func (s composeDaemonSwap) compose(path string, content []byte) error {
	if s.writeCompose != nil {
		return s.writeCompose(path, content)
	}
	return daemonimage.WriteComposeFile(path, content)
}

// settings runs mutate under the daemon settings file's lock, through
// writeSettings when a test set one.
func (s composeDaemonSwap) settings(mutate func(cfg *config.DaemonPatrolConfig, exists bool) error) error {
	if s.writeSettings != nil {
		return s.writeSettings(s.ConfigPath, mutate)
	}
	return config.UpdateConfigJSON(s.ConfigPath, 0o644, mutate)
}

// realImageSwap is the swap the town's container daemon takes: its compose file,
// its compose project and its settings, all read from the town. A town that
// names no compose project has no container daemon to move, and is refused.
func realImageSwap(townRoot string) (update.ImageSwap, error) {
	cfg, err := loadUpdatePatrolConfig(townRoot)
	if err != nil {
		return nil, err
	}
	project, err := containerComposeProject(cfg)
	if err != nil {
		return nil, fmt.Errorf("the daemon image install needs the container daemon's compose project: %w", err)
	}
	return composeDaemonSwap{
		ComposeFile: config.ContainerComposeFile(townRoot),
		Project:     project,
		Context:     defaultDockerContext,
		ConfigPath:  config.DaemonPatrolConfigPath(townRoot),
		Run:         realRunFunc(),
	}, nil
}
