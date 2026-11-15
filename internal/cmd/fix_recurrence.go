package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/callercwd"
	"github.com/steveyegge/gastown/internal/fixrecurrence"
)

// fix_recurrence.go is `gt fix-recurrence scan`: the first step of the
// fix-recurrence signal (gt-1cf7l5). It reads the rig's beads once, asks the
// package for the edges their prose proposes, and prints them; --apply runs the
// idempotent writes the package plans instead (gt-x9b94p).

// fixRecurrenceListArgs is the scan's one read: every bead in the rig, closed
// beads and the infrastructure beads a citation can name included, with no
// result cap. Its dependencies array is what suppresses a pair an agent
// already joined. "bd" leads the argv because the runner runs the program
// argv[0] names.
var fixRecurrenceListArgs = []string{"bd", "list", "--all", "--limit", "0", "--json", "--include-infra"}

// fixRecurrenceRun runs one program in dir and returns its stdout. argv[0]
// names the program an action's Kind selects -- bd for a dependency or label
// write, gt for an escalation -- and the rest its arguments. It is the seam a
// test replaces, so the package's unit tier starts no process.
type fixRecurrenceRun func(dir string, argv []string) ([]byte, error)

// fixRecurrenceBD runs bd in dir with the arguments after "bd" and returns its
// stdout. It is the seam the scan's read and its bd writes go through, so a
// test drives a failed bd call without starting bd.
type fixRecurrenceBD func(dir string, args []string) ([]byte, error)

// fixRecurrenceDeps is what the scan takes from the host: the rig directory the
// programs run in, the runner, and the clock --since counts back from.
type fixRecurrenceDeps struct {
	workDir func() (string, error)
	run     fixRecurrenceRun
	now     func() time.Time
}

// realFixRecurrenceDeps is fixRecurrenceDeps on this host. The rig directory is
// the caller's, read through callercwd so a scan started elsewhere reads the
// rig's store rather than this process's directory; Require refuses a caller
// path that is not present here, where bd could not find that store.
func realFixRecurrenceDeps() fixRecurrenceDeps {
	return fixRecurrenceDeps{
		workDir: callercwd.Require,
		run:     fixRecurrenceRunProgram(runFixRecurrenceBD, runFixRecurrenceGT),
		now:     time.Now,
	}
}

// runFixRecurrenceBD is bd on this host: beads.NewPlain runs it in dir with the
// process environment, leaving bd's own .beads resolution to pick the rig's
// store from the directory.
func runFixRecurrenceBD(dir string, args []string) ([]byte, error) {
	return beads.NewPlain(dir, nil).Run(args...)
}

// fixRecurrenceRunProgram is the production runner. A program the plan never
// names is refused rather than guessed, and a failed program is reported
// without its captured output.
func fixRecurrenceRunProgram(runBD fixRecurrenceBD, runGT fixRecurrenceRun) fixRecurrenceRun {
	return func(dir string, argv []string) ([]byte, error) {
		if len(argv) == 0 {
			return nil, errors.New("no program to run")
		}
		switch argv[0] {
		case "bd":
			out, err := runBD(dir, argv[1:])
			if err != nil {
				return nil, fixRecurrenceWithheld("bd", argv[1:], err)
			}
			return out, nil
		case "gt":
			out, err := runGT(dir, argv[1:])
			if err != nil {
				return nil, fixRecurrenceWithheld("gt", argv[1:], err)
			}
			return out, nil
		}
		return nil, fmt.Errorf("refusing to run %q: the plan runs only bd and gt", argv[0])
	}
}

// fixRecurrenceWithheld is a failed program's error cut down to what cannot
// hold a credential: the program, its arguments and the exit status. bd's
// captured stderr, which beads.CLIError carries in its message, is dropped.
func fixRecurrenceWithheld(program string, args []string, err error) error {
	line := program + " " + strings.Join(args, " ")
	var exit interface{ ExitCode() int }
	if errors.As(err, &exit) {
		return fmt.Errorf("%s: exit status %d", line, exit.ExitCode())
	}
	return fmt.Errorf("%s: failed with no exit status", line)
}

