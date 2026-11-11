package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/jev/scorecard"
	"github.com/steveyegge/gastown/internal/workspace"
)

var (
	jevScorecardBattery string
	jevScorecardSince   string
	jevScorecardJSON    bool
	jevScorecardLines   bool
	jevScorecardRefresh bool
)

var jevScorecardCmd = &cobra.Command{
	Use:   "scorecard",
	Short: "Score a battery's answers against its labels",
	Long: `Read a battery's JSONL log through its registered adapter and print, for each
question, how Jev's answers agree with the labels the adapter finds: lines
logged and labeled, positives and negatives, precision and recall (at 0.5 for a
noul, at the chosen option for a choice), the cut with the best F1, five
calibration bands, and a verdict. A score question reports the mean score by
label and the best cut on the score in place of precision, recall and bands.

The verdict is earning on at least 30 labeled lines, not-yet below 30 labeled
lines, and no-signal otherwise. A noul or choice earns when its precision at
the best cut beats the base rate by 0.1 (twice the base under 0.1) and the cut
flags under 90% of the lines; a score earns when the best F1 beats the base
rate by 0.1. Scoring one battery is read-only; only --refresh writes.

--battery is required, and an unknown name is an error that lists the
registered batteries. --since keeps only the lines stamped in the window ending
now, given as Nd for days or a Go duration such as 36h. --json prints the same
report as JSON. The testsel battery asks about every package of every change, so
its --json report leaves out the per-item rows (the totals stay); --lines prints
them.

--refresh replaces --battery, --json and --lines: it scores every registered
battery over --since (7d when empty), except that it reuses a battery's cached
block when the log holds the same number of lines and bytes as when that block
was scored, the block is under three days old, and it was scored over the same
window. It keeps the cached block of a battery whose scoring failed, and writes
<town>/.runtime/jev/scorecard.json by temp file and rename, so a failed write
leaves the previous file as it was. A refresh that wrote the file exits zero
only when every battery scored or was reused; otherwise it names the failed
batteries and exits non-zero with their last good blocks kept in the file.

Examples:
  gt jev scorecard --battery fixture
  gt jev scorecard --battery fixture --since 14d --json
  gt jev scorecard --refresh`,
	Args: cobra.NoArgs,
	RunE: runJevScorecard,
}

func init() {
	f := jevScorecardCmd.Flags()
	f.StringVar(&jevScorecardBattery, "battery", "", "Battery to score (required unless --refresh)")
	f.StringVar(&jevScorecardSince, "since", "", "Score only lines from this window ending now: Nd or a Go duration (default: all time, or 7d with --refresh)")
	f.BoolVar(&jevScorecardJSON, "json", false, "Print the report as JSON")
	f.BoolVar(&jevScorecardLines, "lines", false, "With --json, print every item's labeling for a battery that leaves them out by default")
	f.BoolVar(&jevScorecardRefresh, "refresh", false, "Score every registered battery and write <town>/.runtime/jev/scorecard.json")
	jevCmd.AddCommand(jevScorecardCmd)
}

func runJevScorecard(cmd *cobra.Command, _ []string) error {
	townRoot, err := workspace.FindFromCwdOrError()
	if err != nil {
		return fmt.Errorf("finding the town root: %w", err)
	}
	o := jevScorecardOptions{
		Battery: jevScorecardBattery,
		Since:   jevScorecardSince,
		JSON:    jevScorecardJSON,
		Lines:   jevScorecardLines,
		Refresh: jevScorecardRefresh,
	}
	return jevScorecardRun(cmd.Context(), cmd.OutOrStdout(), townRoot, o, time.Now(), scoreJevBattery, scorecard.Cache{})
}

// jevScorecardOptions is the command's flags, passed as values so the dispatch
// below can be tested without the process's globals.
type jevScorecardOptions struct {
	Battery string
	Since   string
	JSON    bool
	Lines   bool
	Refresh bool
}

// jevBatteryScorer builds one battery's report over the window starting at
// from. It is a parameter so a test can stand in for the adapters.
type jevBatteryScorer func(ctx context.Context, name, townRoot string, from time.Time) (scorecard.Report, error)

// jevScorecardRun dispatches one invocation of the command: a single battery's
// report, or a refresh of every registered battery into the cache.
func jevScorecardRun(ctx context.Context, w io.Writer, townRoot string, o jevScorecardOptions, now time.Time,
	score jevBatteryScorer, cache scorecard.Cache) error {
	if !o.Refresh {
		if o.Battery == "" {
			return fmt.Errorf("--battery is required unless --refresh is given")
		}
		return jevScorecardWithFlags(ctx, w, townRoot, o.Battery, o.Since, o.JSON, o.Lines, now)
	}
	if o.Battery != "" {
		return fmt.Errorf("--refresh scores every registered battery; drop --battery %q", o.Battery)
	}
	if o.JSON || o.Lines {
		return fmt.Errorf("--refresh writes the scorecard cache; --json and --lines apply only to --battery")
	}
	// The declared list is what the patrol checks against this command's
	// registry; a name the binary does not register is refused here, naming
	// it, rather than read as a battery that costs nothing (gt-jk0162).
	if err := validateJevScorecardBlock(townRoot); err != nil {
		return err
	}
	return jevScorecardRefreshWith(ctx, w, townRoot, o.Since, now, score, cache)
}

// scoreJevBattery scores one registered battery through its adapter over the
// window starting at from.
func scoreJevBattery(ctx context.Context, name, townRoot string, from time.Time) (scorecard.Report, error) {
	a, err := scorecard.Open(name, townRoot)
	if err != nil {
		return scorecard.Report{}, err
	}
	return scorecard.Build(ctx, a, from)
}

