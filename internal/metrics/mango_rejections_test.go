package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/land"
)

// rejectionAt is a fixed Wednesday, so the tables' week is rejectionWeekOf.
var rejectionAt = time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)

// rejectionWeekOf is rejectionAt's Monday, the week the section prints.
const rejectionWeekOf = "2026-10-19"

// rejectionNote is one rejection block as the landing worker writes it, with
// an om finding's wording in the finding title, which is what the pattern
// table reads.
func rejectionNote(attempt int, finding string) string {
	return fmt.Sprintf("MERGE REJECTION (attempt %d): review - om requested changes\n"+
		"Branch: polecat/x/gt-a\nTarget: main\nMR: gt-a\n"+
		"- id:abc sev:major internal/x/y.go:4 — %s", attempt, finding)
}

// rejectionNotes is notes keyed the way the join reads them: the store the
// record names ("" is the town's own) and the bead.
func rejectionNotes(rig string, notes map[string]string) map[RejectionNoteKey]string {
	out := make(map[RejectionNoteKey]string, len(notes))
	for bead, body := range notes {
		out[RejectionNoteKey{Rig: rig, Bead: bead}] = body
	}
	return out
}

// rejectionFixtures is one or more om findings per pattern, worded as om words
// them. The cases go through RejectionRates, so a rule the section stops
// applying fails here rather than only in the classification helper. The
// findings demonstrate the table's behaviour; its phrases are held to om's own
// wording by TestEveryRejectionPhraseIsInTheDirective.
var rejectionFixtures = []struct {
	finding string
	want    string
}{
	{"unreadable main status suppresses escalation (fail-open gate)", "fail_open"},
	{"criterion that the acked item stays counted has no test", "criterion_untested"},
	{"failing branch has no assertion", "refusal_untested"},
	{"decision authority is granted by prefix alone, so any commenter can forge a decision", "forged_authority"},
	{"author under review can self-rebut its own findings", "self_review"},
	{"verification claim has no shown output", "unshown_verification"},
	{"test depends on the host's $SHELL, so it is not hermetic", "hermeticity"},
	{"a test reads the live town tree instead of a temp dir", "hermeticity"},
	{"new parsing and state logic added to a shell script", "script"},
	{"a new python script parses the config", "script"},
	{"change touches files outside the stated scope", "out_of_scope"},
	{"a go.mod bump with an unexplained transitive dependency", "out_of_scope"},
	{"a test that calls the helper directly", "call_site_untested"},
	{"the test calls the helper, so deleting the call site leaves it green", "call_site_untested"},
	{"an error branch treated as a file that does not apply", "helper_fail_open"},
	{"a matcher error read as excluded, so the check does not fail closed", "helper_fail_open"},
	{"an error that echoes captured command output", "secrets_in_errors"},
	{"the error puts the captured output in its message", "secrets_in_errors"},
}

// rejectionRecord is one verdict on gt-a at rejectionAt.
func rejectionRecord(bead string, attempt int, agent string) land.ReviewRecord {
	return land.ReviewRecord{
		BeadID:  bead,
		Rig:     "gastown",
		Attempt: attempt,
		Score:   0.5,
		Verdict: land.VerdictRequestChanges,
		Agent:   agent,
		At:      rejectionAt,
	}
}

func TestEveryRejectionPatternHasAFixture(t *testing.T) {
	t.Parallel()
	covered := map[string]bool{}
	for _, c := range rejectionFixtures {
		covered[c.want] = true
	}
	for _, pattern := range rejectionPatterns {
		if !covered[pattern.Name] {
			t.Errorf("pattern %q has no fixture in rejectionFixtures", pattern.Name)
		}
	}
}

// omRulesFile is the directive carrying om's own finding wording, relative to
// the package directory.
const omRulesFile = "../../directives/polecat.md"

// omRulesHeading opens the directive's list of the twelve rules, each naming
// the finding om files for it.
const omRulesHeading = "# What om rejects"

