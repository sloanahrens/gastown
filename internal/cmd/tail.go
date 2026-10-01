package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	tailRig      string
	tailSince    string
	tailFollow   bool
	tailKind     string
	tailInterval time.Duration
)

// tailMinInterval keeps --follow from spawning bd per rig in a tight loop.
const tailMinInterval = 500 * time.Millisecond

var tailCmd = &cobra.Command{
	Use:     "tail",
	GroupID: GroupDiag,
	Short:   "Stream bd events, landings and daemon.log as one time-ordered log",
	Long: `Print one time-ordered, plain-text stream of what the town is doing,
merged from three read-only sources:

  events    each store's bd events journal (hq and every registered rig)
  landings  each rig's landings file (.runtime/landings/<rig>.jsonl)
  daemon    daemon/daemon.log, plus its rotated backups for --since

Every line is "<local time with zone> <rig> <kind> <text>", so the first
three fields can be cut or grepped by position. The daemon's rig column is
"town".

gt tail only reads. It computes no verdict and exits 0 whatever it shows:
the town's health is gt status. A source that cannot be read says so in one
line and the stream continues. A store whose config leaves the events
journal off says so once: nothing is journaled there.

Examples:
  gt tail                           # the last 15 minutes, then exit
  gt tail -f                        # the last 15 minutes, then follow
  gt tail --rig gastown --since 2h  # one rig (daemon lines naming it)
  gt tail --kind landings --since 1d
  gt tail -f --since 0 | grep ' landings '`,
	Args: cobra.NoArgs,
	RunE: runTail,
}

func init() {
	tailCmd.Flags().StringVar(&tailRig, "rig", "", "Show one rig: its events and landings, and daemon lines naming it (hq for the town store)")
	tailCmd.Flags().StringVar(&tailSince, "since", "15m", "Start at a duration back (15m, 2h, 1d; 0 = now) or a time (RFC3339, 2006-01-02T15:04:05, 2006-01-02 15:04, 2006-01-02)")
	tailCmd.Flags().BoolVarP(&tailFollow, "follow", "f", false, "Keep polling every source and print new lines as they appear")
	tailCmd.Flags().StringVar(&tailKind, "kind", strings.Join(tailKinds, ","), "Comma-separated sources to show: events, landings, daemon")
	tailCmd.Flags().DurationVar(&tailInterval, "interval", 3*time.Second, "Poll interval with --follow")
	rootCmd.AddCommand(tailCmd)
}

func runTail(cmd *cobra.Command, _ []string) error {
	kinds, err := parseTailKinds(tailKind)
	if err != nil {
		return err
	}
	loc := time.Local
	cutoff, err := parseTailSince(tailSince, time.Now(), loc)
	if err != nil {
		return err
	}
	if tailFollow && tailInterval < tailMinInterval {
		return fmt.Errorf("--interval %s: at least %s", tailInterval, tailMinInterval)
	}
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return err
	}
	sources, preface, err := buildTailSources(tailOptions{
		townRoot: townRoot, rig: tailRig, kinds: kinds, cutoff: cutoff, loc: loc, now: time.Now,
		rigNames: knownRigNames, journalFor: openTailJournal,
	})
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	var tick <-chan time.Time
	if tailFollow {
		ticker := time.NewTicker(tailInterval)
		defer ticker.Stop()
		tick = ticker.C
	}
	return runTailStream(ctx, cmd.OutOrStdout(), sources, preface, tailFollow, tick, loc)
}

// tailOptions selects gt tail's sources.
type tailOptions struct {
	townRoot string
	rig      string // "" = every rig
	kinds    map[string]bool
	cutoff   time.Time
	loc      *time.Location
	now      func() time.Time
	// rigNames reads the rig registry (knownRigNames in production).
	rigNames func(townRoot string) ([]string, error)
	// journalFor opens a store's journal (openTailJournal in production).
	journalFor func(townRoot, rig string) (tailJournal, error)
}

func allTailKinds() map[string]bool {
	m := map[string]bool{}
	for _, k := range tailKinds {
		m[k] = true
	}
	return m
}

// openTailJournal opens a store's journal through bd, pinned to that store's
// beads directory.
func openTailJournal(townRoot, rig string) (tailJournal, error) {
	dir := doltserver.FindRigBeadsDir(townRoot, rig)
	if dir == "" {
		return nil, fmt.Errorf("no beads directory for %s", rig)
	}
	if _, err := os.Stat(dir); err != nil {
		return nil, fmt.Errorf("beads directory %s: %w", dir, err)
	}
	return &tailBDJournal{Beads: beads.NewWithBeadsDir(townRoot, dir), dir: dir, run: deps.NewBDProcessRunner(filepath.Dir(dir))}, nil
}

