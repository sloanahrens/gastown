package omlessons

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// day is a local instant on 2026-10-D.
func day(d, hour int) time.Time {
	return time.Date(2026, 10, d, hour, 0, 0, 0, time.Local)
}

// lessonFinding is one finding per class, written as om wrote it when the
// class last fired. The list is spelled out here rather than read from the
// table, so a class dropped from the table leaves its finding unclassified
// and fails the test below.
var lessonFinding = map[string]land.ReviewFindingText{
	"fail-open":            {Title: "unreadable main status suppresses escalation (fail-open gate)"},
	"silent-drop":          {Title: "the parser silently drops an unreadable line"},
	"missing-refusal-test": {Title: "the refusal branch has no test"},
	"no-red-when-removed":  {Title: "a test that passes with the logging call deleted"},
	"wall-clock-wait":      {Title: "the test sleeps waiting for the daemon"},
	"non-hermetic-test":    {Title: "the test reads the host's $SHELL", Claim: "not hermetic"},
	"doc-contradicts-code": {Title: "the package doc contradicts the code"},
	"comment-claim":        {Title: "the comment carries a line number"},
	"scope-widened":        {Title: "the change touches files outside the scope"},
	"secret-in-output":     {Title: "the error echoes captured command output"},
	"script-holds-logic":   {Title: "new parsing logic added to a shell script"},
	"unverified-claim":     {Title: "the verification claim has no shown output"},
}

// pinnedKeywords spells out each row's keywords here rather than read them from
// the table, so a keyword removed from the table is missing from this list and
// fails TestPinnedKeywordsStayInTheirRow.
var pinnedKeywords = map[string]struct{ Title, Claim []string }{
	"fail-open": {
		Title: []string{"fail-open", "fail open", "reads as clean", "as clean", "reads as success", "as success", "as approval", "does not apply", "treated as absent", "as if it succeeded", "suppresses escalation"},
		Claim: []string{"fail-open", "fail open", "reads the error", "reads a failed", "treats the error", "cannot tell a failed", "suppresses the escalation"},
	},
	"silent-drop": {
		Title: []string{"silently", "swallows", "discards", "drops the input", "ignores the error", "no error is returned"},
		Claim: []string{"silently", "swallowed", "discarded", "no error is returned", "drops the"},
	},
	"missing-refusal-test": {
		Title: []string{"refusal branch", "failing branch", "no assertion", "lacks a test", "lacks tests", "untested", "error path has no"},
		Claim: []string{"no test drives", "only the success path", "is not tested", "no assertion", "the refusal branch"},
	},
	"no-red-when-removed": {
		Title: []string{"passes with the", "passed with the", "passes without", "would still pass", "goes red", "fails without the", "test always passes"},
		Claim: []string{"would still pass", "still passes", "went red", "goes red", "fails when removed", "would pass with"},
	},
	"wall-clock-wait": {
		Title: []string{"sleep", "wall clock", "wall-clock", "polls until", "busy-wait", "a wait of"},
		Claim: []string{"sleep", "wall clock", "wall-clock", "the real clock", "waits for the clock", "real time"},
	},
	"non-hermetic-test": {
		Title: []string{"non-hermetic", "not hermetic", "hermetic", "host's", "hosts", "environment variable", "package-level", "global state", "live town", "tempdir"},
		Claim: []string{"hermetic", "os.getenv", "the host's", "package-level", "global state", "developer's environment"},
	},
	"doc-contradicts-code": {
		Title: []string{"contradicts", "stale doc", "documentation says", "the doc says"},
		Claim: []string{"contradicts", "documentation says", "the doc says", "no longer true"},
	},
	"comment-claim": {
		Title: []string{"comment", "line number", "misleading", "misstates"},
		Claim: []string{"comment", "the code does not", "misstates", "line number"},
	},
	"scope-widened": {
		Title: []string{"outside the scope", "out of scope", "scope", "unrelated", "unexplained"},
		Claim: []string{"unrelated", "outside the stated scope", "widens the", "not part of the bead"},
	},
	"secret-in-output": {
		Title: []string{"secret", "password", "credential", "token", "captured output", "echoes"},
		Claim: []string{"secret", "password", "credential", "token", "captured"},
	},
	"script-holds-logic": {
		Title: []string{"shell script", "script", ".sh", "python"},
		Claim: []string{"script", "shell"},
	},
	"unverified-claim": {
		Title: []string{"no shown output", "without output", "no output", "unfalsifiable", "verification claim", "says it passed"},
		Claim: []string{"no output", "not shown", "without showing", "did not show", "unfalsifiable"},
	},
}