// omRuleBlocks is the directive's twelve rules' text by rule number: the
// section's "## <n>. …" heading and the lines under it, to the next heading.
func omRuleBlocks(t *testing.T, directive string) map[int]string {
	t.Helper()
	start := strings.Index(directive, omRulesHeading)
	if start < 0 {
		t.Fatalf("%s holds no %q section", omRulesFile, omRulesHeading)
	}
	blocks := map[int]string{}
	number := 0
	for _, line := range strings.Split(directive[start:], "\n") {
		if rest, ok := strings.CutPrefix(line, "## "); ok {
			number = 0
			if n, _, found := strings.Cut(rest, "."); found {
				if parsed, err := strconv.Atoi(n); err == nil {
					number = parsed
				}
			}
		}
		if number != 0 {
			blocks[number] += line + "\n"
		}
	}
	return blocks
}

// TestEveryRejectionPhraseIsInTheDirective holds each pattern to its own rule's
// wording: a phrase that rule does not carry is a phrase copied from a
// checklist bullet or invented here, which would classify the directive's
// prose rather than a refusal. The wording is read from the repo's own copy of
// om's rules, not from a fixture beside the table, so the check cannot confirm
// phrasing the table made up.
func TestEveryRejectionPhraseIsInTheDirective(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(omRulesFile)
	if err != nil {
		t.Fatalf("reading om's rules: %v", err)
	}
	rules := omRuleBlocks(t, string(data))
	for _, pattern := range rejectionPatterns {
		rule, ok := rules[pattern.Rule]
		if !ok {
			t.Errorf("pattern %q names rule %d, which %s does not carry", pattern.Name, pattern.Rule, omRulesFile)
			continue
		}
		rule = strings.ToLower(rule)
		for _, phrase := range pattern.Phrases {
			if phrase != strings.ToLower(phrase) {
				t.Errorf("pattern %q phrase %q is not lowercase, so it can never match", pattern.Name, phrase)
			}
			if !strings.Contains(rule, phrase) {
				t.Errorf("pattern %q phrase %q is not in rule %d's wording: quote the finding om files, not the checklist",
					pattern.Name, phrase, pattern.Rule)
			}
		}
	}
}

// sameRejectionGroup reports whether got is want, comparing the rate within a
// tolerance: a rate is a computed fraction, and only two equal counts give an
// exactly equal float.
func sameRejectionGroup(got, want RejectionWeekGroup) bool {
	return got.Week == want.Week && got.Name == want.Name &&
		got.Verdicts == want.Verdicts && got.Rejections == want.Rejections &&
		math.Abs(got.Rate-want.Rate) < 1e-9
}

func TestRejectionFixtureClassifiesThroughTheRates(t *testing.T) {
	t.Parallel()
	for _, c := range rejectionFixtures {
		c := c
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			notes := rejectionNotes("gastown", map[string]string{"gt-a": rejectionNote(1, c.finding)})
			byPattern, _, err := RejectionRates([]land.ReviewRecord{rejectionRecord("gt-a", 1, "opus")}, notes)
			if err != nil {
				t.Fatalf("RejectionRates: %v", err)
			}
			want := RejectionWeekGroup{Week: rejectionWeekOf, Name: c.want, Verdicts: 1, Rejections: 1, Rate: 1}
			if len(byPattern) != 1 || !sameRejectionGroup(byPattern[0], want) {
				t.Fatalf("byPattern = %+v, want [%+v]", byPattern, want)
			}
		})
	}
}

func TestRejectionTakesTheFirstMatchingPattern(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, finding, want string
	}{
		{"two rules, the checklist's earlier one wins", "an error that echoes captured command output from a fail-open gate", "fail_open"},
		{"case does not matter", "the check is FAIL-OPEN on a read error", "fail_open"},
		{"no rule matches", "the change reads fine, only a style note", RejectionPatternUnclassified},
		{"an empty note", "", RejectionPatternUnclassified},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			notes := rejectionNotes("gastown", map[string]string{"gt-a": rejectionNote(1, c.finding)})
			byPattern, _, err := RejectionRates([]land.ReviewRecord{rejectionRecord("gt-a", 1, "opus")}, notes)
			if err != nil {
				t.Fatalf("RejectionRates: %v", err)
			}
			if len(byPattern) != 1 || byPattern[0].Name != c.want {
				t.Fatalf("byPattern = %+v, want one %s row", byPattern, c.want)
			}
		})
	}
}

