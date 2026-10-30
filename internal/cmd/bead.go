package cmd

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/style"
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

  gt bead create "title"               File a bead (--rig <rig> files in another rig)
  gt bead note <id> "findings"         Append to the bead's notes
  gt bead update <id> --title=...      Edit title, priority, labels, acceptance
  gt bead claim <id>                   Claim a bead (in_progress, assigned to you)
  gt bead reset <id> --reason=...      Return an orphaned claim to open
  gt bead comment <id> "text"          Add a comment
  gt bead review <id> PASS|FAIL        Record an overseer review of its landed head
  gt bead review <id> FAIL --finding=<id>
                                       Fail it, naming the finding bead it broke
  gt bead review <id> WAIVED|AUDITED --reason=...
                                       Clear its risk-path item without a PASS
  gt bead dep add <issue> <needs>      <issue> depends on <needs>
  gt bead dep remove <issue> <needs>   Drop that dependency
  gt bead close <id> --reason=...      Close beads
  gt bead reopen <id>...               Return beads to open

Read across every rig with 'gt bead search':

  gt bead search <words...>            Search all rigs' beads, grouped by rig`,
	RunE: requireSubcommand,
}

var (
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

--acceptance sets the criteria block on the new bead, so a follow-up that
needs one is a single command rather than create-then-update.

Before filing discovered work, copy the five-section shape in the
mol-polecat-work formula's Discovered work step — run
'gt prime --step 3 --formula mol-polecat-work' to print it in full — so the
spec dispatcher slots the bead instead of holding it unshaped.

Examples:
  gt bead create "Add --acceptance" --acceptance="- [ ] the flag sets the field"
  gt bead create --rig=beads "bd CLI: slow query on list" -q   # file in another rig
  gt bead create "Fix-merge PR #1234" -l pr-review -q   # print only the ID`,
	Args: cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		r, err := beadCreateRequestFrom(cmd.Flags(), args)
		if err != nil {
			return err
		}
		return newBeadVerbs(cmd).create(r)
	},
}

// beadCreateRequestFrom maps the create verb's flags and title argument to
// the request it acts on. acceptanceSet records an explicitly given
// --acceptance, so create can refuse a blank one the store would drop.
func beadCreateRequestFrom(f *pflag.FlagSet, args []string) (beadCreateRequest, error) {
	title, _ := f.GetString("title")
	if len(args) == 1 {
		if title != "" {
			return beadCreateRequest{}, fmt.Errorf("give the title once: as an argument or --title")
		}
		title = args[0]
	}
	kind, _ := f.GetString("type")
	priority, _ := f.GetInt("priority")
	labels, _ := f.GetStringArray("label")
	description, _ := f.GetString("description")
	acceptance, _ := f.GetString("acceptance")
	parent, _ := f.GetString("parent")
	rig, _ := f.GetString("rig")
	quiet, _ := f.GetBool("quiet")
	asJSON, _ := f.GetBool("json")
	return beadCreateRequest{
		title: title, kind: kind, priority: priority,
		labels: labels, description: description,
		acceptance: acceptance, acceptanceSet: f.Changed("acceptance"),
		parent: parent, rig: rig,
		quiet: quiet, json: asJSON,
	}, nil
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
	Short: "Edit a bead's title, priority, labels or acceptance criteria",
	Long: `Edit a bead's title, priority, labels or acceptance criteria.

Status moves through the other verbs: claim, reset, close, reopen. Notes
append through 'gt bead note'.

--acceptance takes the whole criteria block, replacing what the bead had:
bd show prints the current text, and you resend it with '- [x]' on each line
your work satisfies. gt done refuses a submit while any '- [ ]' line is left.

Examples:
  gt bead update gt-abc --title="DISPROVEN: <claim>; <what shipped>"
  gt bead update gt-abc --add-label=needs-review --priority=1
  gt bead update gt-abc --acceptance="- [x] the refusal names the verb"`,
	Args: cobra.ExactArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		return newBeadVerbs(cmd).update(args[0], beadUpdateOptions(cmd.Flags()))
	},
}