// TestClassTableClaimsItsFindings pins the table from the outside: every
// finding above joins its own class, every class has one, and a finding
// carrying nothing the table knows is unclassified.
func TestClassTableClaimsItsFindings(t *testing.T) {
	t.Parallel()
	for id, f := range lessonFinding {
		if got := classify(f); got != id {
			t.Errorf("finding %q classified %q, want %q", f.Title, got, id)
		}
	}
	inTable := map[string]bool{}
	for _, c := range classes {
		inTable[c.ID] = true
		if _, ok := lessonFinding[c.ID]; !ok {
			t.Errorf("class %q has no finding here: add one so the class is pinned", c.ID)
		}
	}
	for id := range lessonFinding {
		if !inTable[id] {
			t.Errorf("class %q is missing from the table", id)
		}
	}
	if got := classify(land.ReviewFindingText{Title: "a finding from a class not yet seeded", Claim: "nothing the table knows"}); got != Unclassified {
		t.Errorf("a finding matching no row classified %q, want %q", got, Unclassified)
	}
}

// TestEveryKeywordClassifiesToItsRow: a finding carrying any keyword of a row
// joins that row, so a keyword moved to the wrong row fails here.
func TestEveryKeywordClassifiesToItsRow(t *testing.T) {
	t.Parallel()
	for _, c := range classes {
		for _, keyword := range c.Title {
			if got := classify(land.ReviewFindingText{Title: "om wrote: " + keyword}); got != c.ID {
				t.Errorf("title %q classified %q, want %q", keyword, got, c.ID)
			}
		}
		for _, keyword := range c.Claim {
			if got := classify(land.ReviewFindingText{Claim: "om wrote: " + keyword}); got != c.ID {
				t.Errorf("claim %q classified %q, want %q", keyword, got, c.ID)
			}
		}
	}
}

// TestPinnedKeywordsStayInTheirRow: each keyword pinned above is still in its
// row and still joins that row, so a keyword removed from the table fails here
// instead of quietly shrinking the check above.
func TestPinnedKeywordsStayInTheirRow(t *testing.T) {
	t.Parallel()
	for id, want := range pinnedKeywords {
		var row *Class
		for i := range classes {
			if classes[i].ID == id {
				row = &classes[i]
			}
		}
		if row == nil {
			t.Errorf("class %q is pinned but missing from the table", id)
			continue
		}
		for _, keyword := range want.Title {
			if !slices.Contains(row.Title, keyword) {
				t.Errorf("class %q: title keyword %q is no longer in its row", id, keyword)
			}
			if got := classify(land.ReviewFindingText{Title: "om wrote: " + keyword}); got != id {
				t.Errorf("title %q classified %q, want %q", keyword, got, id)
			}
		}
		for _, keyword := range want.Claim {
			if !slices.Contains(row.Claim, keyword) {
				t.Errorf("class %q: claim keyword %q is no longer in its row", id, keyword)
			}
			if got := classify(land.ReviewFindingText{Claim: "om wrote: " + keyword}); got != id {
				t.Errorf("claim %q classified %q, want %q", keyword, got, id)
			}
		}
	}
}