func TestRejectionJoinReadsTheBlockForItsAttempt(t *testing.T) {
	t.Parallel()
	notes := rejectionNotes("gastown", map[string]string{"gt-a": rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)") +
		"\n" + rejectionNote(2, "an error that echoes captured command output")})
	records := []land.ReviewRecord{rejectionRecord("gt-a", 1, "opus"), rejectionRecord("gt-a", 2, "opus")}
	byPattern, _, err := RejectionRates(records, notes)
	if err != nil {
		t.Fatalf("RejectionRates: %v", err)
	}
	want := []RejectionWeekGroup{
		{Week: rejectionWeekOf, Name: "fail_open", Verdicts: 2, Rejections: 1, Rate: 0.5},
		{Week: rejectionWeekOf, Name: "secrets_in_errors", Verdicts: 2, Rejections: 1, Rate: 0.5},
	}
	if len(byPattern) != len(want) {
		t.Fatalf("byPattern = %+v, want %+v", byPattern, want)
	}
	for i := range want {
		if !sameRejectionGroup(byPattern[i], want[i]) {
			t.Errorf("byPattern[%d] = %+v, want %+v", i, byPattern[i], want[i])
		}
	}
}

func TestRejectionBlocksSkipAnAttemptThatOverflows(t *testing.T) {
	t.Parallel()
	notes := "MERGE REJECTION (attempt " + strings.Repeat("9", 40) + "): review - x\n" +
		"- id:abc sev:major internal/x/y.go:4 — a style note\n\n" +
		rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)")
	if blocks := rejectionBlocks(notes); len(blocks) != 1 || blocks[0].Attempt != 1 {
		t.Fatalf("blocks = %+v, want the one attempt a review can name", blocks)
	}
}

// TestRejectionPatternReadsOnlyItsBlocksFindings drives the lines around a
// block's findings: the block's pattern comes from the findings it filed, so
// neither its own gate tail nor a later note on the bead — even one quoting a
// finding-shaped line — names the refusal's pattern (gt-b9j3o.10).
func TestRejectionPatternReadsOnlyItsBlocksFindings(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, later string
	}{
		{"later prose names a pattern", "\n\nNext session: this looked like a fail-open gate to the reviewer."},
		{"a later note quotes a finding line", "\n\nNext session's notes:\n" +
			"- id:def sev:major internal/a/b.go:2 — unreadable status (fail-open gate)"},
		{"its own gate tail quotes the wording", "\nGate tail:\n  | rule 1: unreadable status suppresses escalation (fail-open gate)"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			notes := rejectionNotes("gastown", map[string]string{"gt-a": rejectionNote(1, "the change reads fine, only a style note") + c.later})
			byPattern, _, err := RejectionRates([]land.ReviewRecord{rejectionRecord("gt-a", 1, "opus")}, notes)
			if err != nil {
				t.Fatalf("RejectionRates: %v", err)
			}
			if len(byPattern) != 1 || byPattern[0].Name != RejectionPatternUnclassified {
				t.Fatalf("byPattern = %+v, want one %s row: only the block's own findings carry its pattern",
					byPattern, RejectionPatternUnclassified)
			}
		})
	}
}

// TestRejectionPatternReadsEachStoresOwnNotes drives one bead id in two stores:
// the join keys notes by the store the review log names, so each record reads
// its own store's notes rather than whichever store answered last.
func TestRejectionPatternReadsEachStoresOwnNotes(t *testing.T) {
	t.Parallel()
	notes := map[RejectionNoteKey]string{
		{Rig: "gastown", Bead: "gt-a"}: rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)"),
		{Rig: "beads", Bead: "gt-a"}:   rejectionNote(1, "the change reads fine, only a style note"),
	}
	records := []land.ReviewRecord{
		rejectionRecord("gt-a", 1, "opus"),
		{BeadID: "gt-a", Rig: "beads", Attempt: 1, Verdict: land.VerdictRequestChanges, Agent: "opus", At: rejectionAt},
	}
	byPattern, _, err := RejectionRates(records, notes)
	if err != nil {
		t.Fatalf("RejectionRates: %v", err)
	}
	want := []RejectionWeekGroup{
		{Week: rejectionWeekOf, Name: "fail_open", Verdicts: 2, Rejections: 1, Rate: 0.5},
		{Week: rejectionWeekOf, Name: RejectionPatternUnclassified, Verdicts: 2, Rejections: 1, Rate: 0.5},
	}
	if len(byPattern) != len(want) {
		t.Fatalf("byPattern = %+v, want %+v", byPattern, want)
	}
	for i := range want {
		if !sameRejectionGroup(byPattern[i], want[i]) {
			t.Errorf("byPattern[%d] = %+v, want %+v", i, byPattern[i], want[i])
		}
	}
}

