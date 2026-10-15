package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/dashboard"
	"github.com/steveyegge/gastown/internal/question"
)

// memQuestionStore is the town database as the question commands see it: beads
// with labels and comments, in memory.
type memQuestionStore struct {
	now      time.Time
	issues   []*beads.Issue
	comments map[string][]beads.Comment
	deps     [][2]string // {issue, dependsOn}
	prio     map[string]int
	depErr   error
	nextID   int
}

func newMemQuestionStore(now time.Time) *memQuestionStore {
	return &memQuestionStore{now: now, comments: map[string][]beads.Comment{}, prio: map[string]int{}}
}

func (m *memQuestionStore) find(id string) *beads.Issue {
	for _, is := range m.issues {
		if is.ID == id {
			return is
		}
	}
	return nil
}

func (m *memQuestionStore) Create(o beads.CreateOptions) (*beads.Issue, error) {
	m.nextID++
	is := &beads.Issue{
		ID: fmt.Sprintf("hq-t%d", m.nextID), Title: o.Title, Labels: o.Labels,
		Description: o.Description, Status: "open", CreatedAt: m.now.Format(time.RFC3339),
	}
	m.issues = append(m.issues, is)
	m.prio[is.ID] = o.Priority
	return is, nil
}

func (m *memQuestionStore) Show(id string) (*beads.Issue, error) {
	if is := m.find(id); is != nil {
		return is, nil
	}
	return nil, errors.New("not found: " + id)
}

func (m *memQuestionStore) Update(id string, o beads.UpdateOptions) error {
	is := m.find(id)
	if is == nil {
		return errors.New("not found: " + id)
	}
	if o.Description != nil {
		is.Description = *o.Description
	}
	return nil
}

func (m *memQuestionStore) AddDependency(issue, dependsOn string) error {
	if m.depErr != nil {
		return m.depErr
	}
	m.deps = append(m.deps, [2]string{issue, dependsOn})
	return nil
}

func (m *memQuestionStore) AddComment(id, text string) error {
	m.comments[id] = append(m.comments[id], beads.Comment{Text: text, CreatedAt: m.now.Format(time.RFC3339)})
	return nil
}

func (m *memQuestionStore) List(o beads.ListOptions) ([]*beads.Issue, error) {
	var out []*beads.Issue
	for _, is := range m.issues {
		if beads.HasLabel(is, o.Label) {
			out = append(out, is)
		}
	}
	return out, nil
}

func (m *memQuestionStore) Comments(id string) ([]beads.Comment, error) { return m.comments[id], nil }

func openMem(m *memQuestionStore) func() (questionStore, error) {
	return func() (questionStore, error) { return m, nil }
}

const askTestBody = "## Question\nRetry a rejected landing once?\n\n## Recommended answer\nYes, once.\n\n## Held work\n- gt-abc1 (the rework)\n- stub: split the landing gate\n\n## Facts\nTwo of ten passed on retry.\n"

var askNow = time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)

// gt ask files the bead the dashboard reader parses back to the same
// Question, Recommended and Held fields, in the town database, under the
// label, open at priority 2, and holds the work it names.
func TestAskFilesABeadTheDashboardReaderParsesBack(t *testing.T) {
	t.Parallel()
	store := newMemQuestionStore(askNow)
	var out bytes.Buffer
	if err := askQuestion(strings.NewReader(askTestBody), &out, "Retry policy", openMem(store)); err != nil {
		t.Fatal(err)
	}

	q := store.find("hq-t1")
	if q == nil || !reflect.DeepEqual(q.Labels, []string{question.Label}) || q.Status != "open" || store.prio["hq-t1"] != 2 {
		t.Fatalf("question bead = %+v at priority %d, want label %s, open, priority 2", q, store.prio["hq-t1"], question.Label)
	}
	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); lines[0] != "hq-t1" ||
		!strings.Contains(lines[1], `gt answer hq-t1 "<answer>"`) {
		t.Errorf("output = %q, want the id and the answer command", out.String())
	}

	read := newQuestionsReader(store).read(askNow.Add(90 * time.Minute))
	if len(read.Rows) != 1 {
		t.Fatalf("rows = %+v, want the one question", read.Rows)
	}
	row := read.Rows[0]
	if row.Question != "Retry a rejected landing once?" || row.Recommended != "Yes, once." || row.Malformed {
		t.Errorf("row = %+v, want the question and the recommended answer read back", row)
	}
	// The held bead and the stub bead made for the second line.
	if !reflect.DeepEqual(row.Held, []string{"gt-abc1", "hq-t2"}) {
		t.Errorf("held = %v, want gt-abc1 and the stub bead hq-t2", row.Held)
	}
	if row.State != dashboard.QuestionAwaiting || row.AgeSec == nil || *row.AgeSec != 5400 {
		t.Errorf("state %q age %v, want awaiting at 5400s", row.State, row.AgeSec)
	}
	wantDeps := [][2]string{{"gt-abc1", "hq-t1"}, {"hq-t2", "hq-t1"}}
	if !reflect.DeepEqual(store.deps, wantDeps) {
		t.Errorf("deps = %v, want %v: each held bead depends on the question", store.deps, wantDeps)
	}
	if stub := store.find("hq-t2"); stub == nil || stub.Title != "split the landing gate" || !strings.Contains(stub.Description, "hq-t1") {
		t.Errorf("stub bead = %+v, want one titled for the stub and naming the question", stub)
	}
}

