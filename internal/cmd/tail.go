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
	"sync"
	"syscall"
	"time"
	"unicode"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/deps"
	"github.com/steveyegge/gastown/internal/doltserver"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/landings"
	"github.com/steveyegge/gastown/internal/style"
	"github.com/steveyegge/gastown/internal/ui"
	"github.com/steveyegge/gastown/internal/version"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	tailRig      string
	tailSince    string
	tailFollow   bool
	tailKind     string
	tailInterval time.Duration
	tailAll      bool
	tailVerbose  bool
	tailISO      bool
	tailColor    string
)

// tailMinInterval keeps --follow from spawning bd per rig in a tight loop.
const tailMinInterval = 500 * time.Millisecond

var tailCmd = &cobra.Command{
	Use:     "tail",
	GroupID: GroupDiag,
	Short:   "Stream bd events, landings, daemon.log and the watch feed as one time-ordered log",
	Long: `Print one time-ordered, plain-text stream of what the town is doing,
merged from four read-only sources:

  events    each store's bd events journal (hq and every registered rig)
  landings  each rig's landings file (.runtime/landings/<rig>.jsonl)
  daemon    daemon/daemon.log, plus its rotated backups for --since
  watch     the town's watch feed: .runtime/watch/alerts.jsonl and
            .runtime/attention/events.jsonl

Every line is "<local time> <tag> <text>": one short source tag — the rig a
daemon line belongs to ("town" for the daemon's own town-wide lines), the
store's rig name, or the watch feed's "watch" — so a typical line fits a
terminal without wrapping. The time is HH:MM:SS in local time; --iso prints RFC3339 with the
date and zone. --verbose keeps the "<rig> <kind>" columns, the form to cut or
grep by position.

On a terminal each line is colored by what it says and tagged with an emoji:
green and ✅ for a landing, a post-land check or a green health line, red and
❌ for a rejection, a RED line or a failure, yellow and ⚠️ for SLOW, a timeout
or an escalation, blue and 🛬 for a landing still in flight, 🚀 for a
dispatch, 🔁 for a restart. Everything else is dim. A pipe gets plain text,
with no emoji. --color selects: auto (the default) draws color and emoji only
on a terminal, and honors NO_COLOR and GT_NO_EMOJI; never draws either;
always draws both.

The default view hides what the town does every few seconds: wisp events
(gt-wisp-*, hq-wisp-*) and their "close detected" lines, the daemon's
heartbeat, plugin-handler skips, the patrols a config leaves off, the
checkpoint, jsonl-backup, patrol-scan and doctor all-clear lines, clearAlerts
lines, a polecat agent bead's own status writes, a townhealth line that
repeats the last one shown but for the age and the exec-tax reading it
carries, and a spec-dispatcher tick whose counts repeat the last tick shown. A
tick that dispatched a bead, or whose counts moved, always shows. Landings,
rejections, escalations, upgrade restarts, a tick that dispatched, and bead
create/close/status changes always show, and a hidden line that reports a
failure shows anyway. --all (or --verbose) shows everything; --iso prints
today's stream, without the folds and the annotations described below.

It also folds one bead's own churn. A bead's status is rewritten every few
seconds as a polecat works, so the default view collapses the status updates
for one bead written within a minute of one another into one line: the last
status and the count of updates folded into it, as "update gt-1
status=in_progress (×4)". An update that is not adjacent to the run, and a run
of one, prints as it is.

It also drops what repeats. An events line that says what the line before it
said — the same bead, operation and status within two seconds — prints once:
that is one bd write the journal recorded twice, or a retry that landed on the
same row. And an events line drops its seq= cursor, and its
actor= when that actor is your own git user.name; another writer's actor
stays. --verbose keeps both fields, which is what to cut or grep by position.

A bead line names the bead: create, close, dependency and status-change lines
append the bead's title after a " · ", read once per bead per run and
truncated so the line still fits the terminal (80 columns when stdout is not
one). A title that cannot be read is simply absent — never an error line.

Two more annotations are the default view's. A comment line prints the first 60
characters of the comment, and a work bead's submission prints "submitted
<bead> @ <sha7> for landing" beside the update that left it — the READY TO LAND
block a polecat's gt done writes, shown once per head while the bead waits to
land.

The review loop's own words win over the comment text. A comment or a note
that opens with OVERSEER REVIEW, OVERSEER RULING, STEWARD or MERGE REJECTION
prints its first 100 characters after the line, green for a PASS, red for a
FAIL, a REFUSED or a REJECTION, and yellow for a "(shadow)" verdict. Every
other comment prints only its first 60 characters.

The watch feed is what the operator's monitor scripts and the daemon's
attention queue see: one JSON object per line, {ts, class, severity, text}.
Each line prints under the "watch" tag, colored by its severity alone — low
yellow, high red — and the default view always shows it. The feed is the
town's, not one store's, so no line carries a rig and --rig does not narrow
it. A missing feed file is silence; a line that is not an alert is skipped
with one note beside it. Writers append and rotate at 1 MB (see
docs/reference.md).

A daemon line that belongs to one rig prints under that rig's tag, not "town".
The daemon names the rig on a line it writes for one — "<component>: <rig>:
..." for any per-rig component (landing_worker, tier_sweep, patrol_scan, the
dogs and the scheduled jobs) — and a "[land] <bead>: ..." line routes the
bead's prefix through the town's routes.jsonl (gt- is gastown, om- is om, hm-
is hm, be- is beads). A line whose second token is not a registered rig, a
prefix with no route, and the daemon's own town-wide lines (starting,
upgrade-restart, townhealth, dispatcher ticks, the patrols), stay "town". Only
the tag changes: the daemon log's text is what gt-kpi, the gt-watch scripts and
the dashboard parse, and it is untouched.

A bead's trip is shown too. The daemon log's own lines — the spec
dispatcher's dispatch, the landing worker's merge and land, and the upgrade
restart that installs a commit — say when a bead was dispatched, merged,
landed and deployed, and one line per deploy reports it: "<bead> deployed in
<n>m (work <n>m, land <n>m, deploy <n>m)", the total and its three stages
(work is dispatched to merged, land is merged to landed, deploy is landed to
deployed). A bead counts as deployed only when its landed commit is an
ancestor of the commit a restart installs — a git merge-base --is-ancestor
run in the town's gt source checkout — never merely by the next restart,
because a restart can install a binary built before the bead landed; a check
that cannot run leaves the bead waiting. A bead with no dispatch line, one a
human slung, prints its deploy stage alone. The deploy lines are the town's,
like the watch feed's, so --rig does not narrow them.

With -f, one dim "📊" line reports the town's state: the seats in use against
the cap with the polecat:bead pairs holding them, the beads waiting to land,
origin/main against the binary you are running, the open escalations, and the
DeepSeek rate when ~/.runtime/watch/spend.json was written in the last fifteen
minutes. It prints on the first poll, then only when one of those fields moves
and at most once every five minutes; a field that cannot be read is left out.
The line also carries the town's shipping speed: the median
dispatched-to-deployed minutes over the beads that landed in the last hour,
and how many landed beads still wait for a deploy with the oldest one's age.
A run without -f never prints it.

The first poll is gathered from every source — each one bounded by its own
read budget — and printed in time order, so gt tail -f shows one ordered
backlog. What arrives after it prints as each later poll returns.

gt tail only reads. It computes no verdict and exits 0 whatever it shows:
the town's health is gt status. A source that cannot be read says so in one
line and the stream continues. A store whose config leaves the events
journal off says so once: nothing is journaled there.

Examples:
  gt tail                           # the last 15 minutes, then exit
  gt tail -f                        # the last 15 minutes, then follow
  gt tail --rig gastown --since 2h  # one rig (its daemon lines)
  gt tail --kind landings --since 1d
  gt tail -f --verbose --since 0 | grep ' landings '`,
	Args: cobra.NoArgs,
	RunE: runTail,
}

