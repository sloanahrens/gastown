package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/fixrecurrence"
)

// fix_recurrence.go is `gt fix-recurrence scan`: the read-only first step of
// the fix-recurrence signal (gt-1cf7l5). It reads the rig's beads once, asks
// the package for the edges their prose proposes, and prints them. Nothing is
// written, so a scan is safe to run at any time.

// fixRecurrenceListArgs is the scan's one read: every bead in the rig, closed
// beads and the infrastructure beads a citation can name included, with no
// result cap. Its dependencies array is what suppresses a pair an agent
// already joined.
var fixRecurrenceListArgs = []string{"list", "--all", "--limit", "0", "--json", "--include-infra"}

// fixRecurrenceRun runs bd with args in dir and returns its stdout. It is the
// seam a test replaces, so the package's unit tier starts no bd.
type fixRecurrenceRun func(dir string, args []string) ([]byte, error)

// fixRecurrenceDeps is what the scan takes from the host: the rig directory bd
// runs in, the runner, and the clock --since counts back from.
type fixRecurrenceDeps struct {
	workDir func() (string, error)
	run     fixRecurrenceRun
	now     func() time.Time
}

// realFixRecurrenceDeps is fixRecurrenceDeps on this host.
func realFixRecurrenceDeps() fixRecurrenceDeps {
	return fixRecurrenceDeps{workDir: os.Getwd, run: runFixRecurrenceBD, now: time.Now}
}

// runFixRecurrenceBD runs bd in dir with the process environment, leaving
// bd's own .beads resolution to pick the rig's store from the directory.
func runFixRecurrenceBD(dir string, args []string) ([]byte, error) {
	return beads.NewPlain(dir, nil).Run(args...)
}

var fixRecurrenceCmd = &cobra.Command{
	Use:     "fix-recurrence",
	GroupID: GroupDiag,
	Short:   "Propose the dependency edges a bead's prose claims to a closed fix",
	Long: `Read the rig's beads and propose the dependency edges their titles and
descriptions claim: a bead citing a closed bead that closed before it was
created, where no edge already joins the two.`,
	RunE: requireSubcommand,
}

func init() {
	rootCmd.AddCommand(fixRecurrenceCmd)
	fixRecurrenceCmd.AddCommand(newFixRecurrenceScanCmd(realFixRecurrenceDeps()))
}

func newFixRecurrenceScanCmd(d fixRecurrenceDeps) *cobra.Command {
	var since string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "scan",
		Short: "Print the dependency edges the rig's beads propose",
		Long: `Read every bead in the current rig with one bd list and print each
proposed dependency edge: a bead whose title or description cites a closed bead
that closed before the citing bead was created, where neither bead already has
an edge to the other. The relationship comes from the title's classifier, then
the description's, and a title that cites the closed bead with no rule matched
is printed as ambiguous.

--since keeps only the proposals whose citing bead was created in the window
ending now, given as Nd for days or a Go duration such as 36h; without it every
proposal is printed. --json prints one JSON object per proposal on its own
line, and without it one readable line per proposal.

The scan reads only. It writes no bead and never applies an edge; a bead read
that fails is an error, and a bead list that yields no pair prints nothing.

Examples:
  gt fix-recurrence scan
  gt fix-recurrence scan --since 7d
  gt fix-recurrence scan --json`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFixRecurrenceScan(cmd.OutOrStdout(), d, since, asJSON)
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "Keep only proposals whose citing bead was created in this window ending now: Nd or a Go duration (default: all time)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print one JSON object per proposal on its own line")
	return cmd
}

// runFixRecurrenceScan reads the rig's beads, proposes the edges, and prints
// the proposals. A failed read is returned before anything is printed, so a
// partial list is never shown as the rig's answer.
func runFixRecurrenceScan(w io.Writer, d fixRecurrenceDeps, since string, asJSON bool) error {
	from, err := parseSince(since, d.now())
	if err != nil {
		return err
	}
	dir, err := d.workDir()
	if err != nil {
		return fmt.Errorf("finding the rig directory: %w", err)
	}
	raw, err := d.run(dir, fixRecurrenceListArgs)
	if err != nil {
		return fmt.Errorf("reading the rig's beads: %w", err)
	}
	listed, err := fixRecurrenceBeads(raw)
	if err != nil {
		return fmt.Errorf("reading the rig's beads: %w", err)
	}
	proposals := fixRecurrenceSince(fixrecurrence.Propose(listed), listed, from)
	return writeFixRecurrenceProposals(w, proposals, asJSON)
}

