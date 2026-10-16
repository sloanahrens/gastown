package cmd

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/question"
)

// The panel's clock: every fixture below is written against this instant, so an
// age is the one the test meant.
var questionsNow = time.Date(2026, 10, 8, 15, 0, 0, 0, time.UTC)

// fakeQuestions answers a dashQuestionsReader from fixtures, and records which
// beads' comments were read so a test can pin what the pane costs.
type fakeQuestions struct {
	issues      []*beads.Issue
	listErr     error
	comments    map[string][]beads.Comment
	commentsErr error
	reads       []string
}

func (f *fakeQuestions) reader() *dashQuestionsReader {
	return &dashQuestionsReader{
		list: func() ([]*beads.Issue, error) { return f.issues, f.listErr },
		comments: func(id string) ([]beads.Comment, error) {
			f.reads = append(f.reads, id)
			if f.commentsErr != nil {
				return nil, f.commentsErr
			}
			return f.comments[id], nil
		},
	}
}

// questionIssue is a question bead with a description shaped the way the
// overseer's tool writes one, filed the way gt ask files one: at the default
// priority and with no effort tag.
func questionIssue(id, title, qtext, recommended, held string, at time.Time) *beads.Issue {
	return &beads.Issue{
		ID:        id,
		Title:     title,
		Priority:  question.Priority,
		CreatedAt: at.Format(time.RFC3339),
		Description: "## Question\n" + qtext +
			"\n\n## Recommended answer\n" + recommended +
			"\n\n## Held work\n" + held,
	}
}

// questionAt is a question asked at a priority and an instant, for the queue's
// ordering: what it was asked is not what the ordering test is about.
func questionAt(id string, priority int, at time.Time) *beads.Issue {
	return &beads.Issue{
		ID: id, Title: "a question", Priority: priority, CreatedAt: at.Format(time.RFC3339),
		Description: "## Question\nq\n\n## Recommended answer\na\n\n## Held work\nnone",
	}
}

// comment is one comment as bd holds it.
func comment(text string, at time.Time) beads.Comment {
	return beads.Comment{Text: text, CreatedAt: at.Format(time.RFC3339)}
}