func init() {
	tailCmd.Flags().StringVar(&tailRig, "rig", "", "Show one rig: its events and landings, and the daemon lines it names or belongs to (hq for the town store; the watch feed is town-wide)")
	tailCmd.Flags().StringVar(&tailSince, "since", "15m", "Start at a duration back (15m, 2h, 1d; 0 = now) or a time (RFC3339, 2006-01-02T15:04:05, 2006-01-02 15:04, 2006-01-02)")
	tailCmd.Flags().BoolVarP(&tailFollow, "follow", "f", false, "Keep polling every source and print new lines as they appear")
	tailCmd.Flags().StringVar(&tailKind, "kind", strings.Join(tailKinds, ","), "Comma-separated sources to show: events, landings, daemon, watch")
	tailCmd.Flags().DurationVar(&tailInterval, "interval", 3*time.Second, "Poll interval with --follow")
	tailCmd.Flags().BoolVar(&tailAll, "all", false, "Show routine lines too: wisp events, heartbeats, handler skips, dog and patrol-scan chatter")
	tailCmd.Flags().BoolVar(&tailVerbose, "verbose", false, "Same as --all, and keep the <rig> <kind> columns")
	tailCmd.Flags().BoolVar(&tailISO, "iso", false, "Print each time as RFC3339 with the date and zone, not HH:MM:SS")
	tailCmd.Flags().StringVar(&tailColor, "color", string(tailColorAuto), "How to color the lines: auto (only on a terminal), never, always")
	rootCmd.AddCommand(tailCmd)
}