// runFixRecurrenceGT runs the gt subcommand argv in dir with this binary, so an
// escalation reaches the town from the same gt that planned it.
func runFixRecurrenceGT(dir string, argv []string) ([]byte, error) {
	cmd := exec.Command(fixRecurrenceGTBinary(os.Executable, exec.LookPath), argv...)
	cmd.Dir = dir
	return cmd.Output()
}

// fixRecurrenceGTBinary is the binary a gt write runs: this binary when
// executable can name it, else gt from PATH when lookPath finds it, else the
// PATH name, which leaves the run to report what failed.
func fixRecurrenceGTBinary(executable func() (string, error), lookPath func(string) (string, error)) string {
	if exe, err := executable(); err == nil {
		return exe
	}
	if path, err := lookPath("gt"); err == nil {
		return path
	}
	return "gt"
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
	var apply bool
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

--apply runs the plan instead of printing it: each dependency edge, label and
chronic escalation the proposals earn is written once, in plan order, through
bd or gt, and the run stops at the first write that fails, returning its error.
A write the store already carries is planned only once, so a run that follows a
complete one writes nothing. --since still selects the proposals; --json formats
the dry run's lines only.

Without --apply the scan reads only: it writes no bead and applies no edge. A
bead read that fails is an error, a bead row bd reports without a creation time
is an error, and a bead list that yields no pair prints nothing.

Examples:
  gt fix-recurrence scan
  gt fix-recurrence scan --since 7d
  gt fix-recurrence scan --json
  gt fix-recurrence scan --apply --since 7d`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runFixRecurrenceScan(cmd.OutOrStdout(), d, since, asJSON, apply)
		},
	}
	cmd.Flags().StringVar(&since, "since", "", "Keep only proposals whose citing bead was created in this window ending now: Nd or a Go duration (default: all time)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Print one JSON object per proposal on its own line")
	cmd.Flags().BoolVar(&apply, "apply", false, "Run the plan: write each proposed edge, label and chronic escalation through bd or gt")
	return cmd
}

// runFixRecurrenceScan reads the rig's beads, proposes the edges, and prints
// the proposals or runs the plan they earn. A failed read is returned before
// anything is printed or written, so a partial list is never taken as the rig's
// answer.
func runFixRecurrenceScan(w io.Writer, d fixRecurrenceDeps, since string, asJSON, apply bool) error {
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
	listed, state, err := fixRecurrenceBeads(raw)
	if err != nil {
		return fmt.Errorf("reading the rig's beads: %w", err)
	}
	proposals := fixRecurrenceSince(fixrecurrence.Propose(listed), listed, from)
	if !apply {
		return writeFixRecurrenceProposals(w, proposals, asJSON)
	}
	return applyFixRecurrenceActions(w, d, dir, fixrecurrence.Plan(proposals, state))
}

// applyFixRecurrenceActions runs actions in plan order through the runner and
// reports each write it made. The first failure is returned with the action's
// Kind, not its captured output, which can hold a credential; the run stops
// there so a reader sees the write that failed, and a later run resumes from
// the state the earlier writes left.
func applyFixRecurrenceActions(w io.Writer, d fixRecurrenceDeps, dir string, actions []fixrecurrence.Action) error {
	for _, a := range actions {
		program, err := fixRecurrenceProgram(a.Kind)
		if err != nil {
			return err
		}
		argv := append([]string{program}, a.Args...)
		if _, err := d.run(dir, argv); err != nil {
			return fmt.Errorf("%s: %w", a.Kind, err)
		}
		if _, err := fmt.Fprintf(w, "did %s: %s\n", a.Kind, strings.Join(argv, " ")); err != nil {
			return fmt.Errorf("reporting the %s write: %w", a.Kind, err)
		}
	}
	return nil
}

// fixRecurrenceProgram is the binary a planned write's Kind runs: bd for the
// dependency and label writes, gt for the chronic escalation. A Kind Plan does
// not document is refused rather than guessed.
func fixRecurrenceProgram(kind string) (string, error) {
	switch kind {
	case fixrecurrence.KindDepAdd, fixrecurrence.KindLabelAdd:
		return "bd", nil
	case fixrecurrence.KindEscalate:
		return "gt", nil
	}
	return "", fmt.Errorf("no program runs a %q write", kind)
}

// fixRecurrenceRow is one row of bd list --json. Only the fields the pair
// extractor and the plan read are kept: prose, status, the two timestamps, the
// labels, and the relation rows.
type fixRecurrenceRow struct {
	ID           string                `json:"id"`
	Title        string                `json:"title"`
	Description  string                `json:"description"`
	Status       string                `json:"status"`
	CreatedAt    string                `json:"created_at"`
	ClosedAt     string                `json:"closed_at"`
	Labels       []string              `json:"labels"`
	Dependencies []fixRecurrenceDepRow `json:"dependencies"`
}

// fixRecurrenceDepRow is one relation as bd list prints it: the target and the
// bd dependency type the store records for the edge.
type fixRecurrenceDepRow struct {
	DependsOnID string `json:"depends_on_id"`
	Type        string `json:"type"`
}

// fixRecurrenceBeads parses bd list's JSON into the beads Pairs reads and the
// state Plan reads, both from the same rows. Each row's relations become that
// bead's edges; a relation bd prints on the other bead's row still suppresses
// the pair, because Pairs asks either direction.
func fixRecurrenceBeads(raw []byte) ([]fixrecurrence.Bead, fixrecurrence.State, error) {
	var rows []fixRecurrenceRow
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fixrecurrence.State{}, fmt.Errorf("bd list output does not parse: %w", err)
	}
	listed := make([]fixrecurrence.Bead, len(rows))
	for i, row := range rows {
		created, err := fixRecurrenceTime(row.ID, "created_at", row.CreatedAt)
		if err != nil {
			return nil, fixrecurrence.State{}, err
		}
		// Every pair is dated by the citing bead's creation, so a bead without
		// one cannot be judged and would vanish from the signal. Refuse the
		// read instead of reading it as a bead that cites nothing.
		if created.IsZero() {
			return nil, fixrecurrence.State{}, fmt.Errorf("bead %s: created_at is missing, so its citations cannot be dated", row.ID)
		}
		closed, err := fixRecurrenceTime(row.ID, "closed_at", row.ClosedAt)
		if err != nil {
			return nil, fixrecurrence.State{}, err
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
	return listed, fixRecurrenceState(rows), nil
}

// fixRecurrenceState is what the store already holds for the rows' beads: each
// bead's labels and the edges it carries to another bead, with the relationship
// the recorded bd type asserts. Plan reads it to skip a write the store
// already carries and to count the claims already made against a fix.
func fixRecurrenceState(rows []fixRecurrenceRow) fixrecurrence.State {
	state := fixrecurrence.State{
		Deps:   make(map[fixrecurrence.Edge]bool),
		Labels: make(map[string]map[string]bool),
	}
	for _, row := range rows {
		if len(row.Labels) > 0 {
			labels := make(map[string]bool, len(row.Labels))
			for _, label := range row.Labels {
				labels[label] = true
			}
			state.Labels[row.ID] = labels
		}
		for _, dep := range row.Dependencies {
			rel, ok := fixRecurrenceRelationship(dep.Type)
			if !ok || dep.DependsOnID == "" {
				continue
			}
			state.Deps[fixrecurrence.Edge{New: row.ID, Old: dep.DependsOnID, Rel: rel}] = true
		}
	}
	return state
}

// fixRecurrenceRelationship is the relationship a stored bd dependency type
// asserts: bd keeps one "related" type for both a recurrence and a same-class
// claim, and the other two types are its own names. Any other type, such as
// blocks or parent-child, says nothing about a fix, so the state leaves it out.
func fixRecurrenceRelationship(depType string) (fixrecurrence.Relationship, bool) {
	switch depType {
	case "related":
		return fixrecurrence.Recurrence, true
	case "caused-by":
		return fixrecurrence.Regression, true
	case "discovered-from":
		return fixrecurrence.FollowUp, true
	}
	return "", false
}

// fixRecurrenceTime parses one bd timestamp. An empty value is the zero time,
// which the caller reads as unknown -- a missing close is not a close, and a
// missing creation fails the read -- and a value that does not parse fails the
// read rather than standing in as one.
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
// after from. A zero from is all time. The read refuses a bead whose created_at
// is missing, and Propose names a listed bead, so every proposal's citing bead
// is in created with a known time: no proposal is left out because its time
// could not be read.
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
