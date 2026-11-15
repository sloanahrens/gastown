package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/usage"
	"github.com/steveyegge/gastown/internal/workspace"
)

// usageSampleDeps is what `gt usage sample` reads from outside itself: the
// sample clock and the claude_plan store. Production takes the defaults; a
// test builds its own, so no package variable is swapped.
type usageSampleDeps struct {
	now       func() time.Time
	openStore func() (usage.Store, error)
}

// defaultUsageSampleDeps is the production wiring: the wall clock and the
// town's claude_plan rows.
func defaultUsageSampleDeps() usageSampleDeps {
	return usageSampleDeps{now: time.Now, openStore: openClaudePlanStore}
}

// openClaudePlanStore opens the town's cost_samples table, where the status-line
// hook appends its claude_plan rows.
func openClaudePlanStore() (usage.Store, error) {
	return openClaudePlanStoreFrom(workspace.FindFromCwdOrError)
}

// openClaudePlanStoreFrom builds the store over the beads client of the town
// findRoot names. A town root that cannot be found is an error, never a store
// over some other database.
func openClaudePlanStoreFrom(findRoot func() (string, error)) (usage.Store, error) {
	townRoot, err := findRoot()
	if err != nil {
		return nil, fmt.Errorf("the cost_samples table lives in the town: %w", err)
	}
	return clientClaudePlanStore{client: beads.New(townRoot)}, nil
}

// clientClaudePlanStore is the usage.Store over the beads client's
// claude_plan rows: the kind gt usage sample appends.
type clientClaudePlanStore struct {
	client beads.Client
}

// NewestClaudePlan returns the newest claude_plan row, or nil when the table
// holds none. A row missing one of the four readings is an error: it is not a
// reading, and a nil field read as zero would invent a plan the status line
// never reported.
func (s clientClaudePlanStore) NewestClaudePlan(ctx context.Context) (*usage.ClaudePlan, error) {
	rows, err := s.client.ReadCostSamples(ctx, beads.SampleKindClaudePlan, time.Time{}, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	p, err := claudePlanOfSample(rows[0])
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// WriteClaudePlanAfter re-reads the newest claude_plan row and appends p only
// while that row still matches prev. bd's samples table takes appends only and
// has no conditional add, so the compare and the append are two steps: a row
// another writer appends between this read and the write is not seen. The
// re-read is as close to the append as this store can put it, and two writers
// racing over one town cost one duplicate heartbeat row at worst.
func (s clientClaudePlanStore) WriteClaudePlanAfter(ctx context.Context, p usage.ClaudePlan, prev *usage.ClaudePlan) error {
	newest, err := s.NewestClaudePlan(ctx)
	if err != nil {
		return fmt.Errorf("re-read newest claude_plan row: %w", err)
	}
	if !sameClaudePlan(newest, prev) {
		return usage.ErrNewestChanged
	}
	if err := s.client.WriteCostSample(ctx, sampleOfClaudePlan(p)); err != nil {
		return fmt.Errorf("write claude_plan row: %w", err)
	}
	return nil
}

// sameClaudePlan reports whether two newest-row readings are the same row: both
// absent, or both present at the same sample time with the same four values.
func sameClaudePlan(a, b *usage.ClaudePlan) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.At.Equal(b.At) && a.SameValues(*b)
}

// claudePlanOfSample reads a stored claude_plan row. Every reading must be
// present for the row to be a plan reading.
func claudePlanOfSample(s beads.Sample) (usage.ClaudePlan, error) {
	switch {
	case s.FiveHourPct == nil:
		return usage.ClaudePlan{}, fmt.Errorf("row has no five_hour_pct")
	case s.FiveHourResetsAt == nil:
		return usage.ClaudePlan{}, fmt.Errorf("row has no five_hour_resets_at")
	case s.SevenDayPct == nil:
		return usage.ClaudePlan{}, fmt.Errorf("row has no seven_day_pct")
	case s.SevenDayResetsAt == nil:
		return usage.ClaudePlan{}, fmt.Errorf("row has no seven_day_resets_at")
	}
	return usage.ClaudePlan{
		At:               s.At.UTC(),
		FiveHourPct:      *s.FiveHourPct,
		FiveHourResetsAt: s.FiveHourResetsAt.UTC(),
		SevenDayPct:      *s.SevenDayPct,
		SevenDayResetsAt: s.SevenDayResetsAt.UTC(),
	}, nil
}

// sampleOfClaudePlan is p as the claude_plan row the store appends.
func sampleOfClaudePlan(p usage.ClaudePlan) beads.Sample {
	fivePct, sevenPct := p.FiveHourPct, p.SevenDayPct
	fiveResets, sevenResets := p.FiveHourResetsAt, p.SevenDayResetsAt
	return beads.Sample{
		Kind:             beads.SampleKindClaudePlan,
		At:               p.At.UTC(),
		FiveHourPct:      &fivePct,
		FiveHourResetsAt: &fiveResets,
		SevenDayPct:      &sevenPct,
		SevenDayResetsAt: &sevenResets,
	}
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
