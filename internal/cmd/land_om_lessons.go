package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/land"
	"github.com/steveyegge/gastown/internal/omlessons"
	"github.com/steveyegge/gastown/internal/workspace"
)

// landOmLessonsProposeMinFindings is how many findings a class needs in the
// window before --propose drafts a directive edit for it; fewer is too few to say.
const landOmLessonsProposeMinFindings = 5

var (
	landOmLessonsSince      string
	landOmLessonsDays       int
	landOmLessonsRecords    string
	landOmLessonsJSON       bool
	landOmLessonsPropose    bool
	landOmLessonsDirectives string
)

// landOmLessonsCmd counts the rejection classes the weekly lessons pass works
// from. It reads the review log om-stats reads, and writes nothing.
var landOmLessonsCmd = &cobra.Command{
	Use:   "om-lessons",
	Short: "Cluster om's findings into rejection classes, by window",
	Long: `Cluster om's findings into the rejection classes the lessons pass counts.

Reads the town's review records (.runtime/landings/reviews.jsonl) and prints, per
class of the class table, the findings, the beads they were found on and the
landing rounds burned — the attempts past the first on a bead where the class
carried a major or critical finding — in the window and in the window before it,
so a class that grew or shrank reads at a glance.

A window is --days calendar days (7 by default). Without --since it ends today,
so each run counts the week just gone; --since pins its first day.

A finding that matches no keyword of the class table is unclassified, and the
report lists its title: that list is how a class the table is missing shows
itself.

A path given with --records must exist, so a typo is an error. The town's default
may be absent, and a town that has not landed yet is an empty report. A record
written before the log kept finding text carries none; those are counted apart
and fail nothing.

With --propose, the command prints instead one shaped directive-edit bead draft
for each class with at least 5 findings in the window: the counts, three example
bead ids, the rule that covers the class or the word gap, and the landing rounds
it burned. A class under 5 findings is listed as too few to say. The rules come
from the directive files named by --directives, and a directive with no numbered
rules is an error. The drafts go to stdout; the command files nothing.

The command reads only: it writes no file.

Examples:
  gt land om-lessons
  gt land om-lessons --since 2026-10-01 --days 14
  gt land om-lessons --json --records scratch/reviews.jsonl
  gt land om-lessons --propose --directives directives`,
	Args: cobra.NoArgs,
	RunE: runLandOmLessons,
}

func init() {
	landOmLessonsCmd.Flags().StringVar(&landOmLessonsSince, "since", "", "first day of the window, as YYYY-MM-DD (default: a window ending today)")
	landOmLessonsCmd.Flags().IntVar(&landOmLessonsDays, "days", omlessons.DefaultDays, "window length in days; the prior window is the days before it")
	landOmLessonsCmd.Flags().StringVar(&landOmLessonsRecords, "records", "", "review records file (default: <town>/.runtime/landings/reviews.jsonl)")
	landOmLessonsCmd.Flags().BoolVar(&landOmLessonsJSON, "json", false, "print the report as JSON")
	landOmLessonsCmd.Flags().BoolVar(&landOmLessonsPropose, "propose", false, "print a directive-edit bead draft for each class with enough findings, instead of the table")
	landOmLessonsCmd.Flags().StringVar(&landOmLessonsDirectives, "directives", "directives", "directory holding polecat.md and _common.md, read for the rule coverage of --propose")
	landCmd.AddCommand(landOmLessonsCmd)
}

func runLandOmLessons(cmd *cobra.Command, _ []string) error {
	if landOmLessonsPropose {
		if landOmLessonsJSON {
			return errors.New("--propose prints drafts as text: drop --json")
		}
		return landOmLessonsProposeFrom(workspace.FindFromCwdOrError, cmd.OutOrStdout(), time.Now(), landOmLessonsSince, landOmLessonsDays, landOmLessonsRecords, landOmLessonsDirectives)
	}
	return landOmLessonsReport(cmd.OutOrStdout(), time.Now(), landOmLessonsSince, landOmLessonsDays, landOmLessonsRecords, landOmLessonsJSON)
}

// landOmLessonsReport is the command body with the clock named, so a test
// drives both windows without a flag.
func landOmLessonsReport(out io.Writer, now time.Time, sinceValue string, days int, recordsFlag string, asJSON bool) error {
	return landOmLessonsReportFrom(workspace.FindFromCwdOrError, out, now, sinceValue, days, recordsFlag, asJSON)
}

// landOmLessonsReportFrom is the command body over explicitly named inputs, so
// a test supplies the town, the clock and the records without swapping the
// shared flag variables.
func landOmLessonsReportFrom(findTown func() (string, error), out io.Writer, now time.Time, sinceValue string, days int, recordsFlag string, asJSON bool) error {
	report, err := landOmLessonsCluster(findTown, now, sinceValue, days, recordsFlag)
	if err != nil {
		return err
	}
	if asJSON {
		return writeLandOmLessonsJSON(out, report)
	}
	return writeLandOmLessonsTable(out, report)
}