func questionIDs(rows []dashboard.QuestionRow) []string {
	var out []string
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// TestHeldCountFromBdListJSON: the held count is read off `bd list --json`'s
// dependent_count (omitempty on the Issue field, so a renamed key decodes to
// zero without an error). Every other test hands the reader a beads.Issue with
// the field already set, so none would notice bd dropping or renaming it. This
// one starts from the JSON, in the shape bd prints (testdata/bd_list_question.json,
// one question holding two beads), through the same decode beads.List runs.
func TestHeldCountFromBdListJSON(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile(filepath.Join("testdata", "bd_list_question.json"))
	if err != nil {
		t.Fatalf("bd list payload fixture: %v", err)
	}
	var issues []*beads.Issue
	if err := json.Unmarshal(data, &issues); err != nil {
		t.Fatalf("decode the bd list payload: %v", err)
	}

	f := &fakeQuestions{issues: issues}
	q := f.reader().read(questionsNow)
	if q.Unavailable || len(q.Rows) != 1 {
		t.Fatalf("the payload's one question reads as %+v, want one row", q)
	}
	if got := q.Rows[0].HeldCount; got != 2 {
		t.Errorf("HeldCount = %d, want the payload's dependent_count of 2", got)
	}
}

// The panel's query names the label and the open statuses, and carries no
// priority filter: ListOptions' zero value is Priority 0, which bd reads as
// "P0 only", so a question at any other priority would vanish from a pane that
// asked that way. No other test can see this — the query is built inside the
// reader's closure — and a wrong one reads as a town with no questions, which
// is why the sentinel is pinned here.
func TestQuestionListAsksForEveryPriority(t *testing.T) {
	t.Parallel()

	if question.ListOptions.Priority != -1 {
		t.Errorf("the panel's query filters by priority %d, want the no-filter sentinel -1", question.ListOptions.Priority)
	}
	if question.ListOptions.Label != question.Label || question.ListOptions.Status != question.Statuses {
		t.Errorf("the panel's query is %+v, want the label %q at %q", question.ListOptions, question.Label, question.Statuses)
	}
	if question.ListOptions.Limit != 0 {
		t.Errorf("the panel's query caps at %d, want every open question", question.ListOptions.Limit)
	}
}

// The pane shows the question the way the overseer wrote it: the title, the two
// sections the description carries, the work the question holds up, how long it
// has waited, and what its comments say.
func TestQuestionsReadsAWellFormedQuestion(t *testing.T) {
	t.Parallel()

	asked := questionsNow.Add(-2 * time.Hour)
	issue := questionIssue("gt-cr62m", "retry policy",
		"Should the landing worker retry a rejected merge?",
		"Yes: retry twice, then raise an escalation.",
		"gt-wisp-590yk, hq-cv-abc and nothing else",
		asked)
	// The two holds bd counts on the bead, and the effort tag gt ask filed it
	// with — both read off the bead, not the description.
	issue.DependentCount = 2
	issue.Labels = []string{question.Label, "effort:session"}
	f := &fakeQuestions{
		issues: []*beads.Issue{issue},
		comments: map[string][]beads.Comment{
			"gt-cr62m": {comment("[overseer] this one is yours", asked)},
		},
	}

	q := f.reader().read(questionsNow)
	if q.Unavailable {
		t.Fatal("a readable list reads as unavailable")
	}
	if len(q.Rows) != 1 {
		t.Fatalf("the pane holds %d rows, want 1: %+v", len(q.Rows), q.Rows)
	}
	r := q.Rows[0]
	if r.ID != "gt-cr62m" || r.Title != "retry policy" {
		t.Errorf("row %q %q, want gt-cr62m retry policy", r.ID, r.Title)
	}
	if want := "Should the landing worker retry a rejected merge?"; r.Question != want {
		t.Errorf("question %q, want %q", r.Question, want)
	}
	if want := "Yes: retry twice, then raise an escalation."; r.Recommended != want {
		t.Errorf("recommended %q, want %q", r.Recommended, want)
	}
	if want := []string{"gt-wisp-590yk", "hq-cv-abc"}; !reflect.DeepEqual(r.Held, want) {
		t.Errorf("held %v, want %v (the section's prose is not an id)", r.Held, want)
	}
	if r.Priority != question.Priority || r.HeldCount != 2 || r.Effort != question.EffortSession {
		t.Errorf("priority %d held count %d effort %q, want P2 holding 2 with the session tag",
			r.Priority, r.HeldCount, r.Effort)
	}
	if r.AgeSec == nil || *r.AgeSec != 7200 {
		t.Errorf("age %v, want 7200 seconds", r.AgeSec)
	}
	if r.Comments != 1 || r.State != dashboard.QuestionAwaiting || r.Malformed {
		t.Errorf("comments %d state %q malformed %v, want 1 awaiting false", r.Comments, r.State, r.Malformed)
	}
}

// A description the overseer's tool did not shape still yields a row: the title
// is worth showing on its own, and Malformed is what tells the pane it cannot
// draw a question from it.
func TestQuestionsKeepsARowWhoseDescriptionIsMalformed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, description string }{
		{"no sections at all", "the overseer wrote this by hand"},
		{"no recommended answer", "## Question\nOnly the question is here."},
		{"no question", "## Recommended answer\nOnly the answer is here."},
		{"empty sections", "## Question\n\n## Recommended answer\n"},
	} {
		f := &fakeQuestions{issues: []*beads.Issue{{
			ID: "gt-abc12", Title: "a question", Description: tc.description,
			CreatedAt: questionsNow.Format(time.RFC3339),
		}}}
		q := f.reader().read(questionsNow)
		if q.Unavailable || len(q.Rows) != 1 {
			t.Fatalf("%s: unavailable %v with %d rows, want one row", tc.name, q.Unavailable, len(q.Rows))
		}
		r := q.Rows[0]
		if !r.Malformed {
			t.Errorf("%s: the row is not malformed: %+v", tc.name, r)
		}
		if r.ID != "gt-abc12" || r.Title != "a question" {
			t.Errorf("%s: the malformed row lost its title: %+v", tc.name, r)
		}
	}
}