func runTail(cmd *cobra.Command, _ []string) error {
	kinds, err := parseTailKinds(tailKind)
	if err != nil {
		return err
	}
	colorMode, err := parseTailColor(tailColor)
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
	beadReads := openTailBeads(townRoot)
	deploys := newTailDeploys(time.Now, tailGitAncestry(townRoot), cutoff)
	sources, preface, err := buildTailSources(tailOptions{
		townRoot: townRoot, rig: tailRig, kinds: kinds, cutoff: cutoff, loc: loc, now: time.Now,
		rigNames: knownRigNames, journalFor: openTailJournal, beads: beadReads, deploys: deploys,
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
	all := tailAll || tailVerbose
	// The identity the default view trims is the default view's; a raw run
	// asks git for nothing.
	gitUser := ""
	if !all {
		gitUser = tailGitUser(townRoot)
	}
	view := newTailView(loc, tailViewOptions{
		all:        all,
		iso:        tailISO,
		fullSource: tailVerbose,
		decorate:   tailDecorate(colorMode),
		width:      tailDisplayWidth(cmd.OutOrStdout()),
		gitUser:    gitUser,
		beads:      beadReads,
		now:        time.Now,
	})
	// The summary is the follow mode's: a run that prints a backlog and exits
	// has no "since the last line" to summarize.
	var summary *tailSummaryTracker
	if tailFollow {
		summary = &tailSummaryTracker{
			read:     func() tailSummaryFields { return readTailSummary(townRoot, tailRigAgentBeads, deploys) },
			now:      time.Now,
			interval: tailSummaryFresh,
			width:    view.Width,
		}
	}
	return runTailStream(ctx, cmd.OutOrStdout(), sources, preface, tailFollow, tick, view, summary)
}

// tailDisplayWidth is the column the stream keeps its lines inside: the
// terminal's when stdout is one, 80 otherwise, so a pipe and a log file read
// as the terminal does. A terminal that will not report its size gets the
// same 80.
func tailDisplayWidth(w io.Writer) int {
	const tailDefaultWidth = 80
	f, ok := w.(*os.File)
	if !ok {
		return tailDefaultWidth
	}
	width, _, err := term.GetSize(int(f.Fd()))
	if err != nil || width <= 0 {
		return tailDefaultWidth
	}
	return width
}

// tailGitUser is the identity bd records for a write with no explicit actor.
// An events line whose actor is this one is the operator's own write, and the
// default view drops it; "" when the repo has no user.name, which keeps every
// actor.
func tailGitUser(townRoot string) string {
	repo, err := version.GetRepoRootForTown(townRoot)
	if err != nil {
		return ""
	}
	name, err := git.NewGit(repo).ConfigGet("user.name")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(name)
}

// tailSummaryTracker prints the change-driven state line gt tail -f carries:
// the summary line reprints when one of its fields moved, and at most once
// every interval. It never runs for a non-following run — that run has no
// "since the last line" to summarize.
//
// Reading the town's state is a handful of bd and git calls, so the tracker
// makes them no more often than the interval says, not on every poll: inside
// the window the line it last printed still stands.
type tailSummaryTracker struct {
	read     func() tailSummaryFields
	now      func() time.Time
	interval time.Duration
	width    int

	last   string    // the text last printed, "" until the first line
	readAt time.Time // when the state was last read
}

// next returns the summary line to print and whether it is due.
func (t *tailSummaryTracker) next() (tailLine, bool) {
	if t == nil || t.read == nil {
		return tailLine{}, false
	}
	now := t.now()
	if !t.readAt.IsZero() && now.Sub(t.readAt) < t.interval {
		return tailLine{}, false
	}
	t.readAt = now
	text := t.read().line()
	if text == "" {
		return tailLine{}, false
	}
	if t.width > 0 {
		text = tailTruncateDisplay(text, t.width)
	}
	if text == t.last {
		return tailLine{}, false
	}
	t.last = text
	return tailLine{At: now, Rig: "town", Kind: tailKindDaemon, Text: text, Plain: true}, true
}

// tailViewOptions is what the flags ask the view to do.
type tailViewOptions struct {
	all        bool // show the routine lines
	iso        bool // RFC3339 times with the date and zone
	fullSource bool // keep the <rig> <kind> columns
	decorate   bool // color the lines and tag them with an emoji
	// width is the column the default view keeps every line inside (0 = no
	// bound); gitUser is the identity whose actor= field the default view
	// drops; beads resolves the title a bead line carries. now is the clock
	// the update collapse reads to decide a folded run has gone quiet; nil is
	// time.Now.
	width   int
	gitUser string
	beads   *tailBeads
	now     func() time.Time
}

// newTailView is the view the flags select: the routine lines hidden unless
// all, the time as HH:MM:SS unless iso, one short source tag unless
// fullSource, and plain text unless decorate. The title and the two trimmed
// fields belong to that default view too: --all and --verbose print the raw
// line.
//
// The two behaviors that fold lines and annotate them — the update collapse,
// the spec-tick latch, the comment text and the submitted line — are the plain
// default view's alone. --iso keeps the stream as it was, and --all keeps every
// raw line (gt-vxr95).
func newTailView(loc *time.Location, o tailViewOptions) tailView {
	v := tailView{Loc: loc, Layout: tailClockLayout, FullSource: o.fullSource, Width: o.width}
	if o.iso {
		v.Layout = time.RFC3339
	}
	if o.decorate {
		v.Decor = newTailDecor()
	}
	if !o.all {
		// The latches are the view's: they remember the lines this view
		// showed, not ones an earlier run showed. The titles are its too —
		// --all and --verbose print the raw line — and a run that asks for
		// neither pays for no bead read at all.
		filter := &tailDefaultFilter{foldTicks: !o.iso}
		v.Show = filter.visible
		v.Trim = true
		v.GitUser = o.gitUser
		v.Beads = o.beads
		if !o.iso {
			now := o.now
			if now == nil {
				now = time.Now
			}
			v.newAnnotations = true
			v.collapse = &tailUpdateCollapse{window: tailUpdateWindow, now: now}
		}
	}
	return v
}

// tailColorMode is what --color selects.
type tailColorMode string

const (
	tailColorAuto   tailColorMode = "auto"
	tailColorNever  tailColorMode = "never"
	tailColorAlways tailColorMode = "always"
)

// parseTailColor reads --color.
func parseTailColor(s string) (tailColorMode, error) {
	switch mode := tailColorMode(strings.TrimSpace(s)); mode {
	case tailColorAuto, tailColorNever, tailColorAlways:
		return mode, nil
	default:
		return "", fmt.Errorf("--color %q: want auto, never or always", s)
	}
}

// tailDecorate reports whether the stream colors its lines and tags them with
// an emoji. auto follows the terminal conventions the rest of the CLI uses
// (NO_COLOR, CLICOLOR, GT_NO_EMOJI, and stdout being a terminal); the
// explicit modes answer for the operator instead.
func tailDecorate(mode tailColorMode) bool {
	switch mode {
	case tailColorNever:
		return false
	case tailColorAlways:
		return true
	}
	return ui.ShouldUseColor() && ui.ShouldUseEmoji()
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
	// beads resolves the titles and verdicts an events line carries
	// (openTailBeads in production, nil in a test that wants neither).
	beads *tailBeads
	// deploys tracks each bead's dispatch-to-deploy time off the daemon log
	// (nil in a test that wants the raw daemon source).
	deploys *tailDeploys
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
			Text: fmt.Sprintf("cannot read the rig registry (%v): showing hq, the daemon and the watch feed only", regErr)})
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
			sources = append(sources, &eventsSource{rig: rig, journal: j, openErr: openErr, cutoff: o.cutoff, now: o.now, beads: o.beads})
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
		var rigFilter *regexp.Regexp
		if o.rig != "" {
			rigFilter = tailRigFilter(o.rig)
		}
		rigs := loadTailRigs(o.townRoot)
		if o.deploys == nil {
			sources = append(sources, &daemonSource{dir: filepath.Join(o.townRoot, "daemon"), cutoff: o.cutoff, loc: o.loc, now: o.now, rigFilter: rigFilter, rigName: o.rig, rigs: rigs})
		} else {
			// The tracker reads an hour back whatever --since says, so the
			// source cannot carry the rig filter: the wrapper applies both
			// the cutoff and the filter to the lines the stream shows.
			d := &daemonSource{dir: filepath.Join(o.townRoot, "daemon"), cutoff: o.deploys.logCutoff(o.cutoff), loc: o.loc, now: o.now, rigs: rigs}
			sources = append(sources, &tailDeploySource{inner: d, track: o.deploys, from: o.cutoff, rigFilter: rigFilter, rigName: o.rig})
		}
	}
	if o.kinds[tailKindWatch] {
		// The feed is the town's, not a store's; it carries no rig for --rig to
		// select on, so it is read the same whatever --rig says.
		sources = append(sources, newTailWatchSource(o.townRoot, o.cutoff, o.now))
	}
	return sources, preface, nil
}

