package question

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestQuestionHeldIDs(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body string
		want []string
	}{
		{"prose line keeps its ids", "gt-wisp-590yk, hq-cv-abc and nothing else", []string{"gt-wisp-590yk", "hq-cv-abc"}},
		{"bullet takes the leading id only", "- gt-abc (the follow-up to gt-xyz; needs a re-run)", []string{"gt-abc"}},
		{"one id per bullet, annotations dropped", "- gt-abc e-stop pending\n- hq-cv-def: a follow-up\n- gt-wisp-590yk", []string{"gt-abc", "hq-cv-def", "gt-wisp-590yk"}},
		{"stub line names no bead", "- stub: re-run once the follow-up lands", nil},
		{"none line names no bead", "- none", nil},
		{"bare none and blank lines", "none\n\n  \n", nil},
		{"dotted id", "- gt-3t8.1 (re-run)", []string{"gt-3t8.1"}},
		{"empty bullet", "-\n- ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := HeldIDs(tc.body); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("HeldIDs(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

// A question is answered when its newest comment is not the overseer's.
func TestQuestionAnsweredReadsTheNewestComment(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	c := func(text string, min int) beads.Comment {
		return beads.Comment{Text: text, CreatedAt: at.Add(time.Duration(min) * time.Minute).Format(time.RFC3339)}
	}
	cases := []struct {
		name     string
		comments []beads.Comment
		want     bool
	}{
		{"no comment", nil, false},
		{"overseer note only", []beads.Comment{c("[overseer] asked", 0)}, false},
		{"sloan after the overseer", []beads.Comment{c("[overseer] asked", 0), c("yes", 5)}, true},
		{"overseer after sloan", []beads.Comment{c("[overseer] asked", 0), c("yes", 5), c("[overseer] on it", 9)}, false},
		{"listed out of order, newest by time wins", []beads.Comment{c("yes", 5), c("[overseer] asked", 0)}, true},
		{"same second, the one listed last wins", []beads.Comment{c("yes", 5), c("[overseer] on it", 5)}, false},
	}
	for _, tc := range cases {
		if got := Answered(tc.comments); got != tc.want {
			t.Errorf("%s: Answered = %v, want %v", tc.name, got, tc.want)
		}
	}
}

const askBody = "## Question\nRetry once?\n\n## Recommended answer\nYes.\n\n## Held work\n- gt-abc1 (the rework)\n- stub: split the landing gate\nnone\n\n## Facts\nTwo of ten passed on retry.\n"

func TestAskParsesTheBodyGtAskFiles(t *testing.T) {
	t.Parallel()
	ask, err := ParseAsk("  Retry policy  ", askBody)
	if err != nil {
		t.Fatal(err)
	}
	if ask.Title != "Retry policy" {
		t.Errorf("title = %q, want it trimmed", ask.Title)
	}
	wantHolds := []Hold{{ID: "gt-abc1"}, {Stub: "split the landing gate"}}
	if !reflect.DeepEqual(ask.Holds, wantHolds) {
		t.Errorf("holds = %+v, want %+v", ask.Holds, wantHolds)
	}

	// What it files reads back as the same Question, Recommended and Held.
	f := Parse(&beads.Issue{Title: ask.Title, Description: ask.Body})
	if f.Question != "Retry once?" || f.Recommended != "Yes." || f.Malformed {
		t.Errorf("parsed back %+v, want the question and the recommended answer", f)
	}
	if !reflect.DeepEqual(f.Held, []string{"gt-abc1"}) {
		t.Errorf("held read back = %v, want the one bead id", f.Held)
	}
}

func TestAskRefusesWhatSloanCannotAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, title, body, want string
	}{
		{"no title", " ", askBody, "title"},
		{"title over the limit", strings.Repeat("x", TitleMax+1), askBody, "over the 120 limit"},
		{"no question section", "t", "## Recommended answer\nYes.", `"## Question"`},
		{"empty question section", "t", "## Question\n  \n## Recommended answer\nYes.", `"## Question"`},
		{"no recommended answer", "t", "## Question\nRetry?\n## Facts\nf", `"## Recommended answer"`},
		{"held line that is no id", "t", "## Question\nq\n## Recommended answer\nr\n## Held work\n- the rework\n", "not a bead id"},
		{"empty stub title", "t", "## Question\nq\n## Recommended answer\nr\n## Held work\n- stub:\n", "empty stub"},
	}
	for _, tc := range cases {
		_, err := ParseAsk(tc.title, tc.body)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one naming %q", tc.name, err, tc.want)
		}
	}
}

func TestAskHeldWorkAndFactsAreOptional(t *testing.T) {
	t.Parallel()
	ask, err := ParseAsk("t", "## Question\nq\n## Recommended answer\nr")
	if err != nil || len(ask.Holds) != 0 {
		t.Errorf("ParseAsk = %+v, %v; want a question that holds nothing", ask, err)
	}
}

func TestAskReplaceStubPointsTheLineAtTheNewBead(t *testing.T) {
	t.Parallel()
	got := ReplaceStub(askBody, "split the landing gate", "hq-new9")
	if !strings.Contains(got, "\n- hq-new9\n") || strings.Contains(got, "stub:") {
		t.Errorf("body = %q, want the stub line replaced", got)
	}
	if !reflect.DeepEqual(HeldIDs(Sections(got)[HeadingHeld]), []string{"gt-abc1", "hq-new9"}) {
		t.Errorf("held = %v, want the page to list the new bead", HeldIDs(Sections(got)[HeadingHeld]))
	}
	// A stub line outside ## Held work is prose, left alone.
	prose := "## Question\n- stub: split the landing gate\n## Held work\nnone"
	if got := ReplaceStub(prose, "split the landing gate", "hq-new9"); got != prose {
		t.Errorf("body = %q, want it unchanged", got)
	}
}