// A question is answered once Sloan replies: the newest comment is what decides
// it, and the overseer's own comments are the ones that do not.
func TestQuestionsReadsAnsweredAndAwaitingFromTheNewestComment(t *testing.T) {
	t.Parallel()

	early, late := questionsNow.Add(-3*time.Hour), questionsNow.Add(-time.Hour)
	for _, tc := range []struct {
		name     string
		comments []beads.Comment
		want     string
	}{
		{"no comment at all", nil, dashboard.QuestionAwaiting},
		{"the overseer's own comment", []beads.Comment{
			comment("[overseer] which way?", late),
		}, dashboard.QuestionAwaiting},
		{"Sloan's reply", []beads.Comment{
			comment("yes, do that", late),
		}, dashboard.QuestionAnswered},
		{"Sloan's reply after the overseer asked again", []beads.Comment{
			comment("[overseer] which way?", early),
			comment("yes, do that", late),
		}, dashboard.QuestionAnswered},
		{"the overseer asking again after Sloan's reply", []beads.Comment{
			comment("yes, do that", early),
			comment("[overseer] one more thing", late),
		}, dashboard.QuestionAwaiting},
		{"the overseer's prefix mid-comment", []beads.Comment{
			comment("the [overseer] said so", late),
		}, dashboard.QuestionAnswered},
	} {
		f := &fakeQuestions{
			issues: []*beads.Issue{questionIssue("gt-abc12", "a question",
				"which way?", "this way", "gt-wisp-1", late)},
			comments: map[string][]beads.Comment{"gt-abc12": tc.comments},
		}
		q := f.reader().read(questionsNow)
		if q.Unavailable || len(q.Rows) != 1 {
			t.Fatalf("%s: unavailable %v with %d rows", tc.name, q.Unavailable, len(q.Rows))
		}
		if got := q.Rows[0].State; got != tc.want {
			t.Errorf("%s: state %q, want %q", tc.name, got, tc.want)
		}
	}
}

// The queue is the operator's, so it is ordered by how long each question has
// waited within its priority, and a bead whose creation time does not parse
// comes last: an unreadable time must not read as the longest wait.
func TestQuestionsOrdersTheLongestWaitFirst(t *testing.T) {
	t.Parallel()

	f := &fakeQuestions{issues: []*beads.Issue{
		questionIssue("gt-third", "asked an hour ago", "q", "a", "", questionsNow.Add(-time.Hour)),
		{ID: "gt-untimed", Title: "no time at all", Priority: question.Priority,
			Description: "## Question\nq\n## Recommended answer\na", CreatedAt: "not a time"},
		questionIssue("gt-first", "asked a day ago", "q", "a", "", questionsNow.Add(-24*time.Hour)),
	}}
	q := f.reader().read(questionsNow)
	if want := []string{"gt-first", "gt-third", "gt-untimed"}; !reflect.DeepEqual(questionIDs(q.Rows), want) {
		t.Errorf("the queue is %v, want %v", questionIDs(q.Rows), want)
	}
	if r := q.Rows[2]; r.AgeSec != nil {
		t.Errorf("a bead with no parseable creation time carries an age: %v", *r.AgeSec)
	}
	if r := q.Rows[0]; r.AgeSec == nil || *r.AgeSec != int64(24*time.Hour/time.Second) {
		t.Errorf("the oldest question's age is %v, want a day", r.AgeSec)
	}
}

// The queue is a priority queue like the work queue: the most urgent priority
// first, and only then how long the question has waited, so a P0 asked a minute
// ago is read before a P3 that has waited a week. A bead with no parseable time
// still sorts after the timed ones at its own priority, and never above a
// priority it does not hold.
func TestQuestionsOrdersByPriorityThenTheLongestWait(t *testing.T) {
	t.Parallel()

	f := &fakeQuestions{issues: []*beads.Issue{
		questionAt("gt-p3-old", 3, questionsNow.Add(-7*24*time.Hour)),
		questionAt("gt-p1-new", 1, questionsNow.Add(-time.Minute)),
		questionAt("gt-p0-old", 0, questionsNow.Add(-2*time.Hour)),
		{
			ID: "gt-p1-untimed", Title: "no time at all", Priority: 1,
			Description: "## Question\nq\n## Recommended answer\na", CreatedAt: "not a time",
		},
		questionAt("gt-p0-new", 0, questionsNow.Add(-time.Hour)),
		questionAt("gt-p3-new", 3, questionsNow.Add(-2*time.Hour)),
	}}
	q := f.reader().read(questionsNow)
	want := []string{"gt-p0-old", "gt-p0-new", "gt-p1-new", "gt-p1-untimed", "gt-p3-old", "gt-p3-new"}
	if got := questionIDs(q.Rows); !reflect.DeepEqual(got, want) {
		t.Errorf("the queue is %v, want %v", got, want)
	}
}