// tailView is how the stream is shown: the zone and layout of each line's
// time (RFC3339 when Layout is empty), which lines print and what they say
// (all, unchanged, when Show is nil), how the source is written (one short
// tag unless FullSource), and how the line is drawn (plain unless Decor).
//
// Width bounds a line to a terminal column (0 = no bound), and Trim drops the
// events cursor and the operator's own identity — the default view's doing,
// which --verbose and --all turn off. Beads is where a bead line's title comes
// from; nil, in a test or a store that cannot be read, prints no title.
type tailView struct {
	Loc        *time.Location
	Layout     string
	Show       func(tailLine) (tailLine, bool)
	FullSource bool
	Decor      *tailDecor
	Width      int
	Trim       bool
	GitUser    string
	Beads      *tailBeads
	// collapse folds a bead's repeated status updates into one line, and
	// newAnnotations prints the comment text and the submitted line the
	// events source resolved. Both are the plain default view's; --iso,
	// --all and --verbose leave them nil and false (gt-vxr95).
	collapse       *tailUpdateCollapse
	newAnnotations bool
}

// foldUpdates folds a poll's batch of repeated bead status updates into one
// line each; final is the stream's end, which prints a trailing run in place.
// A view with no collapse returns the batch unchanged.
func (v tailView) foldUpdates(batch []tailLine, final bool) []tailLine {
	if v.collapse == nil {
		return batch
	}
	return v.collapse.fold(batch, final)
}

