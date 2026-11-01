package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/metrics"
	"github.com/steveyegge/gastown/internal/workspace"
)

var metricsShapeRig string

func init() {
	metricsShapeOutcomeCmd.Flags().StringVar(&metricsShapeRig, "rig", "gt", "Database in the archive whose issues and events are read")
	metricsCmd.AddCommand(metricsShapeOutcomeCmd)
}

var metricsShapeOutcomeCmd = &cobra.Command{
	Use:   "shape-outcome",
	Short: "Show whether a bead's first-filed shape predicts its rounds and approval",
	Long: `Reads the tracker's 15-minute git export at <town>/.dolt-archive/git and the
town's review log at <town>/.runtime/landings/reviews.jsonl. For each bead the
sampled export commits hold, it takes the earliest version of the description
and acceptance criteria it can see, counts the shape features (acceptance
boxes, constraint words, whether Size says one worker, files named, description
length), joins them to the landing's outcome (rounds, last score, first-review
approval and time from creation to closing), and prints one row per feature
bucket with its n.

One export commit is sampled per local day, the day's last, because reading
every 15-minute commit is thousands of git shows for the same answer. A bead
filed and edited within one day therefore reads as its edited version, and
those beads are counted as edited_same_day. Buckets under 20 beads are marked
thin. The export is read only: git log, git show and git ls-tree, never a write.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		town, err := workspace.FindFromCwdOrError()
		if err != nil {
			return err
		}
		return runMetricsShapeOutcome(cmd.Context(), cmd.OutOrStdout(), shapeArchiveOSEnv(town, metricsShapeRig))
	},
}

// shapeArchiveEnv is everything the command reads outside its own process: the
// archive through a runner, the review log through ReadFile, and the town. A
// test passes fakes, so no git runs in the unit tier.
type shapeArchiveEnv struct {
	Town     string
	Rig      string
	ReadFile func(string) ([]byte, error)
	Run      metrics.Runner
}

// shapeArchiveOSEnv wires the real archive runner: git with no environment
// beyond the allowlist, so the export is read the way every other source reads
// it.
func shapeArchiveOSEnv(town, rig string) shapeArchiveEnv {
	return shapeArchiveEnv{
		Town:     town,
		Rig:      rig,
		ReadFile: os.ReadFile,
		Run:      metrics.OSRunner(metrics.DefaultAllowEnv),
	}
}

// shapeRigNameRE is the archive directory name the rig may be: a database name,
// which is alphanumeric with underscores and hyphens (see the daemon's
// validDBName). It is refused otherwise before it reaches a path or a git path
// spec, so a name like "../x" can never read outside the archive.
var shapeRigNameRE = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// shapeEventShardRE matches a rig's event files in the archive: events.jsonl
// and its numbered shards.
var shapeEventShardRE = regexp.MustCompile(`^events(\.[0-9]+)?\.jsonl$`)

// shapeCommitRE is what git log's commit id may be before it is spliced into a
// git path spec. git prints 40 lowercase hex digits; anything else is not a
// commit and never reaches argv, where a leading dash would read as a flag.
var shapeCommitRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

func runMetricsShapeOutcome(ctx context.Context, out io.Writer, env shapeArchiveEnv) error {
	if env.Run == nil {
		return fmt.Errorf("shape-outcome: no runner: the archive is read only through one")
	}
	if env.ReadFile == nil {
		return fmt.Errorf("shape-outcome: no file reader: the review log is read through one")
	}
	rig := env.Rig
	if rig == "" {
		rig = "gt"
	}
	if !shapeRigNameRE.MatchString(rig) {
		return fmt.Errorf("shape-outcome: rig %q is not a database name", rig)
	}
	repo := filepath.Join(env.Town, ".dolt-archive", "git")
	dropped := &metrics.Dropped{}

	commits, err := shapeSampledCommits(ctx, env, repo, rig, dropped)
	if err != nil {
		return err
	}
	if len(commits) == 0 {
		return fmt.Errorf("shape-outcome: %s holds no commit for %s/issues.jsonl", repo, rig)
	}

	first, err := shapeFirstFiled(ctx, env, repo, rig, commits, dropped)
	if err != nil {
		return err
	}

	reviewsPath := land.TownReviewsFile(env.Town).Path
	reviewBytes, err := env.ReadFile(reviewsPath)
	if err != nil {
		return fmt.Errorf("shape-outcome: reading the review log: %w", err)
	}
	reviews := metrics.DecodeShapeReviews(strings.Split(string(reviewBytes), "\n"), reviewsPath, dropped)

	events, err := shapeArchiveEvents(ctx, env, repo, rig)
	if err != nil {
		return err
	}
	hours := metrics.ArchiveCloseHours(events, rig+"/events.jsonl", dropped)
	outcomes := metrics.ShapeOutcomes(reviews, hours)

	features := make([]metrics.Shape, 0, len(first))
	for _, f := range first {
		features = append(features, metrics.ShapeFeatures(f.Row))
	}
	buckets := metrics.BucketOutcomes(features, outcomes)

	reviewed := make(map[string]bool, len(reviews))
	for _, r := range reviews {
		reviewed[r.Bead] = true
	}
	joined, noReview, reviewedNotLanded, editedSameDay := 0, 0, 0, 0
	for _, f := range first {
		if _, ok := outcomes[f.Row.ID]; ok {
			joined++
		} else if reviewed[f.Row.ID] {
			reviewedNotLanded++
		} else {
			noReview++
		}
		if f.EditedSameDay {
			editedSameDay++
		}
	}
	filed := shapeIDs(first)
	landedWithoutText := 0
	for id := range outcomes {
		if _, ok := filed[id]; !ok {
			landedWithoutText++
		}
	}

	window := commits[0].At.Format(time.DateOnly) + ".." + commits[len(commits)-1].At.Format(time.DateOnly)
	if _, err := fmt.Fprintf(out,
		"shape-outcome %s: %d sampled day(s) %s, %d first-filed bead(s); %d joined to a landing, %d with no review, %d reviewed but not landed, %d landed without a first-filed row; %d read from their filed day's last export (edited_same_day); %d row(s) dropped\n",
		rig, len(commits), window, len(first), joined, noReview, reviewedNotLanded, landedWithoutText, editedSameDay, dropped.Count()); err != nil {
		return err
	}
	return printShapeBuckets(out, buckets)
}

// shapeIDs is the set of beads the archive's first-filed rows name.
func shapeIDs(first []metrics.FirstFiled) map[string]struct{} {
	ids := make(map[string]struct{}, len(first))
	for _, f := range first {
		ids[f.Row.ID] = struct{}{}
	}
	return ids
}

// shapeLogCommit is one commit that touched the archive's issues.jsonl: its id
// and the day it was committed, in the committer's own zone.
type shapeLogCommit struct {
	SHA string
	At  time.Time
}

// shapeSampledCommits lists the commits that touched the rig's issues.jsonl and
// keeps one per local day, oldest first.
//
// git log prints newest first, so the first commit seen for a day is that
// day's last export: the most complete version of the day, and the latest
// point at which a bead filed that day could already have been edited. The
// list is reversed before it is returned, because FirstFiledVersions reads
// successive commits oldest first.
func shapeSampledCommits(ctx context.Context, env shapeArchiveEnv, repo, rig string, dropped *metrics.Dropped) ([]shapeLogCommit, error) {
	// The committer date travels beside the id in one call: pairing a second
	// log of dates with this one by position would be a second ordering to get
	// wrong.
	raw, err := env.Run(ctx, []string{
		"git", "-C", repo, "log", "--first-parent", "--format=%H %cI", "--", rig + "/issues.jsonl",
	})
	if err != nil {
		return nil, fmt.Errorf("shape-outcome: reading %s: %w", filepath.Join(repo, rig, "issues.jsonl"), err)
	}
	var sampled []shapeLogCommit
	seenDay := make(map[string]bool)
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) != 2 || !shapeCommitRE.MatchString(fields[0]) {
			dropped.Add("git log", "line is not a commit id and a date")
			continue
		}
		at, err := time.Parse(time.RFC3339, fields[1])
		if err != nil {
			dropped.Add("git log", "unparseable committer date")
			continue
		}
		day := at.Format(time.DateOnly)
		if seenDay[day] {
			continue
		}
		seenDay[day] = true
		sampled = append(sampled, shapeLogCommit{SHA: fields[0], At: at})
	}
	for i, j := 0, len(sampled)-1; i < j; i, j = i+1, j-1 {
		sampled[i], sampled[j] = sampled[j], sampled[i]
	}
	return sampled, nil
}

// shapeFirstFiled reads each sampled commit's issues.jsonl and returns the
// first row the archive holds per bead. A commit whose file cannot be read
// fails the run: a silently skipped day is a bead read from a later edit
// without any sign of it.
func shapeFirstFiled(ctx context.Context, env shapeArchiveEnv, repo, rig string, commits []shapeLogCommit, dropped *metrics.Dropped) ([]metrics.FirstFiled, error) {
	batches := make([]metrics.ArchiveCommit, 0, len(commits))
	for _, c := range commits {
		data, err := env.Run(ctx, []string{
			"git", "-C", repo, "show", c.SHA + ":" + rig + "/issues.jsonl",
		})
		if err != nil {
			return nil, fmt.Errorf("shape-outcome: reading %s/issues.jsonl at %s: %w", rig, c.SHA, err)
		}
		batches = append(batches, metrics.ArchiveCommit{
			Day:  c.At.Format(time.DateOnly),
			Rows: metrics.DecodeArchiveIssues(data, c.SHA, dropped),
		})
	}
	return metrics.FirstFiledVersions(batches), nil
}

// shapeArchiveEvents reads the rig's event files at the archive's HEAD: the
// export writes the whole events table each cycle, so HEAD holds every created
// and closed stamp the export has. The event table is pruned to its retention
// window before it is exported, so a bead older than that window has no
// created stamp and gets no time to close; the table's hours_n shows how many
// beads the window covers. The shard names are listed rather than assumed, so
// a sharded export is read whole. An archive with no event file is an error:
// the time-to-close join would otherwise be silently absent.
func shapeArchiveEvents(ctx context.Context, env shapeArchiveEnv, repo, rig string) ([]byte, error) {
	raw, err := env.Run(ctx, []string{"git", "-C", repo, "ls-tree", "--name-only", "HEAD", rig + "/"})
	if err != nil {
		return nil, fmt.Errorf("shape-outcome: listing %s in the archive: %w", rig, err)
	}
	var names []string
	for _, name := range strings.Fields(string(raw)) {
		rest, underRig := strings.CutPrefix(name, rig+"/")
		if !underRig || !shapeEventShardRE.MatchString(rest) {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("shape-outcome: the archive holds no %s/events*.jsonl, so time to close cannot be joined", rig)
	}
	var events []byte
	for _, name := range names {
		data, err := env.Run(ctx, []string{"git", "-C", repo, "show", "HEAD:" + name})
		if err != nil {
			return nil, fmt.Errorf("shape-outcome: reading %s at HEAD: %w", name, err)
		}
		events = append(events, data...)
		events = append(events, '\n')
	}
	return events, nil
}

// printShapeBuckets writes one row per feature bucket: its n, and the medians
// and shares over the beads in it, with a bucket under ThinBucketN marked
// thin. An empty bucket prints "-" rather than a zero that would read as a
// measured value.
func printShapeBuckets(w io.Writer, buckets []metrics.BucketOutcome) error {
	if _, err := fmt.Fprintf(w, "%-22s %-9s %5s %8s %8s %6s %8s %7s  %s\n",
		"feature", "bucket", "n", "rounds", "approve", "score", "hours", "hours_n", "note"); err != nil {
		return err
	}
	for _, b := range buckets {
		rounds, approve, score, hours := "-", "-", "-", "-"
		note := ""
		if b.N > 0 {
			rounds = fmt.Sprintf("%.2f", b.MedianRounds)
			approve = fmt.Sprintf("%.0f%%", 100*b.ApproveShare)
			score = fmt.Sprintf("%.2f", b.MedianScore)
			if b.HoursN > 0 {
				hours = fmt.Sprintf("%.1f", b.MedianHours)
			}
		}
		if b.Thin {
			note = "thin"
		}
		if _, err := fmt.Fprintf(w, "%-22s %-9s %5d %8s %8s %6s %8s %7d  %s\n",
			b.Feature, b.Label, b.N, rounds, approve, score, hours, b.HoursN, note); err != nil {
			return err
		}
	}
	return nil
}
