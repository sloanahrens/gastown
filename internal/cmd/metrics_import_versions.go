package cmd

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beadversions"
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	metricsImportVersionsPath string
	metricsImportVersionsRig  string
)

func init() {
	metricsImportVersionsCmd.Flags().StringVar(&metricsImportVersionsPath, "path", "", "Version store file (default <town>/.runtime/metrics/bead-versions.jsonl)")
	metricsImportVersionsCmd.Flags().StringVar(&metricsImportVersionsRig, "rig", "gt", "Database in the archive whose first-filed rows are imported")
	metricsCmd.AddCommand(metricsImportVersionsCmd)
}

var metricsImportVersionsCmd = &cobra.Command{
	Use:   "import-versions",
	Short: "Import the archive's first-filed bead texts into the version store",
	Long: `Read the tracker's 15-minute git export at <town>/.dolt-archive/git and record
each bead's text as filed as its version at the bead's created_at, under the
actor "archive".

One export commit is sampled per local day, the day's last, and the first commit
carrying a bead supplies its text, so the store starts with history from before
it existed. The text is the description, a blank line, then the acceptance
criteria. A bead whose created_at falls on the sampled day of the commit that
first carries it cannot be told apart from a bead edited later that day, so it
is not imported: the store would hold a possibly edited text as the text as
filed, and it cannot correct that text afterwards, because a later version
must carry a later time than the created_at it would replace. The receipt
counts those beads as edited_same_day. Running the command again records
nothing: a bead whose store already ends with the text is skipped.

The export is read only: git log and git show, never a write.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runMetricsImportVersionsIn(cmd.Context(), cmd.OutOrStdout(), workspace.FindFromCwdOrError,
			metricsImportVersionsRig, metricsImportVersionsPath)
	},
}

// beadVersionsFile is the version store's default path under the town.
func beadVersionsFile(town string) string {
	return filepath.Join(constants.TownRuntimePath(town), "metrics", "bead-versions.jsonl")
}

// runMetricsImportVersionsIn finds the town with find and imports from the real
// archive. find is workspace.FindFromCwdOrError in the command and a fake in the
// test, so the lookup's failure path is driven without a town.
func runMetricsImportVersionsIn(ctx context.Context, out io.Writer, find func() (string, error), rig, path string) error {
	town, err := find()
	if err != nil {
		return err
	}
	return runMetricsImportVersions(ctx, out, importVersionsEnv{
		Town: town,
		Rig:  rig,
		Path: path,
		Run:  metrics.OSRunner(metrics.DefaultAllowEnv),
	})
}

// importVersionsEnv is what the command reads outside its own process: the
// archive through a runner, the town, and the store's path and construction. A
// test passes a fake runner and a fake store, so no git runs in the unit tier
// and a store failure does not depend on the host's permissions.
type importVersionsEnv struct {
	Town string
	Rig  string
	// Path is the store file; empty means beadVersionsFile(Town).
	Path string
	Run  metrics.Runner
	// NewStore builds the store at Path. nil means beadversions.NewFileStore.
	NewStore func(path string) versionStore
}

// versionStore is what the import needs of the store: the Store interface
// plus the file store's load (so an unreadable store fails the run) and its
// count of lines the load left out.
type versionStore interface {
	beadversions.Store
	Load() (skipped int, err error)
	Skipped() int
}

func runMetricsImportVersions(ctx context.Context, out io.Writer, env importVersionsEnv) error {
	if env.Run == nil {
		return fmt.Errorf("import-versions: no runner: the archive is read only through one")
	}
	rig := env.Rig
	if rig == "" {
		rig = "gt"
	}
	if !archiveRigNameRE.MatchString(rig) {
		return fmt.Errorf("import-versions: rig %q is not a database name", rig)
	}
	path := env.Path
	if path == "" {
		path = beadVersionsFile(env.Town)
	}
	repo := filepath.Join(env.Town, ".dolt-archive", "git")
	dropped := &metrics.Dropped{}

	commits, err := archiveSampledCommits(ctx, env.Run, repo, rig, dropped)
	if err != nil {
		return fmt.Errorf("import-versions: %w", err)
	}
	if len(commits) == 0 {
		return fmt.Errorf("import-versions: %s holds no commit for %s/issues.jsonl", repo, rig)
	}
	first, err := archiveFirstFiled(ctx, env.Run, repo, rig, commits, dropped)
	if err != nil {
		return fmt.Errorf("import-versions: %w", err)
	}

	newStore := env.NewStore
	if newStore == nil {
		newStore = func(path string) versionStore { return beadversions.NewFileStore(path) }
	}
	store := newStore(path)
	// Load first, so an unreadable store fails the run here and a damaged line
	// is counted whether or not the import has a row to add.
	if _, err := store.Load(); err != nil {
		return fmt.Errorf("import-versions: reading the version store: %w", err)
	}
	damaged := store.Skipped()

	// A row first carried by a commit on its own filed day may already hold an
	// edit made that day, and the store refuses a time that does not advance,
	// so a wrong text recorded now could never be corrected. Leave those rows
	// out and count them.
	importable := make([]metrics.FirstFiled, 0, len(first))
	sameDay := 0
	for _, f := range first {
		if f.EditedSameDay {
			sameDay++
			continue
		}
		importable = append(importable, f)
	}

	put, skipped, importErr := beadversions.ImportFirstFiled(store, importable, beadversions.FiledText)

	window := commits[0].At.Format(time.DateOnly) + ".." + commits[len(commits)-1].At.Format(time.DateOnly)
	line := fmt.Sprintf(
		"import-versions %s: %d sampled day(s) %s, %d first-filed bead(s), %d edited_same_day (not imported); put %d, skipped %d; %d row(s) dropped, %d damaged store line(s); store %s",
		rig, len(commits), window, len(first), sameDay, put, skipped, dropped.Count(), damaged, path)
	if importErr != nil {
		// The receipt still reports what was recorded before the refusal, so a
		// failed import says how far it got.
		line += fmt.Sprintf("; stopped: %v", importErr)
	}
	if _, err := io.WriteString(out, line+"\n"); err != nil {
		return err
	}
	if importErr != nil {
		return fmt.Errorf("import-versions: %w", importErr)
	}
	return nil
}
