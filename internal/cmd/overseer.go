package cmd

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/overseer"
	"github.com/steveyegge/gastown/internal/workspace"
)

// The overseer's two small writes: a heartbeat that says it is alive, and a
// journal line for each intervention. The files and their readers are
// internal/overseer's; the daemon's silent-overseer rule and the dashboard's
// Overseer pane read what these commands leave.

var (
	overseerBeatState   string
	overseerBeatSession string
	overseerLogBead     string
)

var overseerCmd = &cobra.Command{
	Use:     "overseer",
	GroupID: GroupDiag,
	Short:   "Record the overseer's heartbeat and journal its interventions",
	Long: `The overseer session reports to the town through two files under
.runtime/overseer/: a heartbeat the dashboard and the daemon read to tell
whether it is alive, and a journal of what it did, by kind. Neither starts,
stops or restarts anything.`,
	RunE: requireSubcommand,
}

var overseerBeatCmd = &cobra.Command{
	Use:   "beat",
	Short: "Write the overseer heartbeat",
	Long: `Write .runtime/overseer/heartbeat.json: the time, the state and the session.

The state is one of working, idle or away. The daemon raises overseer.silent,
as info, when the heartbeat is older than overseer.silent_after (default 90m)
while ready work or an unanswered question waits.

Examples:
  gt overseer beat
  gt overseer beat --state idle`,
	Args: cobra.NoArgs,
	RunE: runOverseerBeat,
}

var overseerLogCmd = &cobra.Command{
	Use:   "log <kind> <text>",
	Short: "Journal one intervention",
	Long: `Append one line to .runtime/overseer/journal.jsonl: the time, the kind, the
text and, with --bead, the bead it concerned. The text is cut to 300
characters and collapsed to one line. The dashboard counts the last seven days
by kind: the backlog for what to automate next.

Kinds: ` + "file_bead, review, config_edit, hold, install, nuke_seat, answer, decision, fix_by_hand" + `.

Examples:
  gt overseer log hold "parked gt-abc until the retry question is answered" --bead gt-abc
  gt overseer log fix_by_hand "reverted the bad formula sync"`,
	Args: cobra.ExactArgs(2),
	RunE: runOverseerLog,
}

func init() {
	overseerBeatCmd.Flags().StringVar(&overseerBeatState, "state", overseer.StateWorking, "State to report: "+strings.Join(overseer.States(), ", "))
	overseerBeatCmd.Flags().StringVar(&overseerBeatSession, "session", "", "Session to record (default: $GT_SESSION)")
	overseerLogCmd.Flags().StringVar(&overseerLogBead, "bead", "", "The bead the intervention concerned")
	overseerCmd.AddCommand(overseerBeatCmd, overseerLogCmd)
	rootCmd.AddCommand(overseerCmd)
}

func runOverseerBeat(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	session := overseerBeatSession
	if session == "" {
		session = os.Getenv("GT_SESSION")
	}
	return overseerBeat(cmd.OutOrStdout(), townRoot, overseerBeatState, session, time.Now())
}

// overseerOpenWork reads what a working beat must see at zero: the keys of the
// unacknowledged attention items and the count of open escalations. It is a
// parameter and not a package variable, so a test passes its own readers.
type overseerOpenWork struct {
	items       func(townRoot string) ([]string, error)
	escalations func(townRoot string) (int, error)
}

// liveOverseerOpenWork reads the town's attention store and the bead plane. newClient gives the
// bead client for a town; production passes overseerBeadsClient, a test passes a fake.
func liveOverseerOpenWork(newClient func(townRoot string) beads.Client) overseerOpenWork {
	return overseerOpenWork{
		items: overseerUnackedItems,
		escalations: func(townRoot string) (int, error) {
			return countOpenEscalations(newClient(townRoot))
		},
	}
}

// overseerBeadsClient is the bead client the live readers use for a town.
func overseerBeadsClient(townRoot string) beads.Client {
	return beads.New(beads.ResolveBeadsDir(townRoot))
}

// countOpenEscalations counts the open escalations on the client's bead plane.
func countOpenEscalations(c beads.Client) (int, error) {
	issues, err := beads.ListEscalationsAcrossRigs(c)
	if err != nil {
		return 0, err
	}
	return len(issues), nil
}

