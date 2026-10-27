package cmd

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	landVerdictHead string
	landVerdictJSON bool
)

var landVerdictCmd = &cobra.Command{
	Use:   "verdict <bead>",
	Short: "Print the stored om verdict a landing was decided on",
	Long: `Print the om verdict for a work bead, without re-running om.

Reads the newest verdict the landing worker stored for the bead: om's verdict,
score and summary, then every finding with its severity, path, line, title and
detail. The first 50 findings print, each detail is cut at 4 KiB and each
summary, title or path at 1 KiB; the stored file keeps them whole.

--head <sha> selects one reviewed head by a prefix of at least 7 hex digits.
It fails when the prefix matches more than one head.

--json prints the stored file unchanged.

When the store holds no verdict for the bead, or none for the --head given,
the findings in the bead's MERGE REJECTION notes are printed instead. Those
keep no detail, and the output says so. --json has no such fallback.

The command only reads. It writes nothing and calls no om.

Examples:
  gt land verdict gt-lh0sc
  gt land verdict gt-lh0sc --head ab12ab12ab12
  gt land verdict gt-lh0sc --json`,
	Args: cobra.ExactArgs(1),
	RunE: runLandVerdict,
}

func init() {
	landVerdictCmd.Flags().StringVar(&landVerdictHead, "head", "", "the reviewed head to show, by a prefix of at least 7 hex digits (default: the newest verdict)")
	landVerdictCmd.Flags().BoolVar(&landVerdictJSON, "json", false, "print the stored verdict file unchanged")
	landCmd.AddCommand(landVerdictCmd)
}

func runLandVerdict(cmd *cobra.Command, args []string) error {
	return landVerdictFromCwd(cmd.OutOrStdout(), os.Getwd, realVerdictTown, strings.TrimSpace(args[0]), landVerdictHead, landVerdictJSON)
}

// verdictTown is how gt land verdict finds the town and names the rig that owns
// a bead. The command uses the real lookups; a test passes fakes, so the town
// on the host is never read.
type verdictTown struct {
	find   func(startDir string) (string, error)
	rigFor func(townRoot, beadID string) string
}

var realVerdictTown = verdictTown{find: workspace.FindOrError, rigFor: resolveRigForBead}

// landVerdictFromCwd finds the town from the working directory getwd returns,
// then prints the verdict for beadID. The command passes os.Getwd; a test passes
// a fake that fails.
func landVerdictFromCwd(out io.Writer, getwd func() (string, error), town verdictTown, beadID, head string, asJSON bool) error {
	cwd, err := getwd()
	if err != nil {
		return fmt.Errorf("resolving the working directory: %w", err)
	}
	src, err := resolveVerdictSource(town, cwd, beadID)
	if err != nil {
		return err
	}
	return landVerdict(out, src.store, src.bd, src.rig, beadID, head, asJSON)
}

// verdictSource is what gt land verdict reads: the verdict store under the town,
// the rig that owns the bead, and the bead surface for the notes fallback.
type verdictSource struct {
	store fs.FS
	rig   string
	bd    landVerdictBeads
}

// resolveVerdictSource finds the town that holds startDir and the source of
// beadID's verdict within it.
func resolveVerdictSource(town verdictTown, startDir, beadID string) (verdictSource, error) {
	townRoot, err := town.find(startDir)
	if err != nil {
		return verdictSource{}, fmt.Errorf("resolving the town: %w", err)
	}
	rigName := town.rigFor(townRoot, beadID)
	if rigName == "" {
		return verdictSource{}, fmt.Errorf("cannot resolve the rig that owns %s: its ID prefix maps to no rig", beadID)
	}
	bd := beads.NewWithBeadsDir(startDir, beads.ResolveBeadsDirForID(beads.ResolveBeadsDir(startDir), beadID))
	return verdictSource{store: os.DirFS(land.VerdictsDir(townRoot)), rig: rigName, bd: bd}, nil
}

// landVerdictBeads is the bead surface the notes fallback reads. *beads.Beads
// satisfies it; tests pass the beadsfake.
type landVerdictBeads interface {
	Show(id string) (*beads.Issue, error)
}

// landVerdict prints bead's verdict from store, falling back to its notes when
// the store holds none for the bead or for the head asked for. It only reads:
// store is an io/fs.FS and bd has no write.
func landVerdict(out io.Writer, store fs.FS, bd landVerdictBeads, rig, bead, head string, asJSON bool) error {
	sv, err := land.ReadStoredVerdict(store, rig, bead, head)
	if err == nil {
		if asJSON {
			_, err := out.Write(sv.Raw)
			return err
		}
		return land.RenderStoredVerdict(out, bead, sv)
	}
	noStored := errors.Is(err, land.ErrNoStoredVerdict) || errors.Is(err, land.ErrNoVerdictForHead)
	if !noStored {
		return fmt.Errorf("reading the stored verdict for %s: %w", bead, err)
	}
	if asJSON {
		return fmt.Errorf("--json prints a stored verdict, and %s has none to print: %w", bead, err)
	}
	issue, err := bd.Show(bead)
	if err != nil {
		return fmt.Errorf("reading %s: %w", bead, err)
	}
	if issue == nil {
		return fmt.Errorf("reading %s: no such bead", bead)
	}
	blocks, err := land.NotesRejections(issue.Notes, head)
	if err != nil {
		return fmt.Errorf("selecting the head of %s: %w", bead, err)
	}
	if len(blocks) == 0 {
		return fmt.Errorf("%s has no stored verdict and no MERGE REJECTION notes, so there is nothing to show", bead)
	}
	return land.RenderRejectionNotes(out, bead, blocks)
}