// TestClassTableIsWellFormed keeps the table usable as the table: an id is
// what a reader matches a count to, and a keyword is a lowercase substring.
func TestClassTableIsWellFormed(t *testing.T) {
	t.Parallel()
	ids := map[string]bool{}
	for _, c := range classes {
		if c.ID == "" || c.Name == "" {
			t.Errorf("row %+v: want an id and a name", c)
		}
		if ids[c.ID] {
			t.Errorf("class %q appears twice", c.ID)
		}
		ids[c.ID] = true
		if c.Rule < 0 || c.Rule > 12 {
			t.Errorf("class %q carries rule %d, outside the directive's 12", c.ID, c.Rule)
		}
		if len(c.Title) == 0 || len(c.Claim) == 0 {
			t.Errorf("class %q: want keywords for the title and for the claim both", c.ID)
		}
		for _, keyword := range append(append([]string{}, c.Title...), c.Claim...) {
			if keyword == "" {
				t.Errorf("class %q: an empty keyword matches every finding", c.ID)
			}
			if keyword != strings.ToLower(keyword) {
				t.Errorf("class %q: keyword %q is not lowercase, so it never matches", c.ID, keyword)
			}
		}
	}
}

// TestWindowsEndTodayWithoutSince: a run with no --since counts the days
// ending today, and the window before it.
func TestWindowsEndTodayWithoutSince(t *testing.T) {
	t.Parallel()
	sp, err := Windows(day(30, 9), time.Time{}, 7)
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.Current.Start; !got.Equal(day(24, 0)) {
		t.Errorf("current start = %v, want the 24th", got)
	}
	if got := sp.Current.End; !got.Equal(day(31, 0)) {
		t.Errorf("current end = %v, want the 31st", got)
	}
	if got := sp.Prior.Start; !got.Equal(day(17, 0)) {
		t.Errorf("prior start = %v, want the 17th", got)
	}
	if got := sp.Prior.End; !got.Equal(day(24, 0)) {
		t.Errorf("prior end = %v, want the 24th", got)
	}
	if sp.Days != 7 {
		t.Errorf("days = %d, want 7", sp.Days)
	}
}

// TestWindowsStartOnSince: --since pins the first day, and the window runs its
// length from there, whether or not that day has passed.
func TestWindowsStartOnSince(t *testing.T) {
	t.Parallel()
	sp, err := Windows(day(30, 9), day(20, 15), 3)
	if err != nil {
		t.Fatal(err)
	}
	if got := sp.Current.Start; !got.Equal(day(20, 0)) {
		t.Errorf("current start = %v, want the 20th", got)
	}
	if got := sp.Current.End; !got.Equal(day(23, 0)) {
		t.Errorf("current end = %v, want the 23rd", got)
	}
	if got := sp.Prior.Start; !got.Equal(day(17, 0)) {
		t.Errorf("prior start = %v, want the 17th", got)
	}
	if got := sp.Prior.End; !got.Equal(day(20, 0)) {
		t.Errorf("prior end = %v, want the 20th", got)
	}
}

// TestWindowsRejectsAnEmptyWindow: a window of no days would count nothing,
// so a length below one is an error rather than an empty report.
func TestWindowsRejectsAnEmptyWindow(t *testing.T) {
	t.Parallel()
	for _, days := range []int{0, -1} {
		if _, err := Windows(day(30, 9), time.Time{}, days); err == nil {
			t.Errorf("days %d: no error", days)
		}
	}
}