// drainUpdates returns the folded run that has gone quiet, if one has. A run
// the stream's end still holds is printed by foldUpdates with final set.
func (v tailView) drainUpdates() []tailLine {
	if v.collapse == nil {
		return nil
	}
	return v.collapse.drain()
}

// tailAnnotationSep joins a line's text to the title or the verdict it
// carries.
const tailAnnotationSep = " · "

// tailClockLayout is the default time column: the day is the operator's own.
const tailClockLayout = "15:04:05"

// The emoji a class is tagged with. They are the operator's own vocabulary
// for the stream, not the CLI's icon set: ✅ for something that landed, ❌ for
// something that broke, ⚠️ for something to look at, 🛬 for a landing still
// in the air, 🚀 for a dispatch, 🔁 for a restart.
const (
	tailIconSuccess  = "✅"
	tailIconFailure  = "❌"
	tailIconWarning  = "⚠️"
	tailIconLanding  = "🛬"
	tailIconDispatch = "🚀"
	tailIconRestart  = "🔁"
)

// tailDecor draws a line's class: the color its text takes and the emoji that
// tags it. A nil decor draws neither, which is what a pipe gets.
//
// The styles come from internal/style, so gt tail colors the town the way the
// rest of the CLI does. They are bound to a renderer gt tail owns rather than
// the process-wide one, because --color=always has to draw color on a stream
// lipgloss would otherwise decide is not a terminal.
type tailDecor struct {
	classes [tailClassCount]tailClassDraw
}

