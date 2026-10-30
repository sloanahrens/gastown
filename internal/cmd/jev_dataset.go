package cmd

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev/dataset"
	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/workspace"
)

// jevDatasetDeps is what the dataset subcommand takes from the host: the town
// whose batteries it reads. A test supplies the lookup, so no unit test needs a
// working directory inside a town.
type jevDatasetDeps struct {
	findTownRoot func() (string, error)
}

// realJevDatasetDeps is jevDatasetDeps on this host.
func realJevDatasetDeps() jevDatasetDeps {
	return jevDatasetDeps{findTownRoot: workspace.FindFromCwdOrError}
}

// jevDatasetFlags is one dataset command's flags; a command owns its own so a
// later run never inherits an earlier run's values.
type jevDatasetFlags struct {
	battery  string
	question string
	since    string
	out      string
}

// newJevDatasetCmd builds `gt jev dataset`.
func newJevDatasetCmd(d jevDatasetDeps) *cobra.Command {
	var f jevDatasetFlags
	cmd := &cobra.Command{
		Use:   "dataset",
		Short: "Export a battery question's labeled answers as a flat CSV",
		Long: `Read a battery's log through its registered adapter and write one row per
labeled item that answered --question, for the mango analysis to mine: where
Jev disagrees with the label and what distinguishes its failures. The file
holds ids and features only. Prompt text, passages, titles, transcripts and
log samples never enter it, and a question whose name could carry them is an
error rather than a silently dropped column.

The columns are the opaque row id, the day, hour and weekday of the answer,
the model, the answer (a noul probability, a score or the choice name), the
answer's calibration band or score level, the choice's confidence, the label as
true or false with its rule and source, a ref_ column for each ref the adapter
put on the item (rig, agent, attempt and target only), and an ans_ column for
each other question the same item answered. Rows are written in hash order,
not time order, so any leading hundred rows are a sample of the file. A
categorical column with more than fifteen distinct values is dropped and named
in the dict. An empty cell is a missing value.

The dict beside the file names each column, the type mango will infer for it,
what it means, the row count, the rows' date range and the dropped columns.
--out defaults to <town>/.runtime/jev-dataset, and the files are named
<battery>.<question>.csv and <battery>.<question>.dict.md.

Examples:
  gt jev dataset --battery logtriage --question needs_action --since 30d
  gt jev dataset --battery finding --question any_duplicate --out ./scratch`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runJevDataset(cmd.Context(), cmd.OutOrStdout(), d, f, time.Now())
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.battery, "battery", "", "Battery to export (required)")
	fl.StringVar(&f.question, "question", "", "Question whose labeled answers become the rows (required)")
	fl.StringVar(&f.since, "since", "", "Export only lines from this window ending now: Nd or a Go duration (default: all time)")
	fl.StringVar(&f.out, "out", "", "Directory for the CSV and the dict (default: <town>/.runtime/jev-dataset)")
	_ = cmd.MarkFlagRequired("battery")
	_ = cmd.MarkFlagRequired("question")
	return cmd
}

func init() {
	jevCmd.AddCommand(newJevDatasetCmd(realJevDatasetDeps()))
}

// runJevDataset finds the town and exports the question out of it.
func runJevDataset(ctx context.Context, w io.Writer, d jevDatasetDeps, f jevDatasetFlags, now time.Time) error {
	townRoot, err := d.findTownRoot()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return jevDatasetWithFlags(ctx, w, townRoot, f.battery, f.question, f.since, f.out, now)
}

// jevDatasetWithFlags reads the battery, exports the question and names the
// files it wrote.
func jevDatasetWithFlags(ctx context.Context, w io.Writer, townRoot, battery, question, since, out string, now time.Time) error {
	from, err := parseSince(since, now)
	if err != nil {
		return err
	}
	a, err := scorecard.Open(battery, townRoot)
	if err != nil {
		return err
	}
	read, err := scorecard.Read(ctx, a, from)
	if err != nil {
		return err
	}
	if out == "" {
		out = dataset.Dir(townRoot)
	}
	csvPath, dictPath, rows, err := dataset.Export(read, battery, scorecard.QuestionID(question), out)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "wrote %s and %s (%d rows)\n", csvPath, dictPath, rows)
	return err
}