// The pane is bounded: the twenty questions that have waited longest are the
// ones read, and the rest are counted in More. Only the drawn rows cost a
// comment read, so a town with a hundred questions does not pay for a hundred.
func TestQuestionsCapsThePaneAndCountsTheRest(t *testing.T) {
	t.Parallel()

	var issues []*beads.Issue
	for i := 0; i < dashQuestionRows+3; i++ {
		id := "gt-q" + string(rune('a'+i))
		issues = append(issues, questionIssue(id, "question "+id, "q", "a", "",
			questionsNow.Add(-time.Duration(i)*time.Minute)))
	}
	f := &fakeQuestions{issues: issues}

	q := f.reader().read(questionsNow)
	if q.Unavailable {
		t.Fatal("a long queue reads as unavailable")
	}
	if len(q.Rows) != dashQuestionRows || q.More != 3 {
		t.Fatalf("the pane holds %d rows with %d more, want %d and 3", len(q.Rows), q.More, dashQuestionRows)
	}
	if len(f.reads) != dashQuestionRows {
		t.Errorf("the read asked for %d beads' comments, want one per drawn row", len(f.reads))
	}
	// The drawn twenty are the longest waits: the last bead listed is the
	// oldest, and the three the pane leaves out are the newest three.
	if q.Rows[0].ID != "gt-qw" {
		t.Errorf("the pane starts at %q, want the question that waited longest", q.Rows[0].ID)
	}
	if q.Rows[dashQuestionRows-1].ID != "gt-qd" {
		t.Errorf("the pane ends at %q, want the twentieth-longest wait", q.Rows[dashQuestionRows-1].ID)
	}
}

// A read the panel could not make is a named state and never an empty list: the
// pane has to be able to say "the question could not be read" rather than "the
// overseer has asked nothing".
func TestQuestionsNamesAReadItCouldNotMake(t *testing.T) {
	t.Parallel()

	readable := []*beads.Issue{questionIssue("gt-abc12", "a question", "q", "a", "", questionsNow)}
	for _, tc := range []struct {
		name string
		f    *fakeQuestions
	}{
		{"the list fails", &fakeQuestions{listErr: errors.New("bd: no such database")}},
		{"the comments for a row fail", &fakeQuestions{issues: readable, commentsErr: errors.New("bd: no such database")}},
	} {
		q := tc.f.reader().read(questionsNow)
		if !q.Unavailable {
			t.Errorf("%s: the pane reads as available: %+v", tc.name, q)
		}
		if len(q.Rows) != 0 {
			t.Errorf("%s: an unavailable read drew %d rows", tc.name, len(q.Rows))
		}
	}
}

// A town with no open question is an empty list, which is a different thing
// from a read that failed.
func TestQuestionsReadsNoQuestionsAsAnEmptyList(t *testing.T) {
	t.Parallel()

	q := (&fakeQuestions{}).reader().read(questionsNow)
	if q.Unavailable || q.More != 0 {
		t.Fatalf("unavailable %v more %d, want a readable empty list", q.Unavailable, q.More)
	}
	if len(q.Rows) != 0 {
		t.Errorf("the pane holds %d rows, want none", len(q.Rows))
	}
	if q.Rows == nil {
		t.Error("the page's payload has a null list of rows")
	}
}

// The description and the title are the overseer's own text, and the pane draws
// them with textContent: every control and invisible format character goes, an
// HTML tag stays the text it is, and a field longer than the cap is cut at the
// cap rather than drawn whole.
func TestQuestionsStripsControlCharactersAndCapsFields(t *testing.T) {
	t.Parallel()

	hostile := "line one\nline two\rcarriage\x07 bell\x1b[31m nul:\x00 rlo:‮right-to-left <b>bold</b>"
	long := strings.Repeat("é", question.FieldMax+200)
	f := &fakeQuestions{issues: []*beads.Issue{questionIssue(
		"gt-abc12", "a title\x07", hostile, long, "gt-wisp-1", questionsNow)}}

	q := f.reader().read(questionsNow)
	if q.Unavailable || len(q.Rows) != 1 {
		t.Fatalf("unavailable %v with %d rows", q.Unavailable, len(q.Rows))
	}
	r := q.Rows[0]
	for _, want := range []string{"line one line two", "carriage", "bell", "[31m", "nul:", "rlo", "right-to-left", "<b>bold</b>"} {
		if !strings.Contains(r.Question, want) {
			t.Errorf("the question lost %q: %q", want, r.Question)
		}
	}
	if n := len([]rune(r.Recommended)); n != question.FieldMax {
		t.Errorf("an over-long answer is %d runes, want the cap of %d", n, question.FieldMax)
	}
	for _, field := range []string{r.Title, r.Question, r.Recommended} {
		for _, run := range field {
			if unicode.IsControl(run) || unicode.Is(unicode.Cf, run) {
				t.Errorf("the pane would draw the invisible rune %U: %q", run, field)
			}
		}
	}
	if strings.ContainsRune(r.Question, '\n') {
		t.Error("the question is drawn across lines")
	}
}
