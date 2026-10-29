package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/beads/backend"
	"github.com/steveyegge/gastown/internal/trackerreplay"
	"github.com/steveyegge/gastown/internal/workspace"
	"github.com/steveyegge/gastown/tracker/client"
)

// Tracker replay flags
var (
	trackerReplayRig    string
	trackerReplaySince  int64
	trackerReplayDryRun bool
)

var trackerCmd = &cobra.Command{
	Use:     "tracker",
	GroupID: GroupServices,
	Short:   "Operate the town tracker",
	Long:    `Operate the town tracker, the service that holds rigs' issues once they flip off Dolt.`,
	RunE:    requireSubcommand,
}

var trackerReplayCmd = &cobra.Command{
	Use:   "replay",
	Short: "Replay the tracker's events since a seq onto a rig's Dolt through bd",
	Long: `Replay the tracker's events since a seq onto a rig's Dolt through bd.

Rollback: after a rig flips back to Dolt following a soak on the tracker, this
applies the writes the tracker took since the flip, so none are lost. Each
event becomes one bd call. The run stops at the first event the feed cannot
replay (it names the kind and seq; nothing is guessed) or the first bd call
that fails, and prints the last seq applied. Rerun with --since-seq set to that
seq to resume.

The feed carries a created issue's title only, so each creation replays with bd's
defaults for priority, type and description; the run counts them on stderr so
you can review those issues. The rig must already be routed to Dolt. --dry-run
prints the bd calls and runs none.

Examples:
  gt tracker replay --rig agentlab --since-seq 1200 --dry-run
  gt tracker replay --rig agentlab --since-seq 1200`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTrackerReplay(liveReplayEnv(), cmd.OutOrStdout(), cmd.ErrOrStderr(), trackerReplayRig, trackerReplaySince, trackerReplayDryRun)
	},
}

func init() {
	trackerReplayCmd.Flags().StringVar(&trackerReplayRig, "rig", "", "Rig to replay onto, by its path or beads prefix")
	trackerReplayCmd.Flags().Int64Var(&trackerReplaySince, "since-seq", 0, "Replay the events after this seq (a resumed run passes the last applied seq)")
	trackerReplayCmd.Flags().BoolVar(&trackerReplayDryRun, "dry-run", false, "Print the bd calls and run none")
	_ = trackerReplayCmd.MarkFlagRequired("rig")

	trackerCmd.AddCommand(trackerReplayCmd)
	rootCmd.AddCommand(trackerCmd)
}

// replayEnv is what a replay reads from outside the process: the town, its
// route table, the environment, the tracker's feed and bd. Each is a field so
// the refusals are unit tests.
type replayEnv struct {
	findTown   func() (string, error)
	loadRoutes func(beadsDir string) ([]beads.Route, error)
	getenv     func(string) string
	newSource  func(cfg client.Config) (trackerreplay.Source, error)
	newApply   func(rigDir string) trackerreplay.Apply
}

// liveReplayEnv is the production environment.
func liveReplayEnv() replayEnv {
	return replayEnv{
		findTown:   workspace.FindFromCwdOrError,
		loadRoutes: beads.LoadRoutes,
		getenv:     os.Getenv,
		newSource: func(cfg client.Config) (trackerreplay.Source, error) {
			tc, err := client.New(cfg)
			if err != nil {
				return nil, err
			}
			return tc, nil
		},
		newApply: func(rigDir string) trackerreplay.Apply {
			return bdApply(beads.New(rigDir).Run)
		},
	}
}

// bdApply runs each replayed call through a bd runner. The runner is a
// parameter so a failing call is a unit test.
func bdApply(run func(args ...string) ([]byte, error)) trackerreplay.Apply {
	return func(argv []string) error {
		_, err := run(argv...)
		return err
	}
}

func runTrackerReplay(env replayEnv, out, errOut io.Writer, rig string, since int64, dryRun bool) error {
	if since < 0 {
		return fmt.Errorf("gt tracker replay: --since-seq must not be negative (got %d)", since)
	}
	rigDir, src, err := openTrackerReplay(env, rig)
	if err != nil {
		return err
	}
	res, err := trackerreplay.Replay(context.Background(), src, env.newApply(rigDir), since, dryRun, out)
	replayDefaultedNote(errOut, res)
	if err != nil {
		replayStopHint(errOut, res)
		return err
	}
	if dryRun {
		fmt.Fprintf(out, "dry run: %d bd calls, none run; would apply through seq %d\n", res.Commands, res.Applied)
		return nil
	}
	fmt.Fprintf(out, "applied %d bd calls through seq %d\n", res.Commands, res.Applied)
	return nil
}

// replayDefaultedNote says how many creations replayed with bd's defaults. The
// feed carries a created issue's title only, so its priority, type and
// description come back as bd's defaults: that is told on every run that did
// it, so the rollback never fills in a field without saying so.
func replayDefaultedNote(w io.Writer, res trackerreplay.Result) {
	if res.Defaulted == 0 {
		return
	}
	fmt.Fprintf(w, "warning: %d created issue(s) replayed with bd's defaults for priority, type and description (the feed carries only the title); review them in the rig\n", res.Defaulted)
}

// replayStopHint says how to resume a stopped replay. A run whose first call
// failed applied nothing, so the hint says so rather than pointing at a seq it
// never reached: rerunning from there meets the same refusal.
func replayStopHint(w io.Writer, res trackerreplay.Result) {
	if res.Commands == 0 {
		fmt.Fprintf(w, "replay stopped before applying anything (seq %d); fix the cause, then rerun with --since-seq %d\n", res.Applied, res.Applied)
		return
	}
	fmt.Fprintf(w, "replay stopped: last applied seq %d; fix the cause, then rerun with --since-seq %d\n", res.Applied, res.Applied)
}

// openTrackerReplay resolves the rig's directory, where bd runs, and the
// tracker's feed.
func openTrackerReplay(env replayEnv, rig string) (string, trackerreplay.Source, error) {
	townRoot, err := env.findTown()
	if err != nil {
		return "", nil, fmt.Errorf("gt tracker replay: %w", err)
	}
	routes, err := env.loadRoutes(filepath.Join(townRoot, ".beads"))
	if err != nil {
		return "", nil, fmt.Errorf("gt tracker replay: %w", err)
	}
	route, err := pickRigRoute(routes, rig)
	if err != nil {
		return "", nil, fmt.Errorf("gt tracker replay: %w", err)
	}
	tcfg := backend.FromEnv(env.getenv).Tracker
	if tcfg.BaseURL == "" {
		return "", nil, errors.New("gt tracker replay: TRACKER_URL is not set")
	}
	src, err := env.newSource(tcfg)
	if err != nil {
		return "", nil, fmt.Errorf("gt tracker replay: %w", err)
	}
	return filepath.Join(townRoot, route.Path), src, nil
}

// pickRigRoute finds the rig's route by its path or beads prefix. The replay
// writes through bd, so a rig still routed to the tracker is refused: its bd
// calls would land back on the tracker, not on Dolt.
func pickRigRoute(routes []beads.Route, rig string) (beads.Route, error) {
	want := strings.TrimSuffix(rig, "-")
	for _, r := range routes {
		if r.Path != rig && strings.TrimSuffix(r.Prefix, "-") != want {
			continue
		}
		if r.Backend == beads.BackendTracker {
			return beads.Route{}, fmt.Errorf("rig %s is still routed to the tracker; flip it back to dolt before replaying", rig)
		}
		return r, nil
	}
	return beads.Route{}, fmt.Errorf("no rig %q in the route table", rig)
}
