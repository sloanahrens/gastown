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

	"github.com/steveyegge/gastown/internal/daemon"
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
// files and dot entries as `gt polecat list` does. A missing dir keeps its
// fs.ErrNotExist, which newSeatProbe reads as a rig with no polecats.
func polecatDirs(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

// heartbeatReader returns the heartbeat reading for townRoot. A session that
// wrote no heartbeat is (nil, nil); a file that exists but cannot be read or
// parsed is an error. The path is the one polecat.TouchSessionHeartbeat
// writes.
func heartbeatReader(townRoot string) func(string) (*polecat.SessionHeartbeat, error) {
	return func(sessionName string) (*polecat.SessionHeartbeat, error) {
		path := filepath.Join(townRoot, ".runtime", "heartbeats", sessionName+".json")
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
// cancelled context stopped, are errors.
func realRunFunc() update.RunFunc {
	return newRunFunc(combinedOutput)
}

// combinedOutput runs cmd and returns everything it wrote, stdout and stderr
// interleaved.
func combinedOutput(cmd *exec.Cmd) ([]byte, error) {
	return cmd.CombinedOutput()
}

// newRunFunc returns a RunFunc over output. A nonzero exit is a status
// carrying the output. An error means the command reached no status: it could
// not start, or the context stopped it. A cancelled context returns the
// context's error, never a -1 exit with a nil error.
func newRunFunc(output commandOutput) update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		if err := ctx.Err(); err != nil {
			return "", -1, err
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := output(cmd)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return string(out), -1, ctxErr
		}
		if err == nil {
			return string(out), 0, nil
		}
		var exitCoder interface{ ExitCode() int }
		if errors.As(err, &exitCoder) {
			return string(out), exitCoder.ExitCode(), nil
		}
		return string(out), -1, err
	}
}

// newExecInstaller returns the real installer the update job runs. It builds
// and runs in the town's own gt source checkout, the root the daemon's rebuild
// job uses, and hands the installer the polecat-image trim context the daemon
// passes when the tier sweep runs off-host. An unreadable root or an invalid
// sweep context is an error, never an installer that trims the wrong engine.
func newExecInstaller(townRoot string, run update.RunFunc, cfg *daemon.DaemonPatrolConfig) (update.Installer, error) {
	repoRoot, err := version.GetRepoRootForTown(townRoot)
	if err != nil {
		return nil, err
	}
	extraEnv, err := daemon.PolecatImageTrimEnv(cfg)
	if err != nil {
		return nil, err
	}
	return &update.ExecInstaller{RepoRoot: repoRoot, Run: run, ExtraEnv: extraEnv}, nil
}
