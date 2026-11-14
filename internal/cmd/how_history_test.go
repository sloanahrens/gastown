package cmd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/steveyegge/gastown/internal/taskmap"
)

// fakeHistory is the runner the history tests drive `gt how --history` with: it
// records what it was asked for and returns fixed lines, so no test starts hm.
type fakeHistory struct {
	lines    []string
	err      error
	calls    int
	gotText  string
	gotLimit int
}

func (f *fakeHistory) Query(_ context.Context, text string, limit int) ([]string, error) {
	f.calls++
	f.gotText, f.gotLimit = text, limit
	if f.err != nil {
		return nil, f.err
	}
	return f.lines, nil
}

// howListing is what runHow prints for args, the text the history section
// follows.
func howListing(t *testing.T, args []string) string {
	t.Helper()
	var listing bytes.Buffer
	if err := runHow(&listing, howFixture(), args); err != nil {
		t.Fatalf("runHow(%v): %v", args, err)
	}
	return listing.String()
}

func TestHmHistoryRunsTheQueryAndRendersHits(t *testing.T) {
	t.Parallel()

	const hits = `[
		{"rank": 2, "score": 0.41, "kind": "bead", "rig": "gastown", "id": "gt-bbb", "title": "Second hit"},
		{"rank": 1, "score": 0.47, "kind": "bead", "rig": "gastown", "id": "gt-aaa", "title": "First hit"}
	]`
	var gotArgv []string
	run := func(_ context.Context, argv []string) ([]byte, error) {
		gotArgv = argv
		return []byte(hits), nil
	}

	lines, err := hmHistory{run: run}.Query(context.Background(), "land roll", historyLimit)
	if err != nil {
		t.Fatalf("hmHistory.Query: %v", err)
	}

	wantArgv := []string{"hm", "query", "--json", "--", "land roll"}
	if len(gotArgv) != len(wantArgv) {
		t.Fatalf("ran %q, want %q", gotArgv, wantArgv)
	}
	for i := range wantArgv {
		if gotArgv[i] != wantArgv[i] {
			t.Fatalf("ran %q, want %q", gotArgv, wantArgv)
		}
	}
	want := []string{
		"#1 0.47 bead gastown/gt-aaa — First hit",
		"#2 0.41 bead gastown/gt-bbb — Second hit",
	}
	if len(lines) != len(want) {
		t.Fatalf("lines = %q, want %q", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, lines[i], want[i])
		}
	}
}

func TestHmHistoryReportsAFailedQueryWithNoLines(t *testing.T) {
	t.Parallel()

	run := func(context.Context, []string) ([]byte, error) {
		return []byte(`[{"rank": 1, "id": "gt-aaa"}]`), errors.New("hm query: exit status 1")
	}

	lines, err := hmHistory{run: run}.Query(context.Background(), "land roll", historyLimit)
	if err == nil {
		t.Fatal("hmHistory.Query returned hits for a failed query")
	}
	if lines != nil {
		t.Errorf("lines = %q, want none alongside the error", lines)
	}
}

func TestHowHistoryPrintsHitsInOrder(t *testing.T) {
	t.Parallel()

	hits := []string{
		"#1 0.47 bead gastown/gt-aaa — First hit",
		"#2 0.41 bead gastown/gt-bbb — Second hit",
	}
	f := &fakeHistory{lines: hits}

	var out bytes.Buffer
	if err := runHowHistory(context.Background(), &out, howFixture(), f, []string{"roll"}); err != nil {
		t.Fatalf("runHowHistory: %v", err)
	}

	want := howListing(t, []string{"roll"}) +
		"history:\n" +
		hits[0] + "\n" +
		hits[1] + "\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestHowHistoryQueriesGroupDescriptionAndWord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		args     []string
		wantText string
	}{
		{
			name:     "a group adds its description",
			args:     []string{"roll"},
			wantText: taskmap.Describe("roll") + " roll",
		},
		{
			name:     "a word that names no group is the text on its own",
			args:     []string{"rolls"},
			wantText: "rolls",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeHistory{}
			var out bytes.Buffer
			if err := runHowHistory(context.Background(), &out, howFixture(), f, tc.args); err != nil {
				t.Fatalf("runHowHistory(%v): %v", tc.args, err)
			}
			if f.gotText != tc.wantText {
				t.Errorf("query text = %q, want %q", f.gotText, tc.wantText)
			}
			if f.gotLimit != historyLimit {
				t.Errorf("query limit = %d, want %d", f.gotLimit, historyLimit)
			}
		})
	}
}

func TestHowHistoryDegradesOnQueryError(t *testing.T) {
	t.Parallel()

	f := &fakeHistory{err: errors.New("hm query: exit status 1")}

	var out bytes.Buffer
	if err := runHowHistory(context.Background(), &out, howFixture(), f, []string{"roll"}); err != nil {
		t.Fatalf("runHowHistory returned %v, want the section to degrade instead", err)
	}

	want := howListing(t, []string{"roll"}) + "history: not available (hm query: exit status 1)\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestHowHistoryKeepsAWrappedReasonToOneLine(t *testing.T) {
	t.Parallel()

	f := &fakeHistory{err: errors.New("hm query: exit status 1\nsecond line")}

	var out bytes.Buffer
	if err := runHowHistory(context.Background(), &out, howFixture(), f, []string{"roll"}); err != nil {
		t.Fatalf("runHowHistory: %v", err)
	}

	want := howListing(t, []string{"roll"}) + "history: not available (hm query: exit status 1 second line)\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestHowHistoryWithoutAWord(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		args []string
	}{
		{name: "no argument", args: nil},
		{name: "an empty word", args: []string{"  "}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			f := &fakeHistory{}
			var out bytes.Buffer
			if err := runHowHistory(context.Background(), &out, howFixture(), f, tc.args); err != nil {
				t.Fatalf("runHowHistory(%v): %v", tc.args, err)
			}
			if f.calls != 0 {
				t.Errorf("queried hm %d times with no word to look up", f.calls)
			}
			want := howListing(t, tc.args) + "history: not available (no word to look up)\n"
			if out.String() != want {
				t.Errorf("output = %q, want %q", out.String(), want)
			}
		})
	}
}

func TestHowHistoryNoHits(t *testing.T) {
	t.Parallel()

	f := &fakeHistory{lines: []string{"", "  "}}

	var out bytes.Buffer
	if err := runHowHistory(context.Background(), &out, howFixture(), f, []string{"roll"}); err != nil {
		t.Fatalf("runHowHistory: %v", err)
	}

	want := howListing(t, []string{"roll"}) + "history: no hits\n"
	if out.String() != want {
		t.Errorf("output = %q, want %q", out.String(), want)
	}
}

func TestHowHistoryQueriesNothingWhenTheListingFails(t *testing.T) {
	t.Parallel()

	m := howFixture()
	m.describe = func(taskmap.Group) string { return "" }
	f := &fakeHistory{}

	var out bytes.Buffer
	if err := runHowHistory(context.Background(), &out, m, f, nil); err == nil {
		t.Fatal("runHowHistory listed groups with no descriptions, want an error")
	}
	if f.calls != 0 {
		t.Errorf("queried hm %d times before the listing failed", f.calls)
	}
	if out.Len() != 0 {
		t.Errorf("wrote %q before refusing", out.String())
	}
}
