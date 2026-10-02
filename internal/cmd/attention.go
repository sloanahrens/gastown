package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	attentionAll      bool
	attentionJSON     bool
	attentionFollow   bool
	attentionInterval time.Duration
)

var attentionCmd = &cobra.Command{
	Use:     "attention",
	GroupID: GroupDiag,
	Short:   "Show the attention queue the daemon maintains, and acknowledge its items",
	Long: `Show the queue of conditions that need a human or overseer judgment, as
the daemon last wrote it, oldest first. Ack an item to hide it until its
condition clears.

The queue is three files under <town>/.runtime/attention/:

  state.json     the current set, rewritten atomically by the daemon every
                 heartbeat; each item is {key, kind, severity, rig, bead, sha,
                 summary, first_seen, last_seen, acked_at}. An item exits the
                 set on its own once its condition clears.
  events.jsonl   the transitions, append-only, rotated to events.jsonl.1 at
                 1 MB. Each line is {ts, class, severity, text, key, state}:
                 state is new or cleared, and the first four fields are exactly
                 the alerts.jsonl schema the gt tail watch source reads.
  acks.json      the acknowledged keys, {acks: [{key, at}]}. gt attention ack
                 writes it under a flock; the daemon only reads it, so neither
                 process races the other's file.

An ack hides its item until the item clears; the ack is then dropped, so a
recurrence raises a new item. gt attention never writes state.json or
events.jsonl.

A state older than 15 minutes is stale: the daemon has stopped writing it.
That prints a STALE header and exits 3, as gt status --line reports an
unknown health. --json prints state.json as it stands.

With -f, print one line per events.jsonl line appended after the command
started, "+" for a new item and "-" for a cleared one.

Examples:
  gt attention                 # the queue, oldest first
  gt attention --all           # include acked items
  gt attention --json          # state.json
  gt attention ack esc:hq-123  # acknowledge one item
  gt attention -f              # stream transitions as they append`,
	Args: cobra.NoArgs,
	RunE: runAttention,
}

var attentionAckCmd = &cobra.Command{
	Use:   "ack <key>",
	Short: "Acknowledge an item by its key, hiding it until its condition clears",
	Long: `Acknowledge the attention item with the given key, writing it to
acks.json. The item is hidden from the queue until its condition clears;
after it clears the ack is dropped and a recurrence raises a new item. An
unknown key exits 1.`,
	Args: cobra.ExactArgs(1),
	RunE: runAttentionAck,
}

func init() {
	attentionCmd.Flags().BoolVar(&attentionAll, "all", false, "Show acked items too")
	attentionCmd.Flags().BoolVar(&attentionJSON, "json", false, "Print state.json instead of the table")
	attentionCmd.Flags().BoolVarP(&attentionFollow, "follow", "f", false, "Stream events.jsonl lines appended after start")
	attentionCmd.Flags().DurationVar(&attentionInterval, "interval", time.Second, "Poll interval with --follow")
	attentionCmd.AddCommand(attentionAckCmd)
	rootCmd.AddCommand(attentionCmd)
}

// attentionView is what the table and --json print: the state, the acks, and
// whether the state is too old to trust.
type attentionView struct {
	State attention.State
	Acks  attention.Acks
	Stale bool
}

// buildAttention reads the queue for a town at now.
func buildAttention(townRoot string, now time.Time) (attentionView, error) {
	state, err := attention.ReadState(townRoot)
	if err != nil {
		return attentionView{}, err
	}
	acks, err := attention.ReadAcks(townRoot)
	if err != nil {
		return attentionView{}, err
	}
	return attentionView{
		State: state,
		Acks:  acks,
		Stale: attention.Stale(state, now, attention.StaleAfter),
	}, nil
}

func runAttention(cmd *cobra.Command, _ []string) error {
	now := time.Now()
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	out := cmd.OutOrStdout()
	if attentionFollow {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ticker := time.NewTicker(attentionInterval)
		defer ticker.Stop()
		return attentionFollowEvents(ctx, out, townRoot, ticker.C)
	}
	view, err := buildAttention(townRoot, now)
	if err != nil {
		return err
	}
	err = attentionReport(out, attentionAll, attentionJSON, view, now)
	if _, ok := IsSilentExit(err); ok {
		// The exit code is the answer; cobra must not print it as an error.
		cmd.SilenceUsage, cmd.SilenceErrors = true, true
	}
	return err
}

