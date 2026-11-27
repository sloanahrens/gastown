package rehearsal

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// closedAnswers answers the two reads Town.Bead makes for bead gt-1, with a
// bd show status of status and no comments.
func closedAnswers(status string) map[string]fakeAnswer {
	return map[string]fakeAnswer{
		"bd show gt-1 --json":     {out: `{"data":[{"id":"gt-1","status":"` + status + `"}]}`},
		"bd comments gt-1 --json": {out: `{"data":[]}`},
	}
}

func TestRehearsalClosedStateReadsStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		status string
		want   bool
	}{
		{name: "a closed bead is done", status: "closed", want: true},
		{name: "an open bead is not done", status: "open", want: false},
		{name: "an in_progress bead is not done", status: "in_progress", want: false},
		{name: "a deferred bead is not done", status: "deferred", want: false},
		{name: "an empty status is not done", status: "", want: false},
		{name: "a status that is not a bead status is not done", status: "done", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			state := closedState(Town{Run: fakeRun(closedAnswers(tc.status), &ran)})
			got, err := state(context.Background(), "gt-1")
			if err != nil {
				t.Fatalf("closedState(gt-1) error = %v, want nil", err)
			}
			if got != tc.want {
				t.Errorf("closedState(gt-1) with status %q = %v, want %v", tc.status, got, tc.want)
			}
		})
	}
}

func TestRehearsalClosedStateFailedRead(t *testing.T) {
	t.Parallel()
	const leaked = "bd: secret output"
	cause := errors.New("exit status 1")
	cases := []struct {
		name    string
		answers map[string]fakeAnswer
		named   string
	}{
		{name: "a failed show is false with its error wrapped", answers: map[string]fakeAnswer{
			"bd show gt-1 --json": {out: leaked, err: cause}}, named: "bd show gt-1"},
		{name: "a failed comments read is false with its error wrapped", answers: map[string]fakeAnswer{
			"bd show gt-1 --json":     {out: `{"data":[{"id":"gt-1","status":"closed"}]}`},
			"bd comments gt-1 --json": {out: leaked, err: cause}}, named: "bd comments gt-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var ran []string
			state := closedState(Town{Run: fakeRun(tc.answers, &ran)})
			got, err := state(context.Background(), "gt-1")
			if got {
				t.Errorf("closedState(gt-1) = true on a failed read, want false")
			}
			if !errors.Is(err, cause) {
				t.Errorf("closedState(gt-1) error = %v, want it to wrap %v", err, cause)
			}
			if err != nil && strings.Contains(err.Error(), leaked) {
				t.Errorf("closedState(gt-1) error %q carries command output", err)
			}
			if err != nil && !strings.Contains(err.Error(), tc.named) {
				t.Errorf("closedState(gt-1) error = %v, want it to name %q", err, tc.named)
			}
		})
	}
}
