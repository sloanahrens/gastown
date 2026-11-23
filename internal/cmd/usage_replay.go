package cmd

import (
	"fmt"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/usageattrib"
	"github.com/steveyegge/gastown/internal/workspace"
)

// usageReplayDeps is what `gt usage replay` reads from outside itself: the town,
// its usage tree and its store. A test supplies its own, reaching no real town.
type usageReplayDeps struct {
	townRoot  func() (string, error)
	usageDir  func(townRoot string) string
	openStore func(townRoot string) (usageattrib.ReplayStore, error)
}

func defaultUsageReplayDeps() usageReplayDeps {
	return usageReplayDeps{
		townRoot:  workspace.FindFromCwdOrError,
		usageDir:  func(town string) string { return filepath.Join(constants.TownRuntimePath(town), "usage") },
		openStore: func(town string) (usageattrib.ReplayStore, error) { return beads.New(town), nil },
	}
}

func newUsageReplayCmd(deps usageReplayDeps) *cobra.Command {
	var apply bool
	cmd := &cobra.Command{
		Use:   "replay",
		Short: "Write the usage rows the record files are missing (a dry run by default)",
		Long: `Write a usage_records row for each record file whose bead and kind has none, from
<town>/.runtime/usage into the town's usage_records store, whatever the working directory.
By default the run is a dry run and writes nothing; --apply writes the rows. Output is one
line per count (files, written or would write, skipped, failed), then one line per failed
file on stderr. Exit 0 when no file failed, 1 when one did, 2 when the run could not be made.`,
		Args: cobra.NoArgs, SilenceUsage: true, SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runUsageReplay(cmd, deps, apply)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "write the rows; without it the run is a dry run")
	return cmd
}

func runUsageReplay(cmd *cobra.Command, deps usageReplayDeps, apply bool) error {
	town, err := deps.townRoot()
	if err != nil {
		return replayFail(cmd, fmt.Errorf("finding the town: %w", err))
	}
	store, err := deps.openStore(town)
	if err != nil {
		return replayFail(cmd, fmt.Errorf("opening the usage_records store: %w", err))
	}
	res, err := usageattrib.Replay(cmd.Context(), deps.usageDir(town), store, !apply)
	if err != nil {
		return replayFail(cmd, err)
	}
	if apply {
		fmt.Fprintf(cmd.OutOrStdout(), "files: %d\nwritten: %d\n", res.Files, res.Written)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "dry run: no row written\nfiles: %d\nwould write: %d\n", res.Files, res.Written)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "skipped: %d\nfailed: %d\n", res.Skipped, len(res.Failed))
	for _, f := range res.Failed {
		fmt.Fprintf(cmd.ErrOrStderr(), "failed: %v\n", f)
	}
	if len(res.Failed) > 0 {
		return NewSilentExit(1)
	}
	return nil
}

// replayFail reports a run that could not be made, not a file that failed inside it.
func replayFail(cmd *cobra.Command, err error) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "gt usage replay: %v\n", err)
	return NewSilentExit(2)
}