// tailClassDraw is how one class is drawn. The plain class carries no emoji:
// the routine lines stay quiet, and are dimmed so a classified line stands
// out of them.
type tailClassDraw struct {
	icon  string
	style lipgloss.Style
}

func newTailDecor() *tailDecor {
	r := lipgloss.NewRenderer(os.Stdout)
	r.SetColorProfile(termenv.TrueColor)
	r.SetHasDarkBackground(ui.HasDarkBackground())
	return &tailDecor{classes: [tailClassCount]tailClassDraw{
		tailClassPlain:    {style: style.Dim.Renderer(r)},
		tailClassSuccess:  {icon: tailIconSuccess, style: style.Success.Renderer(r)},
		tailClassFailure:  {icon: tailIconFailure, style: style.Error.Renderer(r)},
		tailClassWarning:  {icon: tailIconWarning, style: style.Warning.Renderer(r)},
		tailClassLanding:  {icon: tailIconLanding, style: style.Info.Renderer(r)},
		tailClassDispatch: {icon: tailIconDispatch, style: style.Info.Renderer(r)},
		tailClassRestart:  {icon: tailIconRestart, style: style.Warning.Renderer(r)},
	}}
}

// icon is the emoji the class is tagged with, or "" (the plain class, or a
// view that draws no decor).
func (d *tailDecor) icon(c tailClass) string {
	if d == nil {
		return ""
	}
	return d.classes[c].icon
}

// paint draws text in the class's color, or returns it unchanged when the
// view draws no decor.
func (d *tailDecor) paint(c tailClass, text string) string {
	if d == nil {
		return text
	}
	return d.classes[c].style.Render(text)
}

// pollTailSources polls every source at once and returns each one's batch in
// source order, so a store's bd call does not wait on the one before it. The
// sources answer from disk (the daemon log, the landings files) first, and
// each bd read takes as long as its own budget allows.
func pollTailSources(sources []tailSource) [][]tailLine {
	out := make([][]tailLine, len(sources))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i, s := range sources {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lines := s.Poll()
			mu.Lock()
			defer mu.Unlock()
			out[i] = lines
		}()
	}
	wg.Wait()
	return out
}