func TestRejectionWithoutItsNoteIsNoNote(t *testing.T) {
	t.Parallel()
	records := []land.ReviewRecord{rejectionRecord("gt-a", 1, "opus")}
	byPattern, _, err := RejectionRates(records, rejectionNotes("gastown", map[string]string{"gt-a": rejectionNote(2, "fail-open")}))
	if err != nil {
		t.Fatalf("RejectionRates: %v", err)
	}
	if len(byPattern) != 1 || byPattern[0].Name != RejectionPatternNoNote {
		t.Fatalf("byPattern = %+v, want one %s row", byPattern, RejectionPatternNoNote)
	}
}

func TestRejectionRatesGroupByWeekAndSeat(t *testing.T) {
	t.Parallel()
	nextWeek := rejectionAt.AddDate(0, 0, 7)
	notes := rejectionNotes("gastown", map[string]string{"gt-a": rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)")})
	records := []land.ReviewRecord{
		rejectionRecord("gt-a", 1, "opus"),
		{BeadID: "gt-b", Verdict: land.VerdictApprove, Agent: "opus", At: rejectionAt},
		{BeadID: "gt-c", Verdict: land.VerdictApprove, At: rejectionAt},
		{BeadID: "gt-d", Verdict: land.VerdictApprove, Agent: "sonnet", At: nextWeek},
	}
	byPattern, bySeat, err := RejectionRates(records, notes)
	if err != nil {
		t.Fatalf("RejectionRates: %v", err)
	}
	wantSeat := []RejectionWeekGroup{
		{Week: rejectionWeekOf, Name: "opus", Verdicts: 2, Rejections: 1, Rate: 0.5},
		{Week: rejectionWeekOf, Name: land.OmStatsUnnamed, Verdicts: 1, Rejections: 0, Rate: 0},
		{Week: "2026-10-26", Name: "sonnet", Verdicts: 1, Rejections: 0, Rate: 0},
	}
	if len(bySeat) != len(wantSeat) {
		t.Fatalf("bySeat = %+v, want %+v", bySeat, wantSeat)
	}
	for i := range wantSeat {
		if !sameRejectionGroup(bySeat[i], wantSeat[i]) {
			t.Errorf("bySeat[%d] = %+v, want %+v", i, bySeat[i], wantSeat[i])
		}
	}
	wantPattern := []RejectionWeekGroup{
		{Week: rejectionWeekOf, Name: "fail_open", Verdicts: 3, Rejections: 1, Rate: 1.0 / 3},
	}
	if len(byPattern) != 1 || !sameRejectionGroup(byPattern[0], wantPattern[0]) {
		t.Errorf("byPattern = %+v, want %+v", byPattern, wantPattern)
	}
}

func TestRejectionRatesRefuseARecordWithNoInstant(t *testing.T) {
	t.Parallel()
	_, _, err := RejectionRates([]land.ReviewRecord{{BeadID: "gt-a", Verdict: land.VerdictApprove}}, nil)
	if err == nil || !strings.Contains(err.Error(), "no instant") {
		t.Fatalf("err = %v, want a refusal naming the missing instant", err)
	}
}

// rejectionTown writes a review log under town and returns it.
func rejectionTown(t *testing.T, lines ...land.ReviewRecord) string {
	t.Helper()
	town := t.TempDir()
	var log strings.Builder
	for _, rec := range lines {
		data, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		log.Write(data)
		log.WriteString("\n")
	}
	writeTownFile(t, land.TownReviewsFile(town).Path, log.String())
	return town
}

// rejectionBd is a bd that answers a bd show with the notes of the beads it
// was asked for, recording the store directories it was asked in. A bead it
// has no note for is a fixture bug, so the case fails rather than answering a
// listing that would hide it.
func rejectionBd(t *testing.T, notes map[string]string, asked *[]string) Runner {
	t.Helper()
	return func(_ context.Context, argv []string) ([]byte, error) {
		if len(argv) < 4 || argv[0] != "bd" || argv[1] != "-C" || argv[3] != "show" {
			t.Fatalf("argv = %v, want a bd show in -C", argv)
		}
		*asked = append(*asked, argv[2])
		var rows []bdListBead
		for _, arg := range argv[4:] {
			id, ok := strings.CutPrefix(arg, "--id=")
			if !ok {
				continue
			}
			body, known := notes[id]
			if !known {
				t.Fatalf("bd show asked for %q, which the case has no note for", id)
			}
			rows = append(rows, bdListBead{ID: id, Notes: body})
		}
		return json.Marshal(rows)
	}
}

func TestMangoRejectionsSectionRendersBothTables(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"),
		land.ReviewRecord{BeadID: "gt-b", Rig: "gastown", Verdict: land.VerdictApprove, Agent: "sonnet", At: rejectionAt})
	var asked []string
	env := Env{
		Town:     town,
		ReadFile: os.ReadFile,
		Run:      rejectionBd(t, map[string]string{"gt-a": rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)")}, &asked),
	}
	text, err := MangoRejectionsSection(context.Background(), env)
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	for _, want := range []string{
		"### Rejections by pattern",
		"### Rejections by agent seat",
		"| week | pattern | rejections | verdicts | rate |",
		"| week | seat | rejections | verdicts | rate |",
		"| " + rejectionWeekOf + " | fail_open | 1 | 2 | 50.0% |",
		"| " + rejectionWeekOf + " | opus | 1 | 1 | 100.0% |",
		"| " + rejectionWeekOf + " | sonnet | 0 | 1 | 0.0% |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
	if len(asked) != 1 || asked[0] != filepath.Join(town, "gastown") {
		t.Errorf("bd run in %v, want the gastown store alone", asked)
	}
}

// TestMangoRejectionsSectionFailsOnAMissingLog holds a missing log to the same
// rule as an unreadable one: the section must not read a log it could not find
// as a town that refused nothing.
func TestMangoRejectionsSectionFailsOnAMissingLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Run: func(context.Context, []string) ([]byte, error) { return nil, nil },
	})
	if err == nil || !strings.Contains(err.Error(), "reviews.jsonl") {
		t.Fatalf("err = %v, want a refusal naming the missing review log", err)
	}
}