// beadUpdateOptions maps the update verb's flags to the store options. A flag
// that was not given stays nil, so an omitted --acceptance leaves the
// criteria alone while an explicitly blank one is refused (gt-9smgr).
func beadUpdateOptions(f *pflag.FlagSet) beads.UpdateOptions {
	var opts beads.UpdateOptions
	if f.Changed("title") {
		title, _ := f.GetString("title")
		opts.Title = &title
	}
	if f.Changed("priority") {
		priority, _ := f.GetInt("priority")
		opts.Priority = &priority
	}
	if f.Changed("acceptance") {
		acceptance, _ := f.GetString("acceptance")
		opts.Acceptance = &acceptance
	}
	opts.AddLabels, _ = f.GetStringArray("add-label")
	opts.RemoveLabels, _ = f.GetStringArray("remove-label")
	// The pairs go to bd as written: it reads "key=value" itself and refuses a
	// pair it cannot, which is one rule for the flag and for the verbs.
	opts.SetMetadata, _ = f.GetStringArray("set-metadata")
	opts.UnsetMetadata, _ = f.GetStringArray("unset-metadata")
	return opts
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

// beadCreateFlags registers the create verb's flags. The test that checks the
// flag-to-request mapping registers them through this function too, so the
// two cannot drift.
func beadCreateFlags(f *pflag.FlagSet) {
	f.String("title", "", "Title (or give it as the argument)")
	f.StringP("type", "t", "", "Kind: task, bug, feature, epic (recorded as a gt:<type> label)")
	f.IntP("priority", "p", -1, "Priority 0-4 (default: bd's, 2)")
	f.StringArrayP("label", "l", nil, "Label to add (repeatable)")
	f.StringP("description", "d", "", "Description")
	f.String("acceptance", "", "Acceptance criteria block for the new bead")
	f.String("parent", "", "Parent bead: the new bead becomes its child")
	f.String("rig", "", "File in this rig's database (hq for town beads)")
	f.BoolP("quiet", "q", false, "Print only the new bead's ID")
	f.Bool("json", false, "Print the new bead as JSON")
}

// beadUpdateFlags registers the update verb's flags; the mapping test uses it
// for the same reason beadCreateFlags exists.
func beadUpdateFlags(f *pflag.FlagSet) {
	f.String("title", "", "New title")
	f.IntP("priority", "p", 0, "New priority 0-4")
	f.StringArray("add-label", nil, "Label to add (repeatable)")
	f.StringArray("remove-label", nil, "Label to remove (repeatable)")
	f.StringArray("set-metadata", nil, "Metadata key=value to set (repeatable)")
	f.StringArray("unset-metadata", nil, "Metadata key to remove (repeatable)")
	f.String("acceptance", "", "New acceptance criteria, the whole block")
}

func init() {
	beadCreateFlags(beadCreateCmd.Flags())
	beadUpdateFlags(beadUpdateCmd.Flags())

	beadResetCmd.Flags().StringVar(&beadResetReason, "reason", "", "Why: appended to the bead's notes")
	beadCloseCmd.Flags().StringVarP(&beadCloseReason, "reason", "r", "", "Close reason")
	beadReviewCmd.Flags().StringVar(&beadReviewSHA, "sha", "", "Review this landing of the bead instead of its newest")
	beadReviewCmd.Flags().StringVar(&beadReviewReason, "reason", "", "Why the head is waived or audited (required for WAIVED and AUDITED)")
	beadReviewCmd.Flags().StringSliceVar(&beadReviewFindings, "finding", nil, "Finding bead a FAIL covers (repeatable; required for FAIL)")

	beadDepCmd.AddCommand(beadDepAddCmd, beadDepRemoveCmd)
	beadCmd.AddCommand(beadCreateCmd, beadNoteCmd, beadUpdateCmd, beadClaimCmd, beadResetCmd,
		beadCommentCmd, beadReviewCmd, beadDepCmd, beadCloseCmd, beadReopenCmd, beadSearchCmd)
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
	// reviewHead returns the landed head an overseer review of id names, from
	// the bead's landing record (the reader gt bead review shares with the
	// attention queue's risk-path item).
	reviewHead func(id, sha string) (string, error)
	// recordClosed logs the run end (events.TypeBeadClosed) of each bead a close
	// closed, so a bead closed by hand and dispatched again later starts its
	// clock over instead of being dated from the dispatch of the run that was
	// closed (events.DispatchStart). nil records nothing.
	recordClosed func(ids []string)
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
		reviewHead: func(id, sha string) (string, error) {
			townRoot := beads.FindTownRoot(cwd)
			if townRoot == "" {
				return "", fmt.Errorf("cannot read the landing record of %s: no town root above %s", id, cwd)
			}
			return landedOverseerHead(townRoot, id, sha)
		},
		recordClosed: func(ids []string) { logClosedBeads(beads.FindTownRoot(cwd), actor, ids) },
	}
}