// fixRecurrenceRow is one row of bd list --json. Only the fields the pair
// extractor reads are kept: prose, status, the two timestamps, and the
// relation rows.
type fixRecurrenceRow struct {
	ID           string                `json:"id"`
	Title        string                `json:"title"`
	Description  string                `json:"description"`
	Status       string                `json:"status"`
	CreatedAt    string                `json:"created_at"`
	ClosedAt     string                `json:"closed_at"`
	Dependencies []fixRecurrenceDepRow `json:"dependencies"`
}

// fixRecurrenceDepRow is one relation as bd list prints it: a bare row naming
// the target, not the target's fields.
type fixRecurrenceDepRow struct {
	DependsOnID string `json:"depends_on_id"`
}

// fixRecurrenceBeads parses bd list's JSON into the beads Pairs reads. Each
// row's relations become that bead's edges; a relation bd prints on the other
// bead's row still suppresses the pair, because Pairs asks either direction.
func fixRecurrenceBeads(raw []byte) ([]fixrecurrence.Bead, error) {
	var rows []fixRecurrenceRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("bd list output does not parse: %w", err)
	}
	listed := make([]fixrecurrence.Bead, len(rows))
	for i, row := range rows {
		created, err := fixRecurrenceTime(row.ID, "created_at", row.CreatedAt)
		if err != nil {
			return nil, err
		}
		closed, err := fixRecurrenceTime(row.ID, "closed_at", row.ClosedAt)
		if err != nil {
			return nil, err
		}
		edges := make(map[string]bool, len(row.Dependencies))
		for _, dep := range row.Dependencies {
			if dep.DependsOnID != "" {
				edges[dep.DependsOnID] = true
			}
		}
		listed[i] = fixrecurrence.Bead{
			ID: row.ID, Title: row.Title, Description: row.Description,
			Status: row.Status, CreatedAt: created, ClosedAt: closed,
			Edges: edges,
		}
	}
	return listed, nil
}

// fixRecurrenceTime parses one bd timestamp. An empty value is the zero time,
// which every time decision reads as unknown; a value that does not parse
// fails the read rather than standing in as one.
func fixRecurrenceTime(id, field, raw string) (time.Time, error) {
	if raw == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("bead %s: %s is not an RFC3339 time", id, field)
	}
	return t, nil
}

// fixRecurrenceSince keeps the proposals whose citing bead was created at or
// after from. A zero from is all time.
func fixRecurrenceSince(proposals []fixrecurrence.Proposal, listed []fixrecurrence.Bead, from time.Time) []fixrecurrence.Proposal {
	if from.IsZero() {
		return proposals
	}
	created := make(map[string]time.Time, len(listed))
	for _, b := range listed {
		created[b.ID] = b.CreatedAt
	}
	kept := make([]fixrecurrence.Proposal, 0, len(proposals))
	for _, p := range proposals {
		// Propose names a citing bead the list holds; a missing entry reads as
		// the zero time, which is before any window.
		if created[p.New].Before(from) {
			continue
		}
		kept = append(kept, p)
	}
	return kept
}

// fixRecurrenceLine is one proposal's JSON line. Ambiguous marks a proposal no
// classifier decided, whose relationship and confidence carry no claim.
type fixRecurrenceLine struct {
	New          string  `json:"new"`
	Old          string  `json:"old"`
	Relationship string  `json:"relationship"`
	Confidence   float64 `json:"confidence"`
	Source       string  `json:"source"`
	Ambiguous    bool    `json:"ambiguous,omitempty"`
}

// writeFixRecurrenceProposals prints one line per proposal: the JSON object
// under --json, else a readable line. An empty set prints nothing.
func writeFixRecurrenceProposals(w io.Writer, proposals []fixrecurrence.Proposal, asJSON bool) error {
	for _, p := range proposals {
		line := fixRecurrenceLine{
			New: p.New, Old: p.Old, Relationship: string(p.Rel),
			Confidence: p.Confidence, Source: p.Source, Ambiguous: p.Ambiguous,
		}
		if asJSON {
			if err := json.NewEncoder(w).Encode(line); err != nil {
				return fmt.Errorf("writing a proposal: %w", err)
			}
			continue
		}
		var err error
		if p.Ambiguous {
			_, err = fmt.Fprintf(w, "%s -> %s  ambiguous\n", p.New, p.Old)
		} else {
			_, err = fmt.Fprintf(w, "%s -> %s  %s %.2f (%s)\n", p.New, p.Old, p.Rel, p.Confidence, p.Source)
		}
		if err != nil {
			return fmt.Errorf("writing a proposal: %w", err)
		}
	}
	return nil
}
