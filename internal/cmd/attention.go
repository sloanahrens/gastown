package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/events"
	"github.com/steveyegge/gastown/internal/townhealth"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	attentionAll         bool
	attentionJSON        bool
	attentionFollow      bool
	attentionWakeOnly    bool
	attentionSince       time.Duration
	attentionInterval    time.Duration
	attentionDisposition string
)

// attentionRotateSize is the size at which events.jsonl and acks-history.jsonl
// rotate, spelled for the help text from attention.EventsMaxBytes.
var attentionRotateSize = fmt.Sprintf("%d MB", attention.EventsMaxBytes>>20)

var attentionCmd = &cobra.Command{
	Use:     "attention",
	GroupID: GroupDiag,
	Short:   "Show the attention queue the daemon maintains, and acknowledge its items",
	Long: `Show the queue of conditions that need a human or overseer judgment, as
the daemon last wrote it, oldest first. Ack an item to hide it until its
condition clears.

The queue is four files under <town>/.runtime/attention/:

  state.json     the current set, rewritten atomically by the daemon every
                 heartbeat; each item is {key, kind, severity, rig, bead, sha,
                 summary, first_seen, last_seen, acked_at}. An item exits the
                 set on its own once its condition clears.
  events.jsonl   the transitions, append-only, rotated to events.jsonl.1 at
                 ` + attentionRotateSize + `. Each line is {ts, class, severity, text, key, state}:
                 state is new or cleared, and the first four fields are exactly
                 the alerts.jsonl schema the gt tail watch source reads.
  acks.json      the acknowledged keys, {acks: [{key, at}]}. gt attention ack
                 writes it under a flock; the daemon only reads it, so neither
                 process races the other's file.
  acks-history.jsonl  one line per ack, {ts, key, kind, rule, disposition},
                 appended by gt attention ack and rotated to acks-history.jsonl.1 at
                 ` + attentionRotateSize + `. acks.json forgets an ack when its item clears;
                 this does not.

An ack hides its item until the item clears; the ack is then dropped, so a
recurrence raises a new item. gt attention never writes state.json or
events.jsonl.

A state older than 15 minutes is stale: the daemon has stopped writing it.
That prints a STALE header and exits 3, as gt status --line reports an
unknown health. --json prints state.json as it stands.

Every event also names the rule that raised it, who handles it and whether it
should wake the overseer or the human: {rule, handled, wake}. rule is a stable
id such as landing.stuck_inflight; handled is self_healed, operator_action or
info; a line written before these fields existed reads as info, wake=false.

With -f, print one line per events.jsonl line appended after the command
started, as "HH:MM:SS rule detail" (a cleared item's detail starts "cleared:").
--wake-only prints only the events that need a decision, so one monitor can
stand in for many. --json prints the events.jsonl lines themselves. --since
DURATION first replays the events from that long ago. It follows the file
across its rotation and exits 0 on SIGINT or SIGTERM.

Examples:
  gt attention                 # the queue, oldest first
  gt attention --all           # include acked items
  gt attention --json          # state.json
  gt attention ack esc:hq-123 --disposition needed-human  # ack, saying why
  gt attention -f              # stream transitions as they append
  gt attention --follow --wake-only   # only the events that need a decision`,
	Args: cobra.NoArgs,
	RunE: runAttention,
}

var attentionAckCmd = &cobra.Command{
	Use:   "ack <key>",
	Short: "Acknowledge an item by its key, saying what it needed",
	Long: `Acknowledge the attention item with the given key, writing it to
acks.json. The item is hidden from the queue until its condition clears;
after it clears the ack is dropped and a recurrence raises a new item. An
unknown key exits 1.

--disposition is required and records why the item was acked, in
acks-history.jsonl: needed-human (it needed a human), fixed-by-overseer (the
overseer fixed it) or noise (it was noise). The Jev replay reads it as the
item's label. A call without it, or with another value, exits 1 and acks
nothing, so a script that ran "gt attention ack <key>" must add the flag.`,
	Args: cobra.ExactArgs(1),
	RunE: runAttentionAck,
}

func init() {
	attentionCmd.Flags().BoolVar(&attentionAll, "all", false, "Show acked items too")
	attentionCmd.Flags().BoolVar(&attentionJSON, "json", false, "Print state.json instead of the table")
	attentionCmd.Flags().BoolVarP(&attentionFollow, "follow", "f", false, "Stream events.jsonl lines appended after start")
	attentionCmd.Flags().DurationVar(&attentionInterval, "interval", time.Second, "Poll interval with --follow")
	attentionCmd.Flags().BoolVar(&attentionWakeOnly, "wake-only", false, "With --follow, print only events that need a decision (wake=true)")
	attentionCmd.Flags().DurationVar(&attentionSince, "since", 0, "With --follow, first replay the events from the last DURATION")
	attentionAckCmd.Flags().StringVar(&attentionDisposition, "disposition", "",
		"Why you acked: needed-human, fixed-by-overseer or noise")
	_ = attentionAckCmd.MarkFlagRequired("disposition")
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
	if err := checkFollowFlags(attentionFollow, attentionWakeOnly, attentionSince); err != nil {
		return err
	}
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
		opts := followOptions{wakeOnly: attentionWakeOnly, asJSON: attentionJSON, since: attentionSince, now: now}
		return attentionFollowEvents(ctx, out, townRoot, ticker.C, opts)
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
	return ackAttentionWithDisposition(cmd.OutOrStdout(), townRoot, args[0], attentionDisposition, time.Now())
}