// TestMangoRejectionsSectionReadsEachStoresOwnNotes drives one bead id in two
// stores: each store is asked for the beads its own records name, and each
// record reads the notes the store it names returned.
func TestMangoRejectionsSectionReadsEachStoresOwnNotes(t *testing.T) {
	t.Parallel()
	other := rejectionRecord("gt-a", 1, "opus")
	other.Rig = "beads"
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"), other)
	var asked []string
	// The fixture answers by store, so the same id reaches two different notes.
	run := func(_ context.Context, argv []string) ([]byte, error) {
		if len(argv) < 4 || argv[0] != "bd" || argv[1] != "-C" || argv[3] != "show" {
			t.Fatalf("argv = %v, want a bd show in -C", argv)
		}
		asked = append(asked, argv[2])
		note := "the change reads fine, only a style note"
		if argv[2] == filepath.Join(town, "gastown") {
			note = "unreadable status suppresses escalation (fail-open gate)"
		}
		return json.Marshal([]bdListBead{{ID: "gt-a", Notes: rejectionNote(1, note)}})
	}
	text, err := MangoRejectionsSection(context.Background(), Env{Town: town, ReadFile: os.ReadFile, Run: run})
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	for _, want := range []string{
		"| " + rejectionWeekOf + " | fail_open | 1 | 2 | 50.0% |",
		"| " + rejectionWeekOf + " | " + RejectionPatternUnclassified + " | 1 | 2 | 50.0% |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
	wantAsked := []string{filepath.Join(town, "beads"), filepath.Join(town, "gastown")}
	if len(asked) != 2 || asked[0] != wantAsked[0] || asked[1] != wantAsked[1] {
		t.Errorf("bd run in %v, want %v", asked, wantAsked)
	}
}

// TestMangoRejectionsSectionFailsWhenBdOmitsABead is the join's refusal branch:
// a store that does not return a bead the log names is a broken read, not a
// refusal carrying no note.
func TestMangoRejectionsSectionFailsWhenBdOmitsABead(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile,
		Run: func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "gt-a") {
		t.Fatalf("err = %v, want a refusal naming the bead the store did not return", err)
	}
}

