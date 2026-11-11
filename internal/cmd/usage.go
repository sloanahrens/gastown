package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/usage"
)

// usageSampleDeps is what `gt usage sample` reads from outside itself: the
// sample clock and the claude_plan store. Production takes the defaults; a
// test builds its own, so no package variable is swapped.
type usageSampleDeps struct {
	now       func() time.Time
	openStore func() (usage.Store, error)
}

// defaultUsageSampleDeps is the production wiring. The beads cost_samples
// client (gt-8r17fp) is not on the bead store yet, so openClaudePlanStore
// refuses by name: a run then reports the missing store on stderr and writes
// nothing, instead of pretending the row landed.
func defaultUsageSampleDeps() usageSampleDeps {
	return usageSampleDeps{now: time.Now, openStore: openClaudePlanStore}
}

func openClaudePlanStore() (usage.Store, error) {
	return nil, fmt.Errorf("%w: the beads cost_samples client (gt-8r17fp) has not landed", usage.ErrNoStore)
}

var usageCmd = &cobra.Command{
	Use:     "usage",
	GroupID: GroupDiag,
	Short:   "Record and read Claude usage samples",
	Long: `Record and read the Claude subscription usage samples the plan meter
folds into the dashboard.`,
	RunE: requireSubcommand,
}

func init() {
	rootCmd.AddCommand(usageCmd)
	usageCmd.AddCommand(newUsageSampleCmd(defaultUsageSampleDeps()))
}

func newUsageSampleCmd(deps usageSampleDeps) *cobra.Command {
	var strict bool
	cmd := &cobra.Command{
		Use:   "sample",
		Short: "Record one status-line reading as a claude_plan row",
		Long: `Read one Claude Code status-line JSON document on stdin and write one
claude_plan cost sample from its rate_limits.

Stdin is the status-line hook's payload. The fields read are
rate_limits.five_hour and rate_limits.seven_day, each with used_percentage
(0-100) and resets_at (epoch seconds). Other fields are ignored.

Dedupe: a reading whose four values match the newest claude_plan row is not
written again while that row is under ten minutes old. A row older than that,
or a reading with any value changed, is written.

A payload with no rate_limits (an API-key session, or one before its first
response) writes nothing and exits 0.

The status line must not fail, so by default any error prints one line on
stderr and the command exits 0. --strict makes an error exit 1 instead, for
tests and hand runs.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			err := runUsageSample(cmd.Context(), cmd.InOrStdin(), deps)
			if err == nil {
				return nil
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "gt usage sample: %v\n", err)
			if strict {
				return NewSilentExit(1)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&strict, "strict", false, "exit 1 on any error instead of exiting 0 after a stderr line")
	return cmd
}

// maxStatusLineBytes caps the stdin payload. A payload over this cap is refused
// after reading at most one byte past it, so an oversized payload is never
// read in full.
const maxStatusLineBytes = 64 << 10

// runUsageSample is one status-line reading end to end: parse, then record.
// It returns an error for every failure, and the caller decides the exit.
func runUsageSample(ctx context.Context, in io.Reader, deps usageSampleDeps) error {
	in = io.LimitReader(in, maxStatusLineBytes+1)
	data, err := io.ReadAll(in)
	if err != nil {
		return fmt.Errorf("read stdin: %w", err)
	}
	if len(data) > maxStatusLineBytes {
		return fmt.Errorf("stdin is over %d bytes: not a status-line payload", maxStatusLineBytes)
	}
	reading, ok, err := usage.ParseStatusLine(data)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	reading.At = deps.now().UTC()
	store, err := deps.openStore()
	if err != nil {
		return err
	}
	_, err = usage.Record(ctx, store, reading)
	return err
}