// ackAttentionWithDisposition parses the --disposition value and acks key. A
// value that is not a disposition is refused before anything is read or
// written.
func ackAttentionWithDisposition(w io.Writer, townRoot, key, disposition string, now time.Time) error {
	d, err := attention.ParseDisposition(disposition)
	if err != nil {
		return err
	}
	if err := ackAttention(townRoot, key, now, d); err != nil {
		return err
	}
	fmt.Fprintf(w, "acked %s (%s)\n", key, d)
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
func ackAttention(townRoot, key string, now time.Time, d attention.Disposition) error {
	state, err := attention.ReadState(townRoot)
	if err != nil {
		return err
	}
	it, ok := attention.Find(state, key)
	if !ok {
		return &ExitCodeError{Code: 1, Err: fmt.Errorf("no attention item with key %q", key)}
	}
	return attention.AcknowledgeItem(townRoot, it, now, d)
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

// checkFollowFlags refuses the follow-only flags without --follow, which
// would otherwise be silently ignored.
func checkFollowFlags(follow, wakeOnly bool, since time.Duration) error {
	if !follow && (wakeOnly || since > 0) {
		return fmt.Errorf("--wake-only and --since need --follow")
	}
	return nil
}

// followOptions are the filters and format of gt attention --follow.
type followOptions struct {
	// wakeOnly prints only raised events with wake=true: the ones that need a
	// decision.
	wakeOnly bool
	// asJSON prints the events.jsonl line itself instead of the human line.
	asJSON bool
	// since replays the events already in the file whose ts is within this
	// duration of now, before following. Zero starts at the end of the file.
	since time.Duration
	// now is the clock since is measured from.
	now time.Time
}

// attentionFollowEvents prints each events.jsonl line appended after start
// until ctx is done, driven by ticks so a test can step it. It follows the
// file by path (events.Tail), so the 1 MB rotation to events.jsonl.1 does not
// leave it reading the old inode.
func attentionFollowEvents(ctx context.Context, w io.Writer, townRoot string, ticks <-chan time.Time, opts followOptions) error {
	path := attention.EventsPath(townRoot)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tail, err := events.OpenTail(path)
	if err != nil {
		return err
	}
	defer func() { _ = tail.Close() }()
	if opts.since > 0 {
		history, err := attentionHistory(attention.RotatedEventsPath(townRoot), path, tail.Offset())
		if err != nil {
			return err
		}
		cutoff := opts.now.Add(-opts.since)
		printAttentionLines(w, history, opts, func(e attention.Event) bool { return !e.TS.Before(cutoff) })
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticks:
			lines, err := tail.Poll()
			printAttentionLines(w, lines, opts, nil)
			if err != nil {
				return err
			}
		}
	}
}

// attentionHistory returns the complete lines the retained log held when the
// tail opened: all of rotated, then the first end bytes of path. A missing
// file is empty. The rotated file comes first, as attention.Read reads it, so
// a --since window that spans a rotation replays both halves. The offset stops
// replay from overlapping what follow prints.
func attentionHistory(rotated, path string, end int64) ([]string, error) {
	old, err := os.ReadFile(rotated) //nolint:gosec // G304: a path built from the town root
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	data, err := os.ReadFile(path) //nolint:gosec // G304: a path built from the town root
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > end {
		data = data[:end]
	}
	lines := strings.Split(string(old), "\n")
	return append(lines, strings.Split(string(data), "\n")...), nil
}

// printAttentionLines prints each line that parses as an event, passes the
// wake filter and, when keep is set, keep. A line that does not parse is
// skipped, as ReadEvents skips it.
func printAttentionLines(w io.Writer, lines []string, opts followOptions, keep func(attention.Event) bool) {
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e attention.Event
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		if opts.wakeOnly && !e.Wake {
			continue
		}
		if keep != nil && !keep(e) {
			continue
		}
		if opts.asJSON {
			fmt.Fprintln(w, line)
			continue
		}
		fmt.Fprintln(w, attentionEventLine(e))
	}
}

// attentionEventLine is the human line for one event: the time, the rule that
// raised it and the collector's own detail text. A cleared event says so; a
// line written before rules existed shows its class in the rule's place.
func attentionEventLine(e attention.Event) string {
	rule := e.Rule
	if rule == "" {
		rule = string(e.Class)
	}
	detail := e.Text
	if e.State == attention.EventCleared {
		detail = "cleared: " + detail
	}
	return fmt.Sprintf("%s %s %s", e.TS.Local().Format("15:04:05"), rule, detail)
}