func TestMangoRejectionsSectionSkipsUnreadableLines(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	log := "not a record\n" + // not JSON
		`{"verdict":"approve","at":"2026-10-21T12:00:00Z"}` + "\n" + // no bead
		`{"bead":"gt-b","verdict":"approve","at":"2026-10-21T12:00:00Z"}` + "\n"
	writeTownFile(t, land.TownReviewsFile(town).Path, log)
	text, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Run: func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil },
	})
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	if !strings.Contains(text, "2 review-log line(s) were not readable") {
		t.Errorf("text = %q, want it to count the two unreadable lines", text)
	}
}

func TestMangoRejectionsSectionCountsUnreadableLinesWithoutRecords(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownFile(t, land.TownReviewsFile(town).Path, "not a record\n")
	text, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Run: func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil },
	})
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	for _, want := range []string{"No review records yet", "1 review-log line(s) were not readable"} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
}

func TestMangoRejectionsSectionRefusesARigThatEscapesTheTown(t *testing.T) {
	t.Parallel()
	rec := rejectionRecord("gt-a", 1, "opus")
	rec.Rig = "../elsewhere"
	town := rejectionTown(t, rec)
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile,
		Run: func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "not one path element") {
		t.Fatalf("err = %v, want a refusal naming the rig that escapes the town", err)
	}
}

func TestMangoRejectionsSectionFailsOnARecordWithNoInstant(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	writeTownFile(t, land.TownReviewsFile(town).Path,
		`{"bead":"gt-b","verdict":"approve","agent":"opus"}`+"\n")
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Run: func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "no instant") {
		t.Fatalf("err = %v, want a refusal naming the record with no instant", err)
	}
}

func TestMangoRejectionsSectionFailsWhenBdFails(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile,
		Run: func(context.Context, []string) ([]byte, error) { return nil, errors.New("dolt unreachable") },
	})
	if err == nil || !strings.Contains(err.Error(), "bd show in") {
		t.Fatalf("err = %v, want a refusal naming the store", err)
	}
}

func TestMangoRejectionsSectionFailsOnOutputThatIsNotABeadList(t *testing.T) {
	t.Parallel()
	// "[" is an array that does not parse, so it reaches the decoder's own
	// error rather than the array check in front of it.
	for _, out := range []string{"not json", "null", "  ", "["} {
		out := out
		t.Run(fmt.Sprintf("%q", out), func(t *testing.T) {
			t.Parallel()
			town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
			_, err := MangoRejectionsSection(context.Background(), Env{
				Town: town, ReadFile: os.ReadFile,
				Run: func(context.Context, []string) ([]byte, error) { return []byte(out), nil },
			})
			if err == nil || !strings.Contains(err.Error(), "not a bead list") {
				t.Fatalf("err = %v, want a refusal naming the unreadable listing", err)
			}
		})
	}
}

// TestMangoRejectionsSectionEscapesSeatNames drives a seat name holding a cell
// separator and a line break, which a review log can carry: an unescaped pipe
// would split the row and a line break would end it.
func TestMangoRejectionsSectionEscapesSeatNames(t *testing.T) {
	t.Parallel()
	seat := "opus|sonnet\nand-more"
	town := rejectionTown(t, land.ReviewRecord{
		BeadID: "gt-b", Verdict: land.VerdictApprove, Agent: seat, At: rejectionAt,
	})
	text, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile,
		Run: func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil },
	})
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	want := "| " + rejectionWeekOf + ` | opus\|sonnet and-more | 0 | 1 | 0.0% |`
	if !strings.Contains(text, want) {
		t.Errorf("text = %q, want the seat's row to hold %q", text, want)
	}
}