// jevScorecardRefreshSince is the window a refresh scores when --since is not
// given: a refresh is the periodic review of the batteries, so it defaults to a
// week where a single battery's score still defaults to all time.
const jevScorecardRefreshSince = "7d"

// jevBatteryLogFingerprint measures the log of the battery registered under
// name for the town at townRoot. A refresh reuses a cached block while this
// fingerprint is unchanged.
func jevBatteryLogFingerprint(name, townRoot string) (scorecard.LogFingerprint, error) {
	a, err := scorecard.Open(name, townRoot)
	if err != nil {
		return scorecard.LogFingerprint{}, err
	}
	return scorecard.FingerprintLog(a.LogPath())
}

// jevScorecardRefreshWith scores every registered battery over the window,
// reuses each battery's cached block whose log has not changed and whose block
// is still young enough, keeps each failed battery's last good cached block,
// and writes the result to the town's cache path. A corrupt previous cache
// stops the refresh before it scores anything; a missing one starts fresh.
// Every battery's failure still writes the file, then names the failed
// batteries in the returned error so the caller's exit status says the cache is
// not wholly fresh.
func jevScorecardRefreshWith(ctx context.Context, w io.Writer, townRoot, since string, now time.Time,
	score jevBatteryScorer, cache scorecard.Cache) error {
	window := since
	if window == "" {
		window = jevScorecardRefreshSince
	}
	from, err := parseSince(window, now)
	if err != nil {
		return err
	}
	path := scorecard.CachePath(townRoot)
	prev, err := cache.Read(path)
	if err != nil && !errors.Is(err, scorecard.ErrNoScorecard) {
		return fmt.Errorf("refreshing the scorecard: %w", err)
	}
	doc := scorecard.CacheDoc{ScoredAt: now, Since: window, Batteries: make([]scorecard.BatteryBlock, 0, len(scorecard.Registered()))}
	var failed, reused []string
	for _, name := range scorecard.Registered() {
		// The fingerprint is taken before scoring: a line appended while the
		// battery is being scored then leaves the fingerprint smaller than
		// what was scored, so the next refresh sees a change and scores
		// again. Recording it afterwards could claim lines the report never
		// saw.
		fp, fpErr := jevBatteryLogFingerprint(name, townRoot)
		if fpErr == nil {
			if block, ok := scorecard.ReusableBlock(prev, name, fp, window, now); ok {
				doc.Batteries = append(doc.Batteries, block)
				reused = append(reused, name)
				continue
			}
		}
		block := scorecard.BatteryBlock{Name: name, ScoredAt: now, LogLines: fp.Lines, LogBytes: fp.Bytes}
		if fpErr != nil {
			// A log that cannot be measured is recorded as -1 lines and -1
			// bytes, which no measurement can equal, rather than as the zero
			// fingerprint, which is exactly what an empty log measures: a
			// block scored while the log was unreadable would otherwise be
			// reused, unmeasured, the first time that log is empty.
			block.LogLines, block.LogBytes = -1, -1
		}
		rep, err := score(ctx, name, townRoot, from)
		if err != nil {
			block.Failed, block.Error = true, err.Error()
			failed = append(failed, name)
		} else {
			block.Report = &rep
		}
		doc.Batteries = append(doc.Batteries, scorecard.KeepFailed(prev, block))
	}
	if err := cache.Write(path, doc); err != nil {
		return err
	}
	fmt.Fprintf(w, "wrote %s: %d batteries, %d reused, %d failed\n", path, len(doc.Batteries), len(reused), len(failed))
	if len(failed) > 0 {
		return fmt.Errorf("scoring failed for %s; the cache keeps each battery's last good block", strings.Join(failed, ", "))
	}
	return nil
}

// jevScorecardWithFlags resolves the window and the battery, then prints the
// report for the town at townRoot.
func jevScorecardWithFlags(ctx context.Context, w io.Writer, townRoot, battery, since string, asJSON, withLines bool, now time.Time) error {
	return jevScorecardWithOpener(ctx, w, townRoot, battery, since, asJSON, withLines, now, scorecard.Open)
}

// jevScorecardWithOpener is jevScorecardWithFlags with the adapter opener
// passed in, so a test can put fakes behind an adapter that reads the rig's
// beads.
func jevScorecardWithOpener(ctx context.Context, w io.Writer, townRoot, battery, since string, asJSON, withLines bool, now time.Time,
	open func(battery, townRoot string) (scorecard.Adapter, error)) error {
	from, err := parseSince(since, now)
	if err != nil {
		return err
	}
	a, err := open(battery, townRoot)
	if err != nil {
		return err
	}
	rep, err := scorecard.Build(ctx, a, from)
	if err != nil {
		return err
	}
	if asJSON {
		if _, bulky := a.(scorecard.Bulky); bulky && !withLines {
			rep.Lines, rep.LinesOmitted = nil, true
		}
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(rep)
	}
	return scorecard.WriteText(w, rep)
}

// parseSince returns the start of the --since window ending at now. An empty
// value is all time, which is the zero time.
func parseSince(s string, now time.Time) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	var d time.Duration
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 1 {
			return time.Time{}, fmt.Errorf("--since %q: days must be a whole number of at least 1", s)
		}
		d = time.Duration(n) * 24 * time.Hour
	} else {
		parsed, err := time.ParseDuration(s)
		if err != nil || parsed <= 0 {
			return time.Time{}, fmt.Errorf("--since %q: want Nd or a positive Go duration such as 36h", s)
		}
		d = parsed
	}
	return now.Add(-d), nil
}