// buildTailSources returns the selected sources in tie order: hq, then each
// rig by name (events before landings), then the daemon. preface holds lines
// about the selection itself. Only an unknown --rig is an error.
func buildTailSources(o tailOptions) (sources []tailSource, preface []tailLine, err error) {
	rigs, regErr := o.rigNames(o.townRoot)
	if regErr != nil {
		if o.rig != "" && o.rig != "hq" {
			return nil, nil, fmt.Errorf("--rig %q: cannot read the rig registry: %w", o.rig, regErr)
		}
		preface = append(preface, tailLine{At: o.now(), Rig: "town", Kind: "tail",
			Text: fmt.Sprintf("cannot read the rig registry (%v): showing hq and the daemon only", regErr)})
		rigs = nil
	}
	stores := append([]string{"hq"}, rigs...)
	if o.rig != "" {
		found := false
		for _, s := range stores {
			if s == o.rig {
				found = true
			}
		}
		if !found {
			return nil, nil, fmt.Errorf("--rig %q: not a registered rig (have %s)", o.rig, strings.Join(stores, ", "))
		}
		stores = []string{o.rig}
	}
	for _, rig := range stores {
		if o.kinds[tailKindEvents] {
			j, openErr := o.journalFor(o.townRoot, rig)
			sources = append(sources, &eventsSource{rig: rig, journal: j, openErr: openErr, cutoff: o.cutoff, now: o.now})
		}
		if o.kinds[tailKindLandings] && rig != "hq" {
			path, pathErr := landings.Path(o.townRoot, rig)
			if pathErr != nil {
				preface = append(preface, tailLine{At: o.now(), Rig: rig, Kind: tailKindLandings, Text: pathErr.Error()})
				continue
			}
			sources = append(sources, &landingsSource{rig: rig, reader: &landings.Reader{Path: path}, cutoff: o.cutoff, now: o.now})
		}
	}
	if o.kinds[tailKindDaemon] {
		d := &daemonSource{dir: filepath.Join(o.townRoot, "daemon"), cutoff: o.cutoff, loc: o.loc, now: o.now}
		if o.rig != "" {
			d.rigFilter = tailRigFilter(o.rig)
		}
		sources = append(sources, d)
	}
	return sources, preface, nil
}

// runTailStream polls every source once and prints the merged batch; with
// follow it polls again on each tick until ctx ends. Each batch is sorted on
// its own, so a line that reaches a source late prints after lines already
// shown. A failed write (a closed pipe) ends the stream with its error.
func runTailStream(ctx context.Context, w io.Writer, sources []tailSource, preface []tailLine, follow bool, tick <-chan time.Time, loc *time.Location) error {
	bw := bufio.NewWriter(w)
	emit := func(extra []tailLine) error {
		batches := [][]tailLine{extra}
		for _, s := range sources {
			batches = append(batches, s.Poll())
		}
		for _, l := range mergeTail(batches...) {
			if _, err := bw.WriteString(renderTailLine(l, loc) + "\n"); err != nil {
				return err
			}
		}
		return bw.Flush()
	}
	if err := emit(preface); err != nil {
		return err
	}
	if !follow {
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick:
			if err := emit(nil); err != nil {
				return err
			}
		}
	}
}

// tailLine is one line of the gt tail stream: when it happened, which rig it
// belongs to ("town" for the daemon, "hq" for the town store), which source
// produced it, and the text. Render turns it into
// "<local RFC3339 time> <rig> <kind> <text>".
type tailLine struct {
	At   time.Time
	Rig  string
	Kind string
	Text string
}

// tailSource is one read-only input to the stream. Poll returns the lines
// that appeared since the previous poll; the first poll returns the backlog
// from the source's cutoff. A source reports its own read failures as lines
// and never stops the stream.
type tailSource interface {
	Poll() []tailLine
}

// The kinds gt tail merges, in the order ties print.
const (
	tailKindEvents   = "events"
	tailKindLandings = "landings"
	tailKindDaemon   = "daemon"
)

var tailKinds = []string{tailKindEvents, tailKindLandings, tailKindDaemon}

// mergeTail concatenates the batches and orders them by time. The sort is
// stable, so lines with equal times keep batch order and, within a batch,
// source order.
func mergeTail(batches ...[]tailLine) []tailLine {
	var out []tailLine
	for _, b := range batches {
		out = append(out, b...)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// renderTailLine formats one line in loc. Rig and kind are single tokens and
// the text has no control characters, so every record is exactly one line
// and the first three fields can be cut or grepped by position.
func renderTailLine(l tailLine, loc *time.Location) string {
	return l.At.In(loc).Format(time.RFC3339) + " " + tailToken(l.Rig) + " " + tailToken(l.Kind) + " " + tailText(l.Text)
}

func tailToken(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return '_'
		}
		return r
	}, s)
	if s == "" {
		return "-"
	}
	return s
}

func tailText(s string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}

var tailDaysRe = regexp.MustCompile(`^([0-9]+)d$`)

// tailMaxSinceDays bounds "--since Nd" well inside time.Duration's range.
const tailMaxSinceDays = 36500

// parseTailSince reads --since: a duration back from now ("15m", "2h", "1d",
// "0" for now), an RFC3339 timestamp, or a local time as
// "2006-01-02T15:04:05", "2006-01-02 15:04" or "2006-01-02".
func parseTailSince(s string, now time.Time, loc *time.Location) (time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return now, nil
	}
	if m := tailDaysRe.FindStringSubmatch(s); m != nil {
		n, err := strconv.Atoi(m[1])
		if err != nil || n > tailMaxSinceDays {
			return time.Time{}, fmt.Errorf("--since %q: at most %dd", s, tailMaxSinceDays)
		}
		return now.Add(-time.Duration(n) * 24 * time.Hour), nil
	}
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("--since %q: a duration back from now cannot be negative", s)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("--since %q: want a duration (15m, 2h, 1d) or a time (RFC3339, 2006-01-02T15:04:05, 2006-01-02 15:04, 2006-01-02)", s)
}

// parseTailKinds reads --kind: a comma-separated subset of tailKinds.
func parseTailKinds(s string) (map[string]bool, error) {
	kinds := map[string]bool{}
	for _, k := range strings.Split(s, ",") {
		k = strings.TrimSpace(k)
		if k == "" {
			continue
		}
		valid := false
		for _, v := range tailKinds {
			if k == v {
				valid = true
			}
		}
		if !valid {
			return nil, fmt.Errorf("--kind %q: unknown kind (want any of %s)", k, strings.Join(tailKinds, ","))
		}
		kinds[k] = true
	}
	if len(kinds) == 0 {
		return nil, fmt.Errorf("--kind %q: name at least one of %s", s, strings.Join(tailKinds, ","))
	}
	return kinds, nil
}
