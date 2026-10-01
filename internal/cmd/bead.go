package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// bead.go is `gt bead`, the agent's write surface on beads (deep review D1
// rule 6, gt-7iwy0.8). Agent prose may name only read-only bd commands; every
// write an agent makes goes through one of these verbs, so gt holds the
// invariants bd alone cannot: cross-rig filing through the rig's own
// database (never bd --repo, be-6mk), an atomic claim, no reset of work
// submitted for landing, and the gt-6hmz close-time invariant on the bead a
// polecat branch was cut for.

var beadCmd = &cobra.Command{
	Use:     "bead",
	GroupID: GroupWork,
	Short:   "File, edit, claim, comment on and close beads",
	Long: `Write verbs on beads, for agents and humans alike.

Read with 'gt show' or 'bd show'; write with these:

  gt bead create "title" --type=bug    File a bead (--rig <rig> files in another rig)
  gt bead note <id> "findings"         Append to the bead's notes
  gt bead update <id> --title=...      Edit title, priority or labels
  gt bead claim <id>                   Claim a bead (in_progress, assigned to you)
  gt bead reset <id> --reason=...      Return an orphaned claim to open
  gt bead comment <id> "text"          Add a comment
  gt bead dep add <issue> <needs>      <issue> depends on <needs>
  gt bead dep remove <issue> <needs>   Drop that dependency
  gt bead close <id> --reason=...      Close beads
  gt bead reopen <id>...               Return beads to open`,
	RunE: requireSubcommand,
}

var (
	beadCreateType     string
	beadCreatePriority int
	beadCreateLabels   []string
	beadCreateDesc     string
	beadCreateParent   string
	beadCreateRig      string
	beadCreateTitle    string
	beadCreateQuiet    bool
	beadCreateJSON     bool

	beadUpdateTitle       string
	beadUpdatePriority    int
	beadUpdateAddLabels   []string
	beadUpdateRemoveLabel []string

	beadResetReason string
	beadCloseReason string
)

var beadCreateCmd = &cobra.Command{
	Use:   "create [title]",
	Short: "File a bead",
	Long: `File a bead in this rig's database, or in another rig's with --rig.

--rig names a rig (or hq for town beads) and files through that rig's own
database: never cd elsewhere or pass bd --repo (be-6mk). --type records the
kind as a gt:<type> label.

Examples:
  gt bead create "Found: nil map in sling" --type=bug --priority=2
  gt bead create --rig=beads "bd CLI bug: ..." --type=bug
  gt bead create "Fix-merge PR #1234" -l pr-review -q   # print only the ID`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		title := beadCreateTitle
		if len(args) == 1 {
			if title != "" {
				return fmt.Errorf("give the title once: as an argument or --title")
			}
			title = args[0]
		}
		return newBeadVerbs(cmd).create(beadCreateRequest{
			title: title, kind: beadCreateType, priority: beadCreatePriority,
			labels: beadCreateLabels, description: beadCreateDesc,
			parent: beadCreateParent, rig: beadCreateRig,
			quiet: beadCreateQuiet, json: beadCreateJSON,
		})
	},
}

var beadNoteCmd = &cobra.Command{
	Use:   "note <id> <text>",
	Short: "Append to a bead's notes",
	Long: `Append text to a bead's notes, on a new line after any notes it has.

Notes survive your session: persist findings there as you go.

Example:
  gt bead note gt-abc "Findings so far: the retry loop never resets"`,
	Args: cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).note(args[0], args[1])
	},
}

var beadUpdateCmd = &cobra.Command{
	Use:   "update <id>",
	Short: "Edit a bead's title, priority or labels",
	Long: `Edit a bead's title, priority or labels.

Status moves through the other verbs: claim, reset, close, reopen. Notes
append through 'gt bead note'.

Examples:
  gt bead update gt-abc --title="DISPROVEN: <claim>; <what shipped>"
  gt bead update gt-abc --add-label=needs-review --priority=1`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		var opts beads.UpdateOptions
		if cmd.Flags().Changed("title") {
			opts.Title = &beadUpdateTitle
		}
		if cmd.Flags().Changed("priority") {
			opts.Priority = &beadUpdatePriority
		}
		opts.AddLabels = beadUpdateAddLabels
		opts.RemoveLabels = beadUpdateRemoveLabel
		return newBeadVerbs(cmd).update(args[0], opts)
	},
}

