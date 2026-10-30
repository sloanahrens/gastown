package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
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
	{"the diff edits a file outside the bead's Constraints", "out_of_scope"},
	{"a go.mod bump with an unexplained transitive dependency", "out_of_scope"},
	{"a test that calls the helper directly", "call_site_untested"},
	{"the test calls the helper, so deleting the call site leaves it green", "call_site_untested"},
	{"an error branch treated as a file that does not apply", "fail_open"},
	{"a build-constraint matcher error is read as a file that does not apply", "fail_open"},
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

// omRulesHeading opens the directive's list of rules, each naming the finding
// om files for it.
const omRulesHeading = "# What om rejects"

// omRuleBlocks is the directive's rules' text by rule number: the section's
// "## <n>. …" heading and the lines under it, to the next heading.
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

// rejectionStoreFake is the bead store the rejection section reads in these
// tests: the notes each directory's store holds, the error a directory's read
// gets instead, and a record of every read, so a test sees which store the
// section opened and which ids it asked for.
type rejectionStoreFake struct {
	notes map[string]map[string]string // dir -> bead id -> the notes its store holds
	fail  map[string]error             // dir -> the error that store's read gets
	// shown, for a directory in it, is the answer its store gives exactly: the
	// issues it holds for the ids asked, a nil or id-less issue included, which
	// is an answer a store does not give for a bead.
	shown map[string]map[string]*beads.Issue

	mu    sync.Mutex
	reads []rejectionStoreRead
}

// rejectionStoreRead is one read of one directory's store: the directory, and
// the ids it was asked, in the order it was asked.
type rejectionStoreRead struct {
	dir string
	ids []string
}

// seam is the store seam the section reads through: one store per directory,
// answering what the fake holds for it.
func (f *rejectionStoreFake) seam() BeadStores {
	return func(_ context.Context, dir string) BeadStore {
		return &rejectionFakeStore{f: f, dir: dir}
	}
}

// readDirs lists the directories whose store the section opened, read order
// included.
func (f *rejectionStoreFake) readDirs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	dirs := make([]string, 0, len(f.reads))
	for _, read := range f.reads {
		dirs = append(dirs, read.dir)
	}
	return dirs
}

// askedIDs lists the ids the store in dir was asked, read order included.
func (f *rejectionStoreFake) askedIDs(dir string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var ids []string
	for _, read := range f.reads {
		if read.dir == dir {
			ids = append(ids, read.ids...)
		}
	}
	return ids
}

// rejectionFakeStore is one directory's store as the section opened it.
type rejectionFakeStore struct {
	f   *rejectionStoreFake
	dir string
}

func (s *rejectionFakeStore) List(beads.ListOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the rejection test read the board")
}

func (s *rejectionFakeStore) ReadyFiltered(beads.ReadyOptions) ([]*beads.Issue, error) {
	return nil, errors.New("the rejection test read the ready queue")
}

func (s *rejectionFakeStore) Blocked() ([]*beads.Issue, error) {
	return nil, errors.New("the rejection test read the blocked list")
}

func (s *rejectionFakeStore) ShowMultiple(ids []string) (map[string]*beads.Issue, error) {
	f := s.f
	f.mu.Lock()
	f.reads = append(f.reads, rejectionStoreRead{dir: s.dir, ids: append([]string(nil), ids...)})
	f.mu.Unlock()
	if err := f.fail[s.dir]; err != nil {
		return nil, err
	}
	if exact, ok := f.shown[s.dir]; ok {
		out := make(map[string]*beads.Issue, len(ids))
		for _, id := range ids {
			if issue, ok := exact[id]; ok {
				out[id] = issue
			}
		}
		return out, nil
	}
	out := make(map[string]*beads.Issue, len(ids))
	for _, id := range ids {
		notes, ok := f.notes[s.dir][id]
		if !ok {
			continue // a bead the store does not hold is left out, not refused
		}
		out[id] = &beads.Issue{ID: id, Notes: notes}
	}
	return out, nil
}

func TestMangoRejectionsSectionRendersBothTables(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"),
		land.ReviewRecord{BeadID: "gt-b", Rig: "gastown", Verdict: land.VerdictApprove, Agent: "sonnet", At: rejectionAt})
	store := &rejectionStoreFake{notes: map[string]map[string]string{
		filepath.Join(town, "gastown"): {"gt-a": rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)")},
	}}
	env := Env{
		Town:     town,
		ReadFile: os.ReadFile,
		Beads:    store.seam(),
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
	if dirs := store.readDirs(); len(dirs) != 1 || dirs[0] != filepath.Join(town, "gastown") {
		t.Errorf("store read in %v, want the gastown store alone", dirs)
	}
}