// lessonFixture is the records every clustering case below reads.
func lessonFixture() ([]land.ReviewRecord, Span) {
	sp, err := Windows(day(30, 12), time.Time{}, 7)
	if err != nil {
		panic(err)
	}
	recs := []land.ReviewRecord{
		// One bead through three rounds: a refusal test, then a test that
		// passes without what it covers, then a minor comment finding.
		{BeadID: "gt-a", Attempt: 1, Verdict: land.VerdictRequestChanges, At: day(25, 10),
			FindingText: []land.ReviewFindingText{{Severity: "major", Title: "the refusal branch has no test"}}},
		{BeadID: "gt-a", Attempt: 2, Verdict: land.VerdictRequestChanges, At: day(26, 10),
			FindingText: []land.ReviewFindingText{{Severity: "major", Title: "a test that passes with the logging call deleted"}}},
		{BeadID: "gt-a", Attempt: 3, Verdict: land.VerdictApprove, At: day(27, 10),
			FindingText: []land.ReviewFindingText{{Severity: "minor", Title: "the comment carries a stale line number"}}},
		// A blocking finding on a bead whose first round is in neither window.
		{BeadID: "gt-f", Attempt: 2, Verdict: land.VerdictRequestChanges, At: day(28, 10),
			FindingText: []land.ReviewFindingText{{Severity: "critical", Title: "the gate reads a missing file as clean"}}},
		// Two findings of a class the table does not know.
		{BeadID: "gt-g", Attempt: 1, Verdict: land.VerdictRequestChanges, At: day(29, 10),
			FindingText: []land.ReviewFindingText{
				{Severity: "minor", Title: "a finding from a class not yet seeded"},
				{Severity: "minor", Title: "a finding from a class not yet seeded"},
			}},
		// The window before: a silent drop, a minor sleeping test, and a
		// record written before the log kept finding text.
		{BeadID: "gt-b", Attempt: 1, Verdict: land.VerdictRequestChanges, At: day(18, 10),
			FindingText: []land.ReviewFindingText{{Severity: "major", Title: "the helper silently swallows the read error"}}},
		{BeadID: "gt-d", Attempt: 1, Verdict: land.VerdictRequestChanges, At: day(20, 10),
			FindingText: []land.ReviewFindingText{{Severity: "minor", Title: "the test sleeps waiting for the daemon"}}},
		{BeadID: "gt-c", Attempt: 1, Verdict: land.VerdictApprove, At: day(19, 10)},
		{BeadID: "gt-h", Attempt: 1, Verdict: land.VerdictRequestChanges, At: day(21, 10),
			FindingText: []land.ReviewFindingText{{Severity: "minor", Title: "a finding from a class not yet seeded"}}},
		// Before both windows: counted nowhere.
		{BeadID: "gt-e", Attempt: 1, Verdict: land.VerdictRequestChanges, At: day(10, 10),
			FindingText: []land.ReviewFindingText{{Severity: "major", Title: "the refusal branch has no test"}}},
	}
	return recs, sp
}

// TestClusterCountsEachClassInBothWindows: findings, the beads they were on
// and the rounds burned, per class, for the window and the one before it.
func TestClusterCountsEachClassInBothWindows(t *testing.T) {
	t.Parallel()
	recs, sp := lessonFixture()
	rep := Cluster(recs, sp)
	want := map[string][2]ClassCounts{
		// gt-a's class blocked at attempts 1 and 2, and the bead reached
		// attempt 3: two rounds burned.
		"missing-refusal-test": {{Findings: 1, Beads: 1, Rounds: 2}, {}},
		"no-red-when-removed":  {{Findings: 1, Beads: 1, Rounds: 2}, {}},
		// A minor finding burns no round.
		"comment-claim": {{Findings: 1, Beads: 1}, {}},
		// gt-f's first round is older than both windows; its second is a
		// blocking finding here, so the round after the first is counted.
		"fail-open":       {{Findings: 1, Beads: 1, Rounds: 1}, {}},
		"silent-drop":     {{}, {Findings: 1, Beads: 1}},
		"wall-clock-wait": {{}, {Findings: 1, Beads: 1}},
	}
	if len(rep.Classes) != len(classes) {
		t.Fatalf("report has %d classes, want the table's %d", len(rep.Classes), len(classes))
	}
	for _, c := range rep.Classes {
		expected, ok := want[c.ID]
		if !ok {
			expected = [2]ClassCounts{}
		}
		if c.Current != expected[0] {
			t.Errorf("%s current = %+v, want %+v", c.ID, c.Current, expected[0])
		}
		if c.Prior != expected[1] {
			t.Errorf("%s prior = %+v, want %+v", c.ID, c.Prior, expected[1])
		}
	}
}