var beadClaimCmd = &cobra.Command{
	Use:   "claim <id>",
	Short: "Claim a bead: in_progress, assigned to you",
	Long: `Claim a bead: set it in_progress and assign it to you, atomically.

A bead that is unassigned, or already yours, is claimed. One held by another
agent is refused.`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).claim(args[0])
	},
}

var beadResetCmd = &cobra.Command{
	Use:   "reset <id>",
	Short: "Return an orphaned claim to open",
	Long: `Return a bead whose holder is gone to open with no assignee, ready for
dispatch. --reason is appended to its notes.

The reset applies only while the holder read at the start still holds the
bead, and never to work labeled gt:ready-to-land: the landing worker owns
that until it lands or hands it back.

Example:
  gt bead reset gt-abc --reason="Orphaned with partial work on <branch>"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).reset(args[0], beadResetReason)
	},
}

var beadCommentCmd = &cobra.Command{
	Use:   "comment <id> <text>",
	Short: "Add a comment to a bead",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).comment(args[0], args[1])
	},
}

var beadDepCmd = &cobra.Command{
	Use:   "dep",
	Short: "Add or remove a dependency between beads",
	Long: `Add or remove a dependency. "gt bead dep add A B" means A depends on
(is blocked by) B: name the requirement, "A needs B", not the order.`,
	RunE: requireSubcommand,
}

var beadDepAddCmd = &cobra.Command{
	Use:   "add <issue> <depends-on>",
	Short: "Make <issue> depend on <depends-on>",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).depAdd(args[0], args[1])
	},
}

var beadDepRemoveCmd = &cobra.Command{
	Use:   "remove <issue> <depends-on>",
	Short: "Remove the dependency of <issue> on <depends-on>",
	Args:  cobra.ExactArgs(2),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).depRemove(args[0], args[1])
	},
}

var beadCloseCmd = &cobra.Command{
	Use:   "close <id>...",
	Short: "Close beads",
	Long: `Close beads, recording --reason.

The bead your polecat branch was cut for closes here only while the branch
carries no unlanded commits (or with a supersede:/cancel: reason): submit
landed work with 'gt done' instead.

Examples:
  gt bead close gt-abc.2 --reason="sub-issue done"
  gt bead close gt-abc --reason="no-changes: already fixed by gt-xyz"`,
	Args: cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).close(beadCloseReason, args...)
	},
}

var beadReopenCmd = &cobra.Command{
	Use:   "reopen <id>...",
	Short: "Return beads to open",
	Args:  cobra.MinimumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).reopen(args...)
	},
}

func init() {
	f := beadCreateCmd.Flags()
	f.StringVar(&beadCreateTitle, "title", "", "Title (or give it as the argument)")
	f.StringVarP(&beadCreateType, "type", "t", "", "Kind: task, bug, feature, epic (recorded as a gt:<type> label)")
	f.IntVarP(&beadCreatePriority, "priority", "p", -1, "Priority 0-4 (default: bd's, 2)")
	f.StringArrayVarP(&beadCreateLabels, "label", "l", nil, "Label to add (repeatable)")
	f.StringVarP(&beadCreateDesc, "description", "d", "", "Description")
	f.StringVar(&beadCreateParent, "parent", "", "Parent bead: the new bead becomes its child")
	f.StringVar(&beadCreateRig, "rig", "", "File in this rig's database (hq for town beads)")
	f.BoolVarP(&beadCreateQuiet, "quiet", "q", false, "Print only the new bead's ID")
	f.BoolVar(&beadCreateJSON, "json", false, "Print the new bead as JSON")

	f = beadUpdateCmd.Flags()
	f.StringVar(&beadUpdateTitle, "title", "", "New title")
	f.IntVarP(&beadUpdatePriority, "priority", "p", 0, "New priority 0-4")
	f.StringArrayVar(&beadUpdateAddLabels, "add-label", nil, "Label to add (repeatable)")
	f.StringArrayVar(&beadUpdateRemoveLabel, "remove-label", nil, "Label to remove (repeatable)")

	beadResetCmd.Flags().StringVar(&beadResetReason, "reason", "", "Why: appended to the bead's notes")
	beadCloseCmd.Flags().StringVarP(&beadCloseReason, "reason", "r", "", "Close reason")

	beadDepCmd.AddCommand(beadDepAddCmd, beadDepRemoveCmd)
	beadCmd.AddCommand(beadCreateCmd, beadNoteCmd, beadUpdateCmd, beadClaimCmd, beadResetCmd,
		beadCommentCmd, beadDepCmd, beadCloseCmd, beadReopenCmd)
	rootCmd.AddCommand(beadCmd)
}

// beadVerbs is what the gt bead verbs act through: the beads client, the
// actor a claim assigns, where they report, and the close-time invariant.
type beadVerbs struct {
	client beads.Client
	actor  string
	out    io.Writer
	// closeRefusal returns why closing id with reason would break the
	// gt-6hmz invariant, or "" when the close may proceed.
	closeRefusal func(id, reason string) string
}

// newBeadVerbs builds the verbs for this session: a client on the current
// directory's database that routes each ID by its prefix, and the session's
// actor (BD_ACTOR, else the role gt detects).
func newBeadVerbs(cmd *cobra.Command) *beadVerbs {
	cwd, _ := os.Getwd()
	actor := os.Getenv("BD_ACTOR")
	if actor == "" {
		if a := detectActor(); a != "unknown" {
			actor = a
		}
	}
	return &beadVerbs{
		client:       beads.New(cwd),
		actor:        actor,
		out:          cmd.OutOrStdout(),
		closeRefusal: sessionCloseRefusal(cwd),
	}
}

// sessionCloseRefusal is the guard bd-close-invariant applies to a raw
// bd close, for this session: only the bead the current branch was cut for
// is judged, and an unresolvable session allows the close.
func sessionCloseRefusal(cwd string) func(id, reason string) string {
	return func(id, reason string) string {
		scope, ok := resolveBdCloseInvariantScope(cwd, realGuardProcess())
		if !ok || !branchNamesBead(scope.branch, id) {
			return ""
		}
		return bdCloseInvariantRefusal(scope, id, reason)
	}
}

type beadCreateRequest struct {
	title, kind, description, parent, rig string
	priority                              int
	labels                                []string
	quiet, json                           bool
}

func (v *beadVerbs) create(r beadCreateRequest) error {
	if strings.TrimSpace(r.title) == "" {
		return fmt.Errorf("a title is required")
	}
	labels := append([]string(nil), r.labels...)
	if r.kind != "" {
		labels = append(labels, "gt:"+r.kind)
	}
	is, err := v.client.Create(beads.CreateOptions{
		Title: r.title, Labels: labels, Priority: r.priority,
		Description: r.description, Parent: r.parent, Rig: r.rig, Actor: v.actor,
	})
	if err != nil {
		return err
	}
	switch {
	case r.json:
		enc := json.NewEncoder(v.out)
		enc.SetIndent("", "  ")
		return enc.Encode(is)
	case r.quiet:
		_, err = fmt.Fprintln(v.out, is.ID)
	default:
		_, err = fmt.Fprintf(v.out, "✓ Created %s: %s\n", is.ID, is.Title)
	}
	return err
}

func (v *beadVerbs) note(id, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("nothing to note: the text is empty")
	}
	if err := v.client.AppendNotes(id, text); err != nil {
		return err
	}
	_, err := fmt.Fprintf(v.out, "✓ Noted on %s\n", id)
	return err
}

func (v *beadVerbs) update(id string, opts beads.UpdateOptions) error {
	if opts.Title == nil && opts.Priority == nil && len(opts.AddLabels) == 0 && len(opts.RemoveLabels) == 0 {
		return fmt.Errorf("nothing to update: give --title, --priority, --add-label or --remove-label")
	}
	if opts.Title != nil && strings.TrimSpace(*opts.Title) == "" {
		return fmt.Errorf("--title is empty")
	}
	if err := v.client.Update(id, opts); err != nil {
		return err
	}
	_, err := fmt.Fprintf(v.out, "✓ Updated %s\n", id)
	return err
}

func (v *beadVerbs) claim(id string) error {
	if v.actor == "" {
		return fmt.Errorf("cannot claim %s: no actor (set BD_ACTOR or run in an agent session)", id)
	}
	is, err := v.client.Show(id)
	if err != nil {
		return err
	}
	if is.Status == string(beads.StatusClosed) {
		return fmt.Errorf("cannot claim %s: it is closed", id)
	}
	if is.Assignee != "" && is.Assignee != v.actor {
		return fmt.Errorf("cannot claim %s: %s holds it", id, is.Assignee)
	}
	ok, err := v.client.TransferIfAssignee(id, is.Assignee, string(beads.StatusInProgress), v.actor)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("cannot claim %s: someone claimed it first", id)
	}
	_, err = fmt.Fprintf(v.out, "✓ Claimed %s\n", id)
	return err
}

func (v *beadVerbs) reset(id, reason string) error {
	is, err := v.client.Show(id)
	if err != nil {
		return err
	}
	for _, l := range is.Labels {
		if l == land.LabelReadyToLand {
			return fmt.Errorf("not resetting %s: it is labeled %s, and the landing worker owns it", id, land.LabelReadyToLand)
		}
	}
	if is.Status == string(beads.StatusClosed) {
		return fmt.Errorf("not resetting %s: it is closed (use gt bead reopen)", id)
	}
	ok, err := v.client.ReleaseIfAssignee(id, is.Assignee)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("not resetting %s: its claim changed since it was read (was %q)", id, is.Assignee)
	}
	if err := v.client.AppendNotes(id, reason); err != nil {
		return fmt.Errorf("%s reset to open, but recording the reason failed: %w", id, err)
	}
	_, err = fmt.Fprintf(v.out, "✓ Reset %s to open\n", id)
	return err
}

func (v *beadVerbs) comment(id, text string) error {
	if strings.TrimSpace(text) == "" {
		return fmt.Errorf("nothing to comment: the text is empty")
	}
	if err := v.client.AddComment(id, text); err != nil {
		return err
	}
	_, err := fmt.Fprintf(v.out, "✓ Commented on %s\n", id)
	return err
}

func (v *beadVerbs) depAdd(issue, dependsOn string) error {
	if issue == dependsOn {
		return fmt.Errorf("%s cannot depend on itself", issue)
	}
	if err := v.client.AddDependency(issue, dependsOn); err != nil {
		return err
	}
	_, err := fmt.Fprintf(v.out, "✓ %s now depends on %s\n", issue, dependsOn)
	return err
}

func (v *beadVerbs) depRemove(issue, dependsOn string) error {
	if err := v.client.RemoveDependency(issue, dependsOn); err != nil {
		return err
	}
	_, err := fmt.Fprintf(v.out, "✓ %s no longer depends on %s\n", issue, dependsOn)
	return err
}

func (v *beadVerbs) close(reason string, ids ...string) error {
	for _, id := range ids {
		if refusal := v.closeRefusal(id, reason); refusal != "" {
			return fmt.Errorf("not closing %s: %s", id, refusal)
		}
	}
	var err error
	if reason == "" {
		err = v.client.Close(ids...)
	} else {
		err = v.client.CloseWithReason(reason, ids...)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(v.out, "✓ Closed %s\n", strings.Join(ids, " "))
	return err
}

func (v *beadVerbs) reopen(ids ...string) error {
	open := string(beads.StatusOpen)
	for _, id := range ids {
		if err := v.client.Update(id, beads.UpdateOptions{Status: &open}); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(v.out, "✓ Reopened %s\n", strings.Join(ids, " "))
	return err
}