// TestMangoRejectionsSectionFailsOnAMissingLog holds a missing log to the same
// rule as an unreadable one: the section must not read a log it could not find
// as a town that refused nothing.
func TestMangoRejectionsSectionFailsOnAMissingLog(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Beads: (&rejectionStoreFake{}).seam(),
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
	// The fixture answers by store, so the same id reaches two different notes.
	store := &rejectionStoreFake{notes: map[string]map[string]string{
		filepath.Join(town, "beads"):   {"gt-a": rejectionNote(1, "the change reads fine, only a style note")},
		filepath.Join(town, "gastown"): {"gt-a": rejectionNote(1, "unreadable status suppresses escalation (fail-open gate)")},
	}}
	text, err := MangoRejectionsSection(context.Background(), Env{Town: town, ReadFile: os.ReadFile, Beads: store.seam()})
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
	wantRead := []string{filepath.Join(town, "beads"), filepath.Join(town, "gastown")}
	if dirs := store.readDirs(); len(dirs) != 2 || dirs[0] != wantRead[0] || dirs[1] != wantRead[1] {
		t.Errorf("store read in %v, want %v", dirs, wantRead)
	}
}

// TestMangoRejectionsSectionFailsWhenBdOmitsABead is the join's refusal branch:
// a store that does not return a bead the log names is a broken read, not a
// refusal carrying no note. The error names the store's directory, so a broken
// read is locatable in a town with several rigs.
func TestMangoRejectionsSectionFailsWhenBdOmitsABead(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Beads: (&rejectionStoreFake{}).seam(),
	})
	if err == nil || !strings.Contains(err.Error(), "gt-a") {
		t.Fatalf("err = %v, want a refusal naming the bead the store did not return", err)
	}
	if want := filepath.Join(town, "gastown"); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want a refusal naming the store's directory %q", err, want)
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
		Town: town, ReadFile: os.ReadFile, Beads: (&rejectionStoreFake{}).seam(),
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
		Town: town, ReadFile: os.ReadFile, Beads: (&rejectionStoreFake{}).seam(),
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
		Beads: (&rejectionStoreFake{}).seam(),
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
		Town: town, ReadFile: os.ReadFile, Beads: (&rejectionStoreFake{}).seam(),
	})
	if err == nil || !strings.Contains(err.Error(), "no instant") {
		t.Fatalf("err = %v, want a refusal naming the record with no instant", err)
	}
}

func TestMangoRejectionsSectionFailsWhenBdFails(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
	dir := filepath.Join(town, "gastown")
	store := &rejectionStoreFake{fail: map[string]error{dir: errors.New("dolt unreachable")}}
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Beads: store.seam(),
	})
	if err == nil || !strings.Contains(err.Error(), "bd show in") {
		t.Fatalf("err = %v, want a refusal naming the store", err)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Fatalf("err = %v, want a refusal naming the store's directory %q", err, dir)
	}
	// The read's own failure is carried, so a broken store reads as broken
	// rather than as the bead being gone.
	if !strings.Contains(err.Error(), "dolt unreachable") {
		t.Fatalf("err = %v, want the store's read failure carried", err)
	}
}

// TestMangoRejectionsSectionFailsOnAnAnswerThatIsNotABead: a store answering a
// refused bead with something that is not a bead — no issue at all, or an issue
// carrying no id — fails the section rather than reading as a note-less
// refusal. It is the store seam's side of the case the pre-seam reader held
// when bd printed something that was not a bead list.
func TestMangoRejectionsSectionFailsOnAnAnswerThatIsNotABead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		issue *beads.Issue
	}{
		{"no issue", nil},
		{"an issue with no id", &beads.Issue{}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := rejectionTown(t, rejectionRecord("gt-a", 1, "opus"))
			dir := filepath.Join(town, "gastown")
			store := &rejectionStoreFake{shown: map[string]map[string]*beads.Issue{
				dir: {"gt-a": tc.issue},
			}}
			_, err := MangoRejectionsSection(context.Background(), Env{
				Town: town, ReadFile: os.ReadFile, Beads: store.seam(),
			})
			if err == nil || !strings.Contains(err.Error(), "gt-a") {
				t.Fatalf("err = %v, want a refusal naming the bead the store did not answer", err)
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
		Beads: (&rejectionStoreFake{}).seam(),
	})
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	want := "| " + rejectionWeekOf + ` | opus\|sonnet and-more | 0 | 1 | 0.0% |`
	if !strings.Contains(text, want) {
		t.Errorf("text = %q, want the seat's row to hold %q", text, want)
	}
}

