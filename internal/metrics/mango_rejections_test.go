package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
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

// rejectionFixtures is one om finding per pattern, worded as om words it. The
// cases go through RejectionRates, so a rule the section stops applying fails
// here rather than only in the classification helper.
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
	{"new parsing and state logic added to a shell script", "script"},
	{"change touches files outside the stated scope", "out_of_scope"},
	{"a test that calls the helper directly", "call_site_untested"},
	{"an error branch treated as a file that does not apply", "helper_fail_open"},
	{"an error that echoes captured command output", "secrets_in_errors"},
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

func TestRejectionFixtureClassifiesThroughTheRates(t *testing.T) {
	t.Parallel()
	for _, c := range rejectionFixtures {
		c := c
		t.Run(c.want, func(t *testing.T) {
			t.Parallel()
			notes := map[string]string{"gt-a": rejectionNote(1, c.finding)}
			byPattern, _, err := RejectionRates([]land.ReviewRecord{rejectionRecord("gt-a", 1, "opus")}, notes)
			if err != nil {
				t.Fatalf("RejectionRates: %v", err)
			}
			want := RejectionWeekGroup{Week: rejectionWeekOf, Name: c.want, Verdicts: 1, Rejections: 1, Rate: 1}
			if len(byPattern) != 1 || byPattern[0] != want {
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
			notes := map[string]string{"gt-a": rejectionNote(1, c.finding)}
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
	notes := map[string]string{"gt-a": rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)") +
		"\n" + rejectionNote(2, "an error that echoes captured command output")}
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
		if byPattern[i] != want[i] {
			t.Errorf("byPattern[%d] = %+v, want %+v", i, byPattern[i], want[i])
		}
	}
}

func TestRejectionBlocksSkipAnAttemptThatOverflows(t *testing.T) {
	t.Parallel()
	notes := "MERGE REJECTION (attempt " + strings.Repeat("9", 40) + "): review - x"
	if blocks := rejectionBlocks(notes); len(blocks) != 0 {
		t.Fatalf("blocks = %+v, want none: no review names an attempt that long", blocks)
	}
}

func TestRejectionWithoutItsNoteIsNoNote(t *testing.T) {
	t.Parallel()
	records := []land.ReviewRecord{rejectionRecord("gt-a", 1, "opus")}
	byPattern, _, err := RejectionRates(records, map[string]string{"gt-a": rejectionNote(2, "fail-open")})
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
	notes := map[string]string{"gt-a": rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)")}
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
		if bySeat[i] != wantSeat[i] {
			t.Errorf("bySeat[%d] = %+v, want %+v", i, bySeat[i], wantSeat[i])
		}
	}
	wantPattern := []RejectionWeekGroup{
		{Week: rejectionWeekOf, Name: "fail_open", Verdicts: 3, Rejections: 1, Rate: 1.0 / 3},
	}
	if len(byPattern) != 1 || byPattern[0] != wantPattern[0] {
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

// rejectionBd is a bd that answers one bead list per rig store, recording the
// directories it was asked for.
func rejectionBd(t *testing.T, notes map[string]string, asked *[]string) Runner {
	t.Helper()
	return func(_ context.Context, argv []string) ([]byte, error) {
		if len(argv) < 3 || argv[0] != "bd" || argv[1] != "-C" {
			t.Fatalf("argv = %v, want a bd list in -C", argv)
		}
		*asked = append(*asked, argv[2])
		var rows []bdListBead
		for id, body := range notes {
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

func TestMangoRejectionsSectionWithoutALogSaysSo(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	text, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Run: func(context.Context, []string) ([]byte, error) { return nil, nil },
	})
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	if !strings.Contains(text, "No review records yet") {
		t.Errorf("text = %q, want it to say there are no records", text)
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
	if err == nil || !strings.Contains(err.Error(), "bd list in") {
		t.Fatalf("err = %v, want a refusal naming the store", err)
	}
}

func TestMangoRejectionsSectionFailsOnOutputThatIsNotABeadList(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile,
		Run: func(context.Context, []string) ([]byte, error) { return []byte("not json"), nil },
	})
	if err == nil || !strings.Contains(err.Error(), "not a bead list") {
		t.Fatalf("err = %v, want a refusal naming the unreadable listing", err)
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
		{"no reader", Env{Run: func(context.Context, []string) ([]byte, error) { return nil, nil }}, "Env.ReadFile is unset"},
		{"no runner", Env{ReadFile: os.ReadFile}, "Env.Run is unset"},
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