// TestClusterNamesTheWindowsAndTheirVerdicts: each window says its days, how
// many verdicts it holds, and how many of them carried no finding text.
func TestClusterNamesTheWindowsAndTheirVerdicts(t *testing.T) {
	t.Parallel()
	recs, sp := lessonFixture()
	rep := Cluster(recs, sp)
	if rep.Days != 7 {
		t.Errorf("days = %d, want 7", rep.Days)
	}
	wantCurrent := Window{First: "2026-10-24", Last: "2026-10-30", Verdicts: 5}
	if rep.Window != wantCurrent {
		t.Errorf("window = %+v, want %+v", rep.Window, wantCurrent)
	}
	// The prior window's gt-c record was written before the log kept finding
	// text: it counts as a verdict, and as no finding.
	wantPrior := Window{First: "2026-10-17", Last: "2026-10-23", Verdicts: 4, NoFindingText: 1}
	if rep.Prior != wantPrior {
		t.Errorf("prior = %+v, want %+v", rep.Prior, wantPrior)
	}
}

// TestClusterListsUnclassifiedTitles: a finding no class matched is listed by
// title, with its count in each window, most frequent first.
func TestClusterListsUnclassifiedTitles(t *testing.T) {
	t.Parallel()
	recs, sp := lessonFixture()
	rep := Cluster(recs, sp)
	want := []UnclassifiedTitle{{Title: "a finding from a class not yet seeded", Current: 2, Prior: 1}}
	if len(rep.Unclassified) != len(want) {
		t.Fatalf("unclassified = %+v, want %+v", rep.Unclassified, want)
	}
	if rep.Unclassified[0] != want[0] {
		t.Errorf("unclassified = %+v, want %+v", rep.Unclassified, want)
	}
}

// TestClusterOrdersUnclassifiedTitlesByFrequency: the title om writes most is
// the one the reader sees first, and a tie falls to the title's text.
func TestClusterOrdersUnclassifiedTitlesByFrequency(t *testing.T) {
	t.Parallel()
	sp, err := Windows(day(30, 12), time.Time{}, 7)
	if err != nil {
		t.Fatal(err)
	}
	recs := []land.ReviewRecord{
		{BeadID: "gt-a", Attempt: 1, At: day(25, 10), FindingText: []land.ReviewFindingText{
			{Title: "zebra, twice"}, {Title: "zebra, twice"}, {Title: "alpha"},
		}},
		{BeadID: "gt-b", Attempt: 1, At: day(26, 10), FindingText: []land.ReviewFindingText{
			{Title: "middle"}, {Title: "middle"},
		}},
	}
	rep := Cluster(recs, sp)
	want := []UnclassifiedTitle{
		{Title: "middle", Current: 2},
		{Title: "zebra, twice", Current: 2},
		{Title: "alpha", Current: 1},
	}
	if len(rep.Unclassified) != len(want) {
		t.Fatalf("unclassified = %+v, want %+v", rep.Unclassified, want)
	}
	for i := range want {
		if rep.Unclassified[i] != want[i] {
			t.Errorf("unclassified[%d] = %+v, want %+v", i, rep.Unclassified[i], want[i])
		}
	}
}

// TestClusterWindowsAreHalfOpen: a record at a window's first instant belongs
// to it, and one at the instant the next window starts does not.
func TestClusterWindowsAreHalfOpen(t *testing.T) {
	t.Parallel()
	sp, err := Windows(day(30, 12), time.Time{}, 7)
	if err != nil {
		t.Fatal(err)
	}
	recs := []land.ReviewRecord{
		{BeadID: "gt-first", Attempt: 1, At: sp.Current.Start},
		{BeadID: "gt-after", Attempt: 1, At: sp.Current.End},
		{BeadID: "gt-before", Attempt: 1, At: sp.Prior.Start.Add(-time.Second)},
	}
	rep := Cluster(recs, sp)
	if rep.Window.Verdicts != 1 || rep.Prior.Verdicts != 0 {
		t.Errorf("verdicts = %d current, %d prior; want the first instant in the window and nothing else",
			rep.Window.Verdicts, rep.Prior.Verdicts)
	}
}