func TestAskRefusesABodyWithoutQuestionOrRecommendedAnswer(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"no question":    "## Recommended answer\nYes.\n## Held work\nnone",
		"no recommended": "## Question\nRetry?\n## Held work\nnone",
		"no sections":    "just prose",
	} {
		store := newMemQuestionStore(askNow)
		opened := false
		err := askQuestion(strings.NewReader(body), &bytes.Buffer{}, "t", func() (questionStore, error) {
			opened = true
			return store, nil
		})
		if err == nil {
			t.Errorf("%s: gt ask filed a question", name)
		}
		if opened || len(store.issues) != 0 {
			t.Errorf("%s: a refused question touched the database (opened=%v, beads=%d)", name, opened, len(store.issues))
		}
	}
}

// A hold that fails does not lose the question: its id is printed and the error
// names the hold, so the operator can finish it by hand.
func TestAskKeepsTheQuestionWhenAHoldFails(t *testing.T) {
	t.Parallel()
	store := newMemQuestionStore(askNow)
	store.depErr = errors.New("dep add failed")
	var out bytes.Buffer
	err := askQuestion(strings.NewReader(askTestBody), &out, "Retry policy", openMem(store))
	if err == nil || !strings.Contains(err.Error(), "hq-t1 was filed") || !strings.Contains(err.Error(), "gt-abc1") {
		t.Errorf("err = %v, want one naming the filed question and the failed hold", err)
	}
	if !strings.HasPrefix(out.String(), "hq-t1\n") {
		t.Errorf("output = %q, want the question id printed anyway", out.String())
	}
}

// gt answer comments without the prefix; --overseer adds it; neither can be
// mistaken for the other.
func TestAnswerAddsTheCommentTheConventionReads(t *testing.T) {
	t.Parallel()
	store := newMemQuestionStore(askNow)
	if err := askQuestion(strings.NewReader(askTestBody), &bytes.Buffer{}, "Retry policy", openMem(store)); err != nil {
		t.Fatal(err)
	}

	if err := answerQuestion(store, "hq-t1", " yes, once ", false); err != nil {
		t.Fatal(err)
	}
	if got := store.comments["hq-t1"]; len(got) != 1 || got[0].Text != "yes, once" {
		t.Fatalf("comments = %+v, want Sloan's answer with no prefix", got)
	}
	if state := questionState(store.comments["hq-t1"]); state != dashboard.QuestionAnswered {
		t.Errorf("state = %q, want answered", state)
	}

	if err := answerQuestion(store, "hq-t1", "acting on it", true); err != nil {
		t.Fatal(err)
	}
	if got := store.comments["hq-t1"][1].Text; got != "[overseer] acting on it" {
		t.Errorf("overseer comment = %q, want the [overseer] prefix", got)
	}
	if err := answerQuestion(store, "hq-t1", "[overseer] already prefixed", true); err != nil {
		t.Fatal(err)
	}
	if got := store.comments["hq-t1"][2].Text; got != "[overseer] already prefixed" {
		t.Errorf("overseer comment = %q, want the prefix once", got)
	}
	if state := questionState(store.comments["hq-t1"]); state != dashboard.QuestionAwaiting {
		t.Errorf("state = %q, want awaiting after the overseer's note", state)
	}
}