// runTailStream polls every source and prints the merged batch; with follow
// it polls again on each tick until ctx ends. The sources are polled
// concurrently and each poll's batch is merged and sorted on its own, so the
// first poll of a follow prints one backlog in time order across every source
// and a later line prints after the lines already shown. A failed write (a
// closed pipe) ends the stream with its error.
func runTailStream(ctx context.Context, w io.Writer, sources []tailSource, preface []tailLine, follow bool, tick <-chan time.Time, view tailView, summary *tailSummaryTracker) error {
	bw := bufio.NewWriter(w)
	writeLine := func(l tailLine) error {
		_, err := bw.WriteString(view.renderLine(l) + "\n")
		return err
	}
	// show prints one poll: the batch, folded and filtered, then the folded
	// update runs that poll brought due. final is the stream's end, which drains
	// every run still open. Only the view folds, so a view that keeps every line
	// passes the batch through untouched.
	show := func(lines []tailLine, final bool) error {
		for _, l := range view.foldUpdates(mergeTail(lines), final) {
			if view.Show != nil {
				var ok bool
				if l, ok = view.Show(l); !ok {
					continue
				}
			}
			if err := writeLine(l); err != nil {
				return err
			}
		}
		for _, l := range view.drainUpdates() {
			if err := writeLine(l); err != nil {
				return err
			}
		}
		return bw.Flush()
	}
	// The summary is not a source: it is the state those lines are happening
	// to, so it is not filtered, and its own tracker decides when it is due.
	emitSummary := func() error {
		line, ok := summary.next()
		if !ok {
			return nil
		}
		if err := writeLine(line); err != nil {
			return err
		}
		return bw.Flush()
	}
	emit := func(extra []tailLine, final bool) error {
		all := extra
		for _, b := range pollTailSources(sources) {
			all = append(all, b...)
		}
		return show(all, final)
	}
	if !follow {
		return emit(preface, true)
	}
	if err := emit(preface, false); err != nil {
		return err
	}
	if err := emitSummary(); err != nil {
		return err
	}
	for {
		select {
		case <-ctx.Done():
			if err := show(nil, true); err != nil {
				return err
			}
			return nil
		case <-tick:
			if err := emit(nil, false); err != nil {
				return err
			}
			if err := emitSummary(); err != nil {
				return err
			}
		}
	}
}

// tailLine is one line of the gt tail stream: when it happened, which rig it
// belongs to ("hq" for the town store, the rig a daemon line names or routes
// to, and "town" for the daemon's town-wide lines), which source produced it,
// and the text. Render turns it into
// "<local RFC3339 time> <rig> <kind> <text>".
//
// Title, Verdict, Comment and Submit are the annotations a line can carry,
// written after the text with " · ". Title is the bead title the default view
// looks up for a create, close, status change or dependency line; Verdict is
// the review loop's own words, read off a comment or a note event and carried by
// the source so --all shows it too. Comment is a comment's own text, cut short,
// and Submit is the READY TO LAND block a submission wrote — both are the plain
// default view's to print. The more specific annotation wins: a verdict over a
// submitted or a comment line, and those over the title.
//
// Plain draws the line dim whatever its text says. It is the summary line's:
// a state line about the town is not one of the town's own lines, so the
// classifier's reading of a word it carries — "escalations", say — is not the
// class it is drawn in.
//
// Class is how the line's source fixed its class, for a line whose own field
// says how it should be drawn whatever its words say: a watch alert draws by
// its severity, and an alert's text naming a failure it is not must not
// repaint it. The zero class means the source fixed none.
type tailLine struct {
	At      time.Time
	Rig     string
	Kind    string
	Text    string
	Title   string
	Verdict string
	Comment string
	Submit  string
	Plain   bool
	Class   tailClass
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
	// tailKindWatch is both the kind and the tag of a watch line: the feed is
	// the town's, not one store's, so it prints no rig.
	tailKindWatch = "watch"
)

var tailKinds = []string{tailKindEvents, tailKindLandings, tailKindDaemon, tailKindWatch}

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

