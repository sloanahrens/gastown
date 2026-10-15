package cmd

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

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

// overseerBeat writes the heartbeat at now.
func overseerBeat(out io.Writer, townRoot, state, session string, now time.Time) error {
	hb := overseer.Heartbeat{TS: now.UTC(), State: state, Session: session}
	if err := overseer.WriteHeartbeat(townRoot, hb); err != nil {
		return fmt.Errorf("gt overseer beat: %w", err)
	}
	fmt.Fprintf(out, "heartbeat %s at %s\n", state, hb.TS.Format(time.RFC3339))
	return nil
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