// landOmLessonsCluster reads the review log and clusters its verdicts over the
// two windows the flags name. The report and the proposals both come from it.
func landOmLessonsCluster(findTown func() (string, error), now time.Time, sinceValue string, days int, recordsFlag string) (omlessons.Report, error) {
	since, err := parseLandOmStatsSince(sinceValue)
	if err != nil {
		return omlessons.Report{}, err
	}
	span, err := omlessons.Windows(now, since, days)
	if err != nil {
		return omlessons.Report{}, err
	}
	path, named, err := landOmLessonsRecordsPath(recordsFlag, findTown)
	if err != nil {
		return omlessons.Report{}, err
	}
	records, err := readLandOmLessonsRecords(path, named, span.Prior.Start)
	if err != nil {
		return omlessons.Report{}, err
	}
	return omlessons.Cluster(records, span), nil
}

// landOmLessonsProposeFrom prints the directive-edit drafts for the classes
// with enough findings in the window. The coverage of each class is read from
// the directive files in directivesDir; a directive that cannot be read is an
// error, so a missing file never reads as a list of gaps.
func landOmLessonsProposeFrom(findTown func() (string, error), out io.Writer, now time.Time, sinceValue string, days int, recordsFlag, directivesDir string) error {
	report, err := landOmLessonsCluster(findTown, now, sinceValue, days, recordsFlag)
	if err != nil {
		return err
	}
	rules, err := readLandOmLessonsRules(directivesDir)
	if err != nil {
		return err
	}
	return writeLandOmLessonsProposals(out, report, rules, directivesDir)
}

// landOmLessonsRuleHeading matches a numbered rule's heading in a directive,
// such as "## 1. Failure is not success (fail-open)". Rule numbers run 1 to 99.
var landOmLessonsRuleHeading = regexp.MustCompile(`^## ([1-9][0-9]?)\. `)

// landOmLessonsDirectiveFiles are the directive files whose numbered rules
// count as coverage: the polecat rules and the shared rules.
var landOmLessonsDirectiveFiles = []string{"polecat.md", "_common.md"}

// readLandOmLessonsRules returns the set of rule numbers the directive files in
// dir carry, as their decimal text. A file that cannot be read is an error; so
// is a directive set with no numbered rule at all, since that would read every
// class as a gap.
func readLandOmLessonsRules(dir string) (map[string]bool, error) {
	rules := map[string]bool{}
	for _, name := range landOmLessonsDirectiveFiles {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading the directive: %w", err)
		}
		for _, line := range strings.Split(string(data), "\n") {
			if m := landOmLessonsRuleHeading.FindStringSubmatch(line); m != nil {
				rules[m[1]] = true
			}
		}
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("%s holds no numbered rules: every class would read as a gap", dir)
	}
	return rules, nil
}

// landOmLessonsCoverage names a class's coverage. A class whose rule is in the
// directive is violated-anyway when it still appears in the window. A class
// with no rule, or whose rule number the directive does not carry, is a gap.
func landOmLessonsCoverage(c omlessons.ClassRow, rules map[string]bool) string {
	switch {
	case c.Rule == 0:
		return "gap: no numbered rule covers it"
	case !rules[strconv.Itoa(c.Rule)]:
		return fmt.Sprintf("gap: the table names rule #%d and the directive has no rule #%d", c.Rule, c.Rule)
	}
	return fmt.Sprintf("violated-anyway: rule #%d covers it and it still appears", c.Rule)
}