func TestAnswerRefusesWhatItCannotMarkCorrectly(t *testing.T) {
	t.Parallel()
	store := newMemQuestionStore(askNow)
	if err := askQuestion(strings.NewReader(askTestBody), &bytes.Buffer{}, "Retry policy", openMem(store)); err != nil {
		t.Fatal(err)
	}
	other, _ := store.Create(beads.CreateOptions{Title: "not a question", Labels: []string{"gt:task"}})

	for name, tc := range map[string]struct {
		id, text string
		overseer bool
		want     string
	}{
		"empty text":                    {"hq-t1", "  \n", false, "empty comment"},
		"unknown bead":                  {"hq-nope", "yes", false, "reading hq-nope"},
		"not a question":                {other.ID, "yes", false, "not an overseer question"},
		"answer that looks like a note": {"hq-t1", "[overseer] yes", false, "use --overseer"},
	} {
		err := answerQuestion(store, tc.id, tc.text, tc.overseer)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one containing %q", name, err, tc.want)
		}
	}
	if n := len(store.comments); n != 0 {
		t.Errorf("a refused answer commented: %v", store.comments)
	}
}

// gt questions lists the open ones with their ages and held work, oldest
// first, and --json prints the dashboard's payload.
func TestQuestionsListsOpenQuestionsWithAgeAndHeldWork(t *testing.T) {
	t.Parallel()
	store := newMemQuestionStore(askNow.Add(-26 * time.Hour))
	if err := askQuestion(strings.NewReader(askTestBody), &bytes.Buffer{}, "older question", openMem(store)); err != nil {
		t.Fatal(err)
	}
	store.now = askNow.Add(-20 * time.Minute)
	if err := askQuestion(strings.NewReader("## Question\nq\n## Recommended answer\nr\n## Held work\nnone"), &bytes.Buffer{}, "newer question", openMem(store)); err != nil {
		t.Fatal(err)
	}
	store.now = askNow
	if err := answerQuestion(store, "hq-t1", "go", false); err != nil {
		t.Fatal(err)
	}

	reader := newQuestionsReader(store)
	reader.all = true
	qs := reader.read(askNow)

	var table bytes.Buffer
	if err := writeQuestions(&table, qs, false); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(table.String()), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "ID") {
		t.Fatalf("table = %q, want a header and two rows", table.String())
	}
	for _, want := range []string{"hq-t1", "26h00m", "answered", "gt-abc1,hq-t2", "older question"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("first row %q has no %q", lines[1], want)
		}
	}
	for _, want := range []string{"hq-t3", "20m", "awaiting", "newer question"} {
		if !strings.Contains(lines[2], want) {
			t.Errorf("second row %q has no %q", lines[2], want)
		}
	}

	var js bytes.Buffer
	if err := writeQuestions(&js, qs, true); err != nil {
		t.Fatal(err)
	}
	var back dashboard.Questions
	if err := json.Unmarshal(js.Bytes(), &back); err != nil || len(back.Rows) != 2 || back.Rows[0].ID != "hq-t1" {
		t.Errorf("json = %s (%v), want the dashboard payload with both rows", js.String(), err)
	}

	var none bytes.Buffer
	if err := writeQuestions(&none, &dashboard.Questions{}, false); err != nil || !strings.Contains(none.String(), "no open questions") {
		t.Errorf("empty table = %q (%v)", none.String(), err)
	}
}

// gt questions lists the whole queue, where the pane stops at its row cap.
func TestQuestionsListsPastThePaneCap(t *testing.T) {
	t.Parallel()
	store := newMemQuestionStore(askNow)
	for i := 0; i < dashQuestionRows+3; i++ {
		if _, err := store.Create(beads.CreateOptions{
			Title: fmt.Sprintf("q%d", i), Labels: []string{question.Label},
			Description: "## Question\nq\n## Recommended answer\nr",
		}); err != nil {
			t.Fatal(err)
		}
	}
	pane := newQuestionsReader(store).read(askNow)
	if len(pane.Rows) != dashQuestionRows || pane.More != 3 {
		t.Fatalf("pane rows %d more %d, want the cap and 3 more", len(pane.Rows), pane.More)
	}
	all := newQuestionsReader(store)
	all.all = true
	if got := all.read(askNow); len(got.Rows) != dashQuestionRows+3 || got.More != 0 {
		t.Errorf("gt questions rows %d more %d, want every question", len(got.Rows), got.More)
	}
}