func runAttentionAck(cmd *cobra.Command, args []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	if err := ackAttention(townRoot, args[0], time.Now()); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "acked %s\n", args[0])
	return nil
}

// attentionReport writes the table or --json, and returns a silent exit 3 when
// the state is stale.
func attentionReport(w io.Writer, all, asJSON bool, view attentionView, now time.Time) error {
	if asJSON {
		data, err := json.MarshalIndent(view.State, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(w, string(data))
	} else {
		renderAttention(w, view, all, now)
	}
	if view.Stale {
		return NewSilentExit(3)
	}
	return nil
}

// ackAttention records key, refusing a key that is not in the current state.
func ackAttention(townRoot, key string, now time.Time) error {
	state, err := attention.ReadState(townRoot)
	if err != nil {
		return err
	}
	if _, ok := attention.Find(state, key); !ok {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("no attention item with key %q", key)}
	}
	return attention.Acknowledge(townRoot, key, now)
}

// renderAttention prints the table: a STALE header when the state is old, then
// one row per unacked item (all items with --all), oldest first.
func renderAttention(w io.Writer, v attentionView, all bool, now time.Time) {
	if v.Stale {
		fmt.Fprintf(w, "STALE: the daemon has not written attention state since %s (%s ago)\n",
			v.State.Updated.Local().Format("2006-01-02 15:04:05"), townhealth.Short(now.Sub(v.State.Updated)))
	}
	if v.State.Updated.IsZero() {
		fmt.Fprintln(w, "No attention state yet: the daemon has not written one.")
		return
	}
	rows := attentionRows(v, all)
	if len(rows) == 0 {
		fmt.Fprintln(w, "No attention items.")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KIND\tKEY\tBEAD/SHA\tAGE\tSUMMARY")
	for _, it := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			it.Kind, it.Key, beadOrSHA(it), townhealth.Short(now.Sub(it.FirstSeen)), it.Summary)
	}
	_ = tw.Flush()
}

// attentionRows returns the items to show, oldest first, hiding acked ones
// unless all is set. An item is acked if state.json marks it or acks.json
// names its key, so an ack shows before the daemon's next tick.
func attentionRows(v attentionView, all bool) []attention.Item {
	rows := make([]attention.Item, 0, len(v.State.Items))
	for _, it := range v.State.Items {
		if !all && (it.AckedAt != nil || attention.AckedKey(v.Acks, it.Key)) {
			continue
		}
		rows = append(rows, it)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if !rows[i].FirstSeen.Equal(rows[j].FirstSeen) {
			return rows[i].FirstSeen.Before(rows[j].FirstSeen)
		}
		return rows[i].Key < rows[j].Key
	})
	return rows
}

// beadOrSHA is the BEAD/SHA column: the bead when the item names one, else
// the commit.
func beadOrSHA(it attention.Item) string {
	if it.Bead != "" {
		return it.Bead
	}
	return it.SHA
}

// attentionFollowEvents prints each events.jsonl line appended after start
// until ctx is done, driven by ticks so a test can step it.
func attentionFollowEvents(ctx context.Context, w io.Writer, townRoot string, ticks <-chan time.Time) error {
	seen, err := attentionEventCount(townRoot)
	if err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			events, err := attention.ReadEvents(townRoot)
			if err != nil {
				return err
			}
			if len(events) < seen {
				seen = 0 // the log rotated; start over on the fresh one
			}
			for _, e := range events[seen:] {
				writeAttentionEvent(w, e)
			}
			seen = len(events)
		}
	}
}

// attentionEventCount is how many transitions events.jsonl holds now.
func attentionEventCount(townRoot string) (int, error) {
	events, err := attention.ReadEvents(townRoot)
	if err != nil {
		return 0, err
	}
	return len(events), nil
}

// writeAttentionEvent prints one transition as a signed line.
func writeAttentionEvent(w io.Writer, e attention.Event) {
	sign := "+"
	if e.State == attention.EventCleared {
		sign = "-"
	}
	fmt.Fprintf(w, "%s %s %s %s\n", sign, e.TS.Format("15:04:05"), e.Key, e.Text)
}
