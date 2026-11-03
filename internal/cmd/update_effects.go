package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"

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
	// Heartbeat is polecat.ReadSessionHeartbeat for the town root.
	Heartbeat func(session string) *polecat.SessionHeartbeat
}

// newSeatProbe returns the probe update.Drain watches. It reads each polecat's
// heartbeat the way the daemon's workingPolecats does, so a drain and the
// daemon's own gc report agree on who is working: Working is the
// agent-reported state, and LastActive is when the heartbeat was last written.
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
				seats = append(seats, seatOf(env, rig, name))
			}
		}
		return seats, nil
	})
}

// seatOf reads one polecat's seat. A heartbeat that is missing or unreadable
// reads as not working with a zero LastActive, the same reading the daemon
// takes; the drain's freshness bound then decides.
func seatOf(env seatProbeEnv, rig, name string) update.Seat {
	seat := update.Seat{Name: rig + "/" + name}
	hb := env.Heartbeat(session.PolecatSessionName(env.Prefix(rig), name))
	if hb == nil {
		return seat
	}
	seat.Working = hb.EffectiveState() == polecat.HeartbeatWorking
	seat.LastActive = hb.Timestamp
	return seat
}

// seatProbeFunc adapts a function to update.SeatProbe.
type seatProbeFunc func(ctx context.Context) ([]update.Seat, error)

func (f seatProbeFunc) Seats(ctx context.Context) ([]update.Seat, error) {
	return f(ctx)
}

// realRunFunc is the production update.RunFunc the installer wrapper uses. It
// appends env to the process environment, so the wrapper's own entries reach
// the command without the caller repeating os.Environ(). A nonzero exit is a
// status reported with no error; only a process that could not start is an
// error.
func realRunFunc() update.RunFunc {
	return func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err == nil {
			return string(out), 0, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return string(out), exitErr.ExitCode(), nil
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