func TestMangoRejectionsSectionFailsOnAnUnreadableLog(t *testing.T) {
	t.Parallel()
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: t.TempDir(),
		ReadFile: func(string) ([]byte, error) {
			return nil, errors.New("permission denied")
		},
		Run: func(context.Context, []string) ([]byte, error) { return []byte(`[]`), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "reviews.jsonl") {
		t.Fatalf("err = %v, want a refusal naming the log", err)
	}
}

func TestMangoRejectionsSectionNeedsItsSeams(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		env  Env
		want string
	}{
		{"no town", Env{ReadFile: os.ReadFile, Run: func(context.Context, []string) ([]byte, error) { return nil, nil }}, "Env.Town is unset"},
		{"no reader", Env{Town: t.TempDir(), Run: func(context.Context, []string) ([]byte, error) { return nil, nil }}, "Env.ReadFile is unset"},
		{"no runner", Env{Town: t.TempDir(), ReadFile: os.ReadFile}, "Env.Run is unset"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, err := MangoRejectionsSection(context.Background(), c.env)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// rejectionDigestRunner answers bd from the rejection notes and every other
// command from the fake mango, so one section can drive both readers.
func rejectionDigestRunner(t *testing.T, bd, mango Runner) Runner {
	t.Helper()
	return func(ctx context.Context, argv []string) ([]byte, error) {
		if len(argv) > 0 && argv[0] == "bd" {
			return bd(ctx, argv)
		}
		return mango(ctx, argv)
	}
}

// TestMangoSectionCarriesTheRejectionTables is the digest's side of the join:
// the findings section MangoSection renders ends with the refusal rate, so a
// rate is readable without a second command.
func TestMangoSectionCarriesTheRejectionTables(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	mango, _ := mangoSectionRun(t, mangoSectionResult)
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"),
		land.ReviewRecord{BeadID: "gt-b", Rig: "gastown", Verdict: land.VerdictApprove, Agent: "sonnet", At: rejectionAt})
	var asked []string
	run := rejectionDigestRunner(t,
		rejectionBd(t, map[string]string{"gt-a": rejectionNote(1, "failing branch has no assertion")}, &asked), mango)
	text, err := MangoSection(context.Background(), Env{Town: town, ReadFile: os.ReadFile, Run: run},
		dir, "/opt/mango", MangoSectionOptions{MinPositives: 1})
	if err != nil {
		t.Fatalf("MangoSection: %v", err)
	}
	for _, want := range []string{
		"### Rejections by pattern",
		"### Rejections by agent seat",
		"| " + rejectionWeekOf + " | refusal_untested | 1 | 2 | 50.0% |",
		"| " + rejectionWeekOf + " | opus | 1 | 1 | 100.0% |",
		"| " + rejectionWeekOf + " | sonnet | 0 | 1 | 0.0% |",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("text = %q, want it to hold %q", text, want)
		}
	}
}

// TestMangoSectionFailsWhenTheRejectionLogFails holds the digest side to the
// same rule as the section: an unreadable review log is an error, never a
// findings block with the tables quietly missing.
func TestMangoSectionFailsWhenTheRejectionLogFails(t *testing.T) {
	t.Parallel()
	dir := mangoSectionTown(t, mangoSectionRows())
	mango, _ := mangoSectionRun(t, mangoSectionResult)
	reviewLog := land.TownReviewsFile("/town").Path
	readFile := func(path string) ([]byte, error) {
		if path == reviewLog {
			return nil, errors.New("permission denied")
		}
		return os.ReadFile(path)
	}
	_, err := MangoSection(context.Background(), Env{Town: "/town", ReadFile: readFile, Run: mango},
		dir, "/opt/mango", MangoSectionOptions{MinPositives: 1})
	if err == nil || !strings.Contains(err.Error(), "reviews.jsonl") {
		t.Fatalf("err = %v, want a refusal naming the review log", err)
	}
}