// overseerUnackedItems lists the keys of the attention items no human has
// acknowledged yet. An escalation the daemon raised also appears here, so the
// escalations counted separately are the ones an ack has hidden.
func overseerUnackedItems(townRoot string) ([]string, error) {
	state, err := attention.ReadState(townRoot)
	if err != nil {
		return nil, err
	}
	// The old check read only the state item's acked_at, which the daemon never writes; acks live
	// in acks.json (gt attention ack writes them there). An item counts as acked when either says
	// so. An unreadable acks.json is an error, so the beat refuses rather than guessing (gt-smdcj).
	acks, err := attention.ReadAcks(townRoot)
	if err != nil {
		return nil, fmt.Errorf("reading acks: %w", err)
	}
	var keys []string
	for _, item := range state.Items {
		if item.AckedAt != nil || attention.AckedKey(acks, item.Key) {
			continue
		}
		keys = append(keys, item.Key)
	}
	return keys, nil
}

// overseerBeat writes the heartbeat at now, reading the live town for the
// working-beat check.
func overseerBeat(out io.Writer, townRoot, state, session string, now time.Time) error {
	return overseerBeatWith(out, townRoot, state, session, now, liveOverseerOpenWork(overseerBeadsClient))
}

// overseerBeatWith writes the heartbeat at now. A working beat is refused while
// an attention item is unacknowledged or an escalation is open: the overseer has
// to triage them before it can say it is working, and the refusal lists them.
func overseerBeatWith(out io.Writer, townRoot, state, session string, now time.Time, open overseerOpenWork) error {
	if state == overseer.StateWorking {
		if err := overseerRefuseIfOpen(townRoot, open); err != nil {
			return err
		}
	}
	hb := overseer.Heartbeat{TS: now.UTC(), State: state, Session: session}
	if err := overseer.WriteHeartbeat(townRoot, hb); err != nil {
		return fmt.Errorf("gt overseer beat: %w", err)
	}
	fmt.Fprintf(out, "heartbeat %s at %s\n", state, hb.TS.Format(time.RFC3339))
	return nil
}

// overseerRefuseIfOpen returns an error naming every open item, or nil when
// there are none.
func overseerRefuseIfOpen(townRoot string, open overseerOpenWork) error {
	keys, err := open.items(townRoot)
	if err != nil {
		return fmt.Errorf("gt overseer beat: reading attention: %w", err)
	}
	escalations, err := open.escalations(townRoot)
	if err != nil {
		return fmt.Errorf("gt overseer beat: counting escalations: %w", err)
	}
	// A daemon-raised escalation is counted in both lists: its attention item's key is esc:<id>.
	// Only the escalations with no such item are added to the count, so each is named once.
	extra := escalations - escalationItemKeys(keys)
	if extra < 0 {
		extra = 0
	}
	if len(keys) == 0 && extra == 0 {
		return nil
	}
	quoted := make([]string, len(keys))
	for i, k := range keys {
		quoted[i] = strconv.Quote(k)
	}
	return fmt.Errorf("gt overseer beat: refusing to record working with %d unacked attention item(s) and %d open escalation(s) with no attention item; triage each with gt attention ack <key> --disposition needed-human|fixed-by-overseer|noise, then gt escalate close:\n  %s",
		len(keys), extra, strings.Join(quoted, "\n  "))
}

// escalationItemKeys counts the attention keys that name an escalation.
func escalationItemKeys(keys []string) int {
	n := 0
	for _, k := range keys {
		if strings.HasPrefix(k, "esc:") {
			n++
		}
	}
	return n
}

func runOverseerLog(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	return overseerLog(cmd.OutOrStdout(), townRoot, args[0], args[1], overseerLogBead, time.Now())
}

// overseerLog appends one journal line at now. An unknown kind is refused with
// the allowed ones, before anything is written.
func overseerLog(out io.Writer, townRoot, kind, text, bead string, now time.Time) error {
	e := overseer.Entry{TS: now.UTC(), Kind: kind, Text: text, Bead: bead}
	if err := overseer.AppendEntry(townRoot, e); err != nil {
		return fmt.Errorf("gt overseer log: %w", err)
	}
	fmt.Fprintf(out, "journaled %s\n", kind)
	return nil
}