// renderLine formats one line: the time in the view's layout (RFC3339 when
// empty), the source, and the text. The source is one short tag — the line's
// rig, "town" for the daemon's town-wide lines — or the "<rig> <kind>" pair
// when the view keeps the full source. A decorated view puts the class's emoji
// between the source and the text and draws the text in the class's color.
//
// The rig and kind are single tokens, and the text and the annotation it
// carries are passed through tailText, so every record is exactly one line
// and, with --verbose, the first three fields can be cut or grepped by
// position.
func (v tailView) renderLine(l tailLine) string {
	if v.Trim {
		l = tailTrimEventFields(l, v.GitUser)
	}
	annotated := v.newAnnotations && (l.Submit != "" || l.Comment != "")
	if l.Title == "" && l.Verdict == "" && !annotated && v.Beads != nil {
		l.Title = v.Beads.lineTitle(l)
	}
	layout := v.Layout
	if layout == "" {
		layout = time.RFC3339
	}
	var b strings.Builder
	b.WriteString(l.At.In(v.Loc).Format(layout))
	b.WriteByte(' ')
	b.WriteString(tailToken(l.Rig))
	if v.FullSource {
		b.WriteByte(' ')
		b.WriteString(tailToken(l.Kind))
	}
	class := tailLineClass(l)
	if icon := v.Decor.icon(class); icon != "" {
		b.WriteByte(' ')
		b.WriteString(icon)
	}
	text := tailText(l.Text)
	annotation := l.Verdict
	if annotation == "" && v.newAnnotations {
		if l.Submit != "" {
			annotation = l.Submit
		} else {
			annotation = l.Comment
		}
	}
	if annotation == "" {
		annotation = l.Title
	}
	if annotation != "" {
		// A bead's title and the review loop's words are bead text: they
		// arrive after tailText has sanitized the line's own copy, so they
		// need the same pass. A title carrying a newline or an escape would
		// otherwise forge a second line or move the operator's cursor.
		annotation = tailText(annotation)
		// A line with no room for the annotation keeps its own words: the
		// separator goes with the annotation, not before it.
		if annotation = tailAnnotation(text, b.String()+" ", annotation, v.Width); annotation != "" {
			text += tailAnnotationSep + annotation
		}
	}
	b.WriteByte(' ')
	b.WriteString(v.Decor.paint(class, text))
	return b.String()
}

// tailLineClass is the class a line is drawn in. Plain wins: the summary line
// is a state line about the town, not one of the town's own lines, so the
// classifier's reading of a word it carries ("escalations", say) is not the
// class it is drawn in. A class its source fixed — a watch alert's severity —
// is next, then a verdict's own colors; everything else is what its text says.
func tailLineClass(l tailLine) tailClass {
	switch {
	case l.Plain:
		return tailClassPlain
	case l.Class != tailClassPlain:
		return l.Class
	case l.Verdict != "":
		return tailVerdictClass(l.Verdict)
	}
	return tailClassOf(l.Text)
}

// tailAnnotation fits an annotation to the room left on a line: the column
// bound minus everything already written (prefix, its trailing space, the
// line's own text) and minus the separator. A line with no bound, or one too
// narrow to hold any annotation at all, carries it whole or not at all.
func tailAnnotation(text, prefix, annotation string, width int) string {
	if width <= 0 {
		return annotation
	}
	room := width - lipgloss.Width(prefix) - lipgloss.Width(text) - lipgloss.Width(tailAnnotationSep)
	if room <= 0 {
		return ""
	}
	return tailTruncateDisplay(annotation, room)
}

// tailTruncateDisplay cuts text to at most width terminal columns, marking a
// cut. It counts what the terminal draws, so a wide rune costs two.
func tailTruncateDisplay(text string, width int) string {
	if lipgloss.Width(text) <= width {
		return text
	}
	var b strings.Builder
	used := 0
	for _, r := range text {
		w := lipgloss.Width(string(r))
		if used+w > width-1 {
			return b.String() + "…"
		}
		b.WriteRune(r)
		used += w
	}
	return b.String()
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