// TestMangoRejectionsSectionSkipsAQueueLine: a follow-up queue line is the
// feed's bookkeeping rather than a verdict, so the section does not report it
// as a line it could not attribute (gt-3arxj).
func TestMangoRejectionsSectionSkipsAQueueLine(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t,
		land.ReviewRecord{BeadID: "gt-b", Rig: "gastown", Verdict: land.VerdictApprove, Score: 0.9, At: rejectionAt},
		land.ReviewRecord{BeadID: "gt-q", Rig: "gastown", At: rejectionAt, Queue: &land.FollowupQueue{
			Landed: "abc123", Score: 0.7,
			Findings: []land.ReviewFindingText{{File: "a.go", Line: 1, Title: "name the error", Claim: "it panics"}},
		}},
	)
	text, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile,
		Beads: (&rejectionStoreFake{}).seam(),
	})
	if err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	if strings.Contains(text, "not readable as records") {
		t.Errorf("text = %q, want the queue line skipped and uncounted", text)
	}
}

func TestMangoRejectionsSectionFailsOnAnUnreadableLog(t *testing.T) {
	t.Parallel()
	_, err := MangoRejectionsSection(context.Background(), Env{
		Town: t.TempDir(),
		ReadFile: func(string) ([]byte, error) {
			return nil, errors.New("permission denied")
		},
		Beads: (&rejectionStoreFake{}).seam(),
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
		{"no town", Env{ReadFile: os.ReadFile, Beads: (&rejectionStoreFake{}).seam()}, "Env.Town is unset"},
		{"no reader", Env{Town: t.TempDir(), Beads: (&rejectionStoreFake{}).seam()}, "Env.ReadFile is unset"},
		{"no store", Env{Town: t.TempDir(), ReadFile: os.ReadFile}, "Env.Beads is unset"},
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

// TestMangoRejectionsSectionRefusesABeadIDThatIsNotOne drives a refusal whose
// bead id the store must never be asked for: the id comes from the review log,
// and a store reads its ids as arguments, so a dash-leading one would reach bd
// as a flag. The refusal names the id and the store is never opened.
func TestMangoRejectionsSectionRefusesABeadIDThatIsNotOne(t *testing.T) {
	t.Parallel()
	for _, id := range []string{"--help", "-x"} {
		id := id
		t.Run(id, func(t *testing.T) {
			t.Parallel()
			store := &rejectionStoreFake{}
			town := rejectionTown(t, rejectionRecord(id, 1, "opus"))
			_, err := MangoRejectionsSection(context.Background(), Env{
				Town: town, ReadFile: os.ReadFile, Beads: store.seam(),
			})
			if err == nil || !strings.Contains(err.Error(), id) || !strings.Contains(err.Error(), "is not a bead id") {
				t.Fatalf("err = %v, want a refusal naming %q as an id that is not a bead id", err, id)
			}
			if len(store.readDirs()) != 0 {
				t.Errorf("the store was opened in %v, want no read for a bead id that is not one", store.readDirs())
			}
		})
	}
}

// TestRejectionStoresRefusesAnEmptyBeadID holds the store builder to the empty
// id, which the section's log reader drops before it reaches here: no answer a
// store could give for it would be a bead, so a caller that builds the record
// itself gets the same refusal.
func TestRejectionStoresRefusesAnEmptyBeadID(t *testing.T) {
	t.Parallel()
	_, err := rejectionStores("/town", []land.ReviewRecord{rejectionRecord("", 1, "opus")})
	if err == nil || !strings.Contains(err.Error(), "is not a bead id") {
		t.Fatalf("err = %v, want a refusal naming the empty bead id", err)
	}
}

// TestMangoRejectionsSectionAsksEachStoreForItsRefusedBeads holds the read to
// the beads the log refused, per rig directory: the store in a rig's directory
// is asked for exactly the beads that rig's refusals name — the ids the
// pre-seam reader passed as --id= flags — and a refusal that names no rig
// reaches the town's own store. An approved bead is never asked for, and the ids
// arrive sorted, so the read does not depend on the log's order.
func TestMangoRejectionsSectionAsksEachStoreForItsRefusedBeads(t *testing.T) {
	t.Parallel()
	town := rejectionTown(t,
		rejectionRecord("gt-c", 1, "opus"),
		land.ReviewRecord{BeadID: "gt-approved", Rig: "gastown", Verdict: land.VerdictApprove, Agent: "opus", At: rejectionAt},
		rejectionRecord("gt-a", 1, "opus"),
		land.ReviewRecord{BeadID: "gt-z", Verdict: land.VerdictRequestChanges, Agent: "opus", Attempt: 1, At: rejectionAt},
	)
	gastownDir := filepath.Join(town, "gastown")
	store := &rejectionStoreFake{notes: map[string]map[string]string{
		gastownDir: {"gt-a": rejectionNote(1, "failing branch has no assertion"), "gt-c": rejectionNote(1, "call site")},
		town:       {"gt-z": rejectionNote(1, "no shown output")},
	}}
	if _, err := MangoRejectionsSection(context.Background(), Env{
		Town: town, ReadFile: os.ReadFile, Beads: store.seam(),
	}); err != nil {
		t.Fatalf("MangoRejectionsSection: %v", err)
	}
	want := map[string][]string{
		gastownDir: {"gt-a", "gt-c"},
		town:       {"gt-z"},
	}
	for dir, ids := range want {
		if got := store.askedIDs(dir); !reflect.DeepEqual(got, ids) {
			t.Errorf("the store in %s was asked for %v, want %v", dir, got, ids)
		}
	}
}

// The store's read of the refused beads, recorded through the real seam over a
// fake bd: the seam runs `bd --allow-stale show --json <id>...` with its working
// directory set to the rig's own directory, where the pre-seam reader ran
// `bd -C <dir> show --json --id=<bead>...`. The argv therefore differs three
// ways: the seam adds --allow-stale, -C <dir> becomes the directory the store is
// opened in, and the --id= flags become the ids themselves, which isBeadID has
// already refused a dash-leading id from. A read may now be as stale as the
// store last synced, the one behaviour change gt-vu312's overseer accepted for
// the review group's read; a rejection note is written before the review record
// that carries it, so the stale window is the same one.
func TestLoadRejectionNotesRunsTheStoreSeamsArgv(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	gastownDir := filepath.Join(town, "gastown")
	var reads [][]string
	var storeDirs []string
	answer := func(_ context.Context, args []string) ([]byte, []byte, error) {
		// The seam's probe that bd takes --allow-stale, keyed the way
		// internal/beads' own tests key it.
		if showTestVersionProbe(args) {
			return []byte("bd version 1.2.2\n"), nil, nil
		}
		reads = append(reads, append([]string(nil), args...))
		return []byte(`[{"id":"gt-a","notes":"MERGE REJECTION (attempt 1): review\n"}]`), nil, nil
	}
	env := Env{Town: town, Beads: func(ctx context.Context, dir string) BeadStore {
		storeDirs = append(storeDirs, dir)
		return beads.New(dir, beads.WithCallContext(ctx), beads.WithRunner(answer))
	}}
	stores := []rejectionStore{{Rig: "gastown", Dir: gastownDir, Beads: []string{"gt-a"}}}
	notes, err := loadRejectionNotes(context.Background(), env, stores)
	if err != nil {
		t.Fatalf("loadRejectionNotes: %v", err)
	}
	want := []string{"--allow-stale", "show", "--json", "gt-a"}
	if len(reads) != 1 || !reflect.DeepEqual(reads[0], want) {
		t.Fatalf("the store ran %v, want one %v", reads, want)
	}
	if len(storeDirs) != 1 || storeDirs[0] != gastownDir {
		t.Errorf("the store was opened in %v, want %q: -C <dir> is the store's own directory now", storeDirs, gastownDir)
	}
	if got := notes[RejectionNoteKey{Rig: "gastown", Bead: "gt-a"}]; got == "" {
		t.Errorf("notes = %v, want the refused bead's notes read through the seam", notes)
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
	store := &rejectionStoreFake{notes: map[string]map[string]string{
		filepath.Join(town, "gastown"): {"gt-a": rejectionNote(1, "failing branch has no assertion")},
	}}
	text, err := MangoSection(context.Background(), Env{Town: town, ReadFile: os.ReadFile, ReadDir: listDir, Run: mango, Beads: store.seam()},
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
	_, err := MangoSection(context.Background(), Env{Town: "/town", ReadFile: readFile, ReadDir: listDir, Run: mango},
		dir, "/opt/mango", MangoSectionOptions{MinPositives: 1})
	if err == nil || !strings.Contains(err.Error(), "reviews.jsonl") {
		t.Fatalf("err = %v, want a refusal naming the review log", err)
	}
}