// logClosedBeads records the run end of each bead a close closed. An event that
// cannot be written is a warning, not a failed close: the close the caller
// asked for already happened, and the next dispatch of the bead falls back to
// the agent bead's time (which can only read short) like a bead the log never
// dispatched.
func logClosedBeads(townRoot, actor string, ids []string) {
	if townRoot == "" {
		return
	}
	for _, id := range ids {
		if err := events.LogAuditTo(townRoot, events.TypeBeadClosed, actor,
			map[string]interface{}{"bead": id}); err != nil {
			style.PrintWarning("could not record the close of %s on the town log: %v", id, err)
		}
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
	title, kind, description, acceptance, parent, rig string
	priority                                          int
	labels                                            []string
	quiet, json                                       bool
	// acceptanceSet records an explicitly given --acceptance, which create
	// refuses when blank rather than letting the store drop it.
	acceptanceSet bool
}

func (v *beadVerbs) create(r beadCreateRequest) error {
	if strings.TrimSpace(r.title) == "" {
		return fmt.Errorf("a title is required")
	}
	// The other gt verb that writes acceptance is update, which refuses a
	// blank block the same way. Here a blank block would leave the store
	// field empty, so the criteria would look absent rather than deliberately
	// unset (gt-aht2p).
	if r.acceptanceSet && strings.TrimSpace(r.acceptance) == "" {
		return fmt.Errorf("--acceptance is empty: the new bead would carry no acceptance criteria")
	}
	labels := append([]string(nil), r.labels...)
	if r.kind != "" {
		labels = append(labels, "gt:"+r.kind)
	}
	is, err := v.client.Create(beads.CreateOptions{
		Title: r.title, Labels: labels, Priority: r.priority,
		Description: r.description, Acceptance: r.acceptance,
		Parent: r.parent, Rig: r.rig, Actor: v.actor,
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
	if opts.Title == nil && opts.Priority == nil && opts.Acceptance == nil &&
		len(opts.AddLabels) == 0 && len(opts.RemoveLabels) == 0 {
		return fmt.Errorf("nothing to update: give --title, --priority, --acceptance, --add-label or --remove-label")
	}
	if opts.Title != nil && strings.TrimSpace(*opts.Title) == "" {
		return fmt.Errorf("--title is empty")
	}
	// A blank block would replace the criteria with nothing, and gt done
	// reads zero unchecked boxes as satisfied (gt-9smgr).
	if opts.Acceptance != nil && strings.TrimSpace(*opts.Acceptance) == "" {
		return fmt.Errorf("--acceptance is empty: the acceptance criteria were not changed")
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
	// A closed work bead ends its molecule with it. This is the only path that
	// ends the bead for a session that closes its own to report no-changes and
	// then exits without a landing (`gt bead close --reason="no-changes: ..."`),
	// and nothing after it ends the molecule: gt done's close block needs a
	// COMPLETED or ESCALATED exit, and a landed bead's molecule is the landing's
	// (gt-x1d8s).
	closed := beads.ClosedIDs(ids, err)
	v.endMoleculesOf(closed)
	// The close also ends the bead's run on the town's clock, so the dispatch
	// that follows a reopen is policed from its own start (gt-tzbtf).
	if v.recordClosed != nil {
		v.recordClosed(closed)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(v.out, "✓ Closed %s\n", strings.Join(ids, " "))
	return err
}

// endMoleculesOf ends the workflow each of ids carries in its attached_molecule
// field. Only the beads a close actually closed arrive here, and only a bead
// carrying an attachment acts, so a plain close is unchanged. Failures are
// warnings: the close the caller asked for already happened.
func (v *beadVerbs) endMoleculesOf(ids []string) {
	for _, id := range ids {
		is, err := v.client.Show(id)
		if err != nil {
			style.PrintWarning("reading %s to end its molecule: %v", id, err)
			continue
		}
		closeMoleculeOfClosedBead(v.client, id, is, "work bead closed")
	}
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
