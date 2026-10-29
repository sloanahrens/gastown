package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/dispatch"
	"github.com/steveyegge/gastown/internal/polecatimage"
	"github.com/steveyegge/gastown/internal/slot"
)

const (
	// defaultQuietBudget is how long a cutover waits for a quiet town before it
	// gives up: long enough for the operator's own hold or a suite holding the
	// gate to finish, and short enough that a stuck probe ends the wait.
	defaultQuietBudget = 30 * time.Minute

	// defaultQuietPoll is how often WaitQuiet reads the probes meanwhile.
	defaultQuietPoll = 15 * time.Second
)

// QuietProbes is the three readings that decide whether the daemon may be
// swapped: whether the operator's dispatch hold is on, whether a `gt sling` is
// running, and whether the container-gate pool has a free slot (gt-qpeei.22).
// Each answers for the condition its name states, and an error means the probe
// could not look, which is never quiet.
type QuietProbes struct {
	DispatchHeld func() (bool, error)
	SlingRunning func() (bool, error)
	GateBusy     func() (bool, error)
}

// QuietProbeDeps overrides how the host probes read the host; a nil field reads
// the host itself.
type QuietProbeDeps struct {
	// Stat is the only filesystem call the probes make, so a test can record
	// every read: the hold file is the operator's to create and remove.
	Stat func(name string) (fs.FileInfo, error)
	// Run starts pgrep, the one program the probes run.
	Run func(name string, args ...string) error
	// GateStatus reads the container-gate pool, the `docker ps` cross-check
	// included, because a suite the slot does not know about is still a suite.
	// slot's own status cleans up a dead holder's display-only owner file as it
	// reads (internal/slot/pool.go), which is the one file the gate may remove;
	// it creates none and never touches the operator's hold.
	GateStatus func(townRoot string) (slot.Report, error)
}

// NewQuietProbes returns the host probes WaitQuiet reads for townRoot. It
// refuses a town root that is not absolute, because a relative path would read
// the hold file and the gate slot out of whatever directory the caller is in.
func NewQuietProbes(townRoot string, deps QuietProbeDeps) (QuietProbes, error) {
	if !filepath.IsAbs(townRoot) {
		return QuietProbes{}, fmt.Errorf("the quiet gate needs an absolute town root, got %q", townRoot)
	}
	stat := deps.Stat
	if stat == nil {
		stat = os.Stat
	}
	run := deps.Run
	if run == nil {
		run = func(name string, args ...string) error {
			return exec.Command(name, args...).Run()
		}
	}
	gateStatus := deps.GateStatus
	if gateStatus == nil {
		gateStatus = func(townRoot string) (slot.Report, error) {
			return slot.StatusPool(townRoot, containerGatePool(townRoot))
		}
	}
	holdPath := dispatch.HoldFilePath(townRoot)
	return QuietProbes{
		DispatchHeld: func() (bool, error) {
			_, err := stat(holdPath)
			switch {
			case err == nil:
				return true, nil
			case errors.Is(err, fs.ErrNotExist):
				return false, nil
			default:
				return false, fmt.Errorf("reading %s: %w", holdPath, err)
			}
		},
		SlingRunning: func() (bool, error) {
			err := run("pgrep", "-f", polecatimage.SlingPattern)
			switch {
			case err == nil:
				return true, nil
			case processExitCode(err) == 1:
				return false, nil
			default:
				return false, fmt.Errorf("pgrep could not say whether a gt sling is running: %w", err)
			}
		},
		GateBusy: func() (bool, error) {
			rep, err := gateStatus(townRoot)
			if err != nil {
				return false, fmt.Errorf("reading the container-gate slot: %w", err)
			}
			return rep.Busy(), nil
		},
	}, nil
}

// WaitQuiet returns nil once the town is quiet: the dispatch hold is on, no
// `gt sling` is running and the container-gate slot is free. It reads the
// probes at once and then once per poll, and gives up at budget with an error
// naming the first blocker standing at the last read, having changed nothing. A
// budget or poll that is not positive takes the default (gt-qpeei.22).
func WaitQuiet(ctx context.Context, q QuietProbes, clk clockwork.Clock, budget, poll time.Duration) error {
	if clk == nil {
		clk = clockwork.NewRealClock()
	}
	if budget <= 0 {
		budget = defaultQuietBudget
	}
	if poll <= 0 {
		poll = defaultQuietPoll
	}
	if missing := q.missing(); missing != "" {
		return fmt.Errorf("cannot wait for a quiet town: %s", missing)
	}
	deadline := clk.Now().Add(budget)
	for {
		blocker := q.blocker()
		if blocker == "" {
			return nil
		}
		if !clk.Now().Before(deadline) {
			return fmt.Errorf("no quiet town within %s: %s", budget, blocker)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("no quiet town: %s (%w)", blocker, ctx.Err())
		case <-clk.After(poll):
		}
	}
}

// missing names a probe WaitQuiet was not given, so a caller's unfinished
// literal is refused rather than read as quiet.
func (q QuietProbes) missing() string {
	switch {
	case q.DispatchHeld == nil:
		return "the dispatch-hold probe is not set"
	case q.SlingRunning == nil:
		return "the sling probe is not set"
	case q.GateBusy == nil:
		return "the container-gate probe is not set"
	}
	return ""
}

// blocker is the first reading, in probe order, that keeps the town from being
// quiet, and "" when all three are quiet. A probe that errors is a blocker: a
// reading that could not be taken must never pass for a quiet one.
func (q QuietProbes) blocker() string {
	held, err := q.DispatchHeld()
	switch {
	case err != nil:
		return fmt.Sprintf("the dispatch-hold probe cannot answer (%v)", err)
	case !held:
		return fmt.Sprintf("the dispatch hold is not on (no %s)", dispatch.HoldFileName)
	}
	running, err := q.SlingRunning()
	switch {
	case err != nil:
		return fmt.Sprintf("the sling probe cannot answer (%v)", err)
	case running:
		return "a gt sling is running"
	}
	busy, err := q.GateBusy()
	switch {
	case err != nil:
		return fmt.Sprintf("the container-gate probe cannot answer (%v)", err)
	case busy:
		return "the container-gate slot is busy"
	}
	return ""
}

// processExitCode is the exit status err carries, or -1 when err is not an exit
// at all, so a missing pgrep cannot read as pgrep's "nothing matched".
func processExitCode(err error) int {
	var exited interface{ ExitCode() int }
	if errors.As(err, &exited) {
		return exited.ExitCode()
	}
	return -1
}