// writeLandOmLessonsProposals prints a draft for each class with enough
// findings, and a too-few line for each class without. The drafts are text for
// the overseer, who writes the edit itself.
func writeLandOmLessonsProposals(w io.Writer, report omlessons.Report, rules map[string]bool, directivesDir string) error {
	var b strings.Builder
	fmt.Fprintf(&b, "om lessons proposals: %s (%d days), against %s\n",
		landOmLessonsRange(report.Window), report.Days, landOmLessonsRange(report.Prior))
	fmt.Fprintf(&b, "rules read from %s; a class needs %d findings in the window to get a draft\n",
		directivesDir, landOmLessonsProposeMinFindings)
	for _, c := range report.Classes {
		if c.Current.Findings < landOmLessonsProposeMinFindings {
			fmt.Fprintf(&b, "\ntoo few to say: %s (%d finding(s) in the window)\n", c.Name, c.Current.Findings)
			continue
		}
		fmt.Fprintf(&b, "\n--- %s (%s)\n", c.Name, c.ID)
		fmt.Fprintf(&b, "Coverage: %s\n", landOmLessonsCoverage(c, rules))
		fmt.Fprintf(&b, "Counts: %d finding(s) on %d bead(s), %d round(s) burned; prior window %d finding(s) on %d bead(s), %d round(s)\n",
			c.Current.Findings, c.Current.Beads, c.Current.Rounds,
			c.Prior.Findings, c.Prior.Beads, c.Prior.Rounds)
		fmt.Fprintf(&b, "Examples: %s\n", strings.Join(c.ExampleBeads, ", "))
		fmt.Fprintln(&b, "\nGoal: a directive edit that stops om raising this class.")
		fmt.Fprintf(&b, "Constraints: the edit goes to the directive files in %s; the edit text is the overseer's to write. A rule that changes what polecats may do needs the operator's yes.\n", directivesDir)
		fmt.Fprintln(&b, "Out of scope: om's bar or threshold; turning anything on.")
		fmt.Fprintln(&b, "Gate: go test ./internal/cmd -run TestLandOmLessons -count=1")
		fmt.Fprintln(&b, "Size: one directive edit, one landing.")
		fmt.Fprintln(&b, "Acceptance:")
		fmt.Fprintln(&b, "- [ ] the rule for this class says what the case is and what to do instead")
		fmt.Fprintln(&b, "- [ ] the next weekly pass counts fewer findings of this class than this window")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// landOmLessonsRecordsPath fills an empty --records with the town's review
// log, and reports whether the operator named the path.
func landOmLessonsRecordsPath(recordsFlag string, findTown func() (string, error)) (path string, named bool, err error) {
	if path = strings.TrimSpace(recordsFlag); path != "" {
		return path, true, nil
	}
	townRoot, err := findTown()
	if err != nil {
		return "", false, fmt.Errorf("resolving the town: %w", err)
	}
	return land.TownReviewsFile(townRoot).Path, false, nil
}

// readLandOmLessonsRecords reads the verdicts at or after since through the
// review log's own reader. The reader reads an absent log as no records,
// because a town that has not landed yet has none; a path the operator named
// must exist, so a typo cannot read as that same empty report.
func readLandOmLessonsRecords(path string, named bool, since time.Time) ([]land.ReviewRecord, error) {
	if named {
		if _, err := os.Stat(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, fmt.Errorf("%s does not exist: %w", path, err)
			}
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}
	records, err := (&land.ReviewsFile{Path: path}).Since(since)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return records, nil
}

// writeLandOmLessonsJSON prints the report as the struct the table renders, so
// a reader can take every figure without scraping the columns.
func writeLandOmLessonsJSON(w io.Writer, report omlessons.Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("writing the report: %w", err)
	}
	return nil
}

// writeLandOmLessonsTable prints a row per class with the current window first
// and the prior one beside it, then the titles no class matched. The table is
// built in a strings.Builder, which cannot fail, and written to w once, so a
// refused write to w is the run's error.
func writeLandOmLessonsTable(w io.Writer, report omlessons.Report) error {
	var b strings.Builder
	fmt.Fprintf(&b, "om lessons: %s (%d days), against %s\n",
		landOmLessonsRange(report.Window), report.Days, landOmLessonsRange(report.Prior))
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "Class\tRule\tFindings\tBeads\tRounds\tPrior findings\tPrior beads\tPrior rounds")
	for _, c := range report.Classes {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%d\t%d\t%d\n", c.Name, landOmLessonsRule(c.Rule),
			c.Current.Findings, c.Current.Beads, c.Current.Rounds,
			c.Prior.Findings, c.Prior.Beads, c.Prior.Rounds)
	}
	_ = tw.Flush()

	if len(report.Unclassified) > 0 {
		fmt.Fprintln(&b, "\nUnclassified")
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "Findings\tPrior\tTitle")
		for _, u := range report.Unclassified {
			title := "(no title)"
			if u.Title != "" {
				title = terminalSafe(u.Title)
			}
			fmt.Fprintf(tw, "%d\t%d\t%s\n", u.Current, u.Prior, title)
		}
		_ = tw.Flush()
	}

	fmt.Fprintf(&b, "\n%d verdict(s) in the window, %d in the prior; %d and %d carry no finding text\n",
		report.Window.Verdicts, report.Prior.Verdicts, report.Window.NoFindingText, report.Prior.NoFindingText)
	_, err := io.WriteString(w, b.String())
	return err
}

func landOmLessonsRange(window omlessons.Window) string {
	return window.First + " to " + window.Last
}

func landOmLessonsRule(rule int) string {
	if rule == 0 {
		return "-"
	}
	return fmt.Sprintf("#%d", rule)
}
