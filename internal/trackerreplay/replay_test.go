package trackerreplay

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/tracker/api"
)

// Unit tier: a scripted feed and a recording runner, no process and no clock.

// fakeSource serves events in pages of pageSize, after the seq asked for, and
// reports firstSeq as the oldest seq it still holds. Like the store, it echoes
// the cursor on an empty page.
type fakeSource struct {
	events   []api.Event
	pageSize int
	firstSeq int64
}

func (f *fakeSource) Events(_ context.Context, after int64, _ int) (api.EventsPage, error) {
	var out []api.Event
	for _, e := range f.events {
		if e.Seq > after && (f.pageSize == 0 || len(out) < f.pageSize) {
			out = append(out, e)
		}
	}
	first := f.firstSeq
	if first == 0 && len(f.events) > 0 {
		first = f.events[0].Seq
	}
	page := api.EventsPage{Events: out, FirstSeq: first, NextAfter: after}
	if len(out) > 0 {
		page.NextAfter = out[len(out)-1].Seq
	}
	return page, nil
}

// recorder runs nothing: it keeps each argv it was asked to apply and fails
// the call at failAt (1-based), if set.
type recorder struct {
	calls  [][]string
	failAt int
	err    error
}

func (r *recorder) apply(argv []string) error {
	r.calls = append(r.calls, argv)
	if r.failAt == len(r.calls) {
		return r.err
	}
	return nil
}

func str(s string) *string { return &s }

func TestCommandMapsEachReplayableEventToItsBdCall(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		ev   api.Event
		want []string
	}{
		"created with an actor": {
			ev:   api.Event{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("Add x"), Actor: "town/crew/x"},
			want: []string{"create", "--id=gt-1", "--title=Add x", "--actor=town/crew/x"},
		},
		"created without an actor": {
			ev:   api.Event{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("Add x")},
			want: []string{"create", "--id=gt-1", "--title=Add x"},
		},
		"status changed": {
			ev:   api.Event{Seq: 2, Op: "status_changed", IssueID: "gt-1", NewValue: str("in_progress")},
			want: []string{"update", "gt-1", "--status=in_progress"},
		},
		"reopened": {
			ev:   api.Event{Seq: 3, Op: "reopened", IssueID: "gt-1", NewValue: str("open")},
			want: []string{"update", "gt-1", "--status=open"},
		},
		"label added": {
			ev:   api.Event{Seq: 4, Op: "label_added", IssueID: "gt-1", NewValue: str("gt:agent")},
			want: []string{"update", "gt-1", "--add-label=gt:agent"},
		},
		"label removed": {
			ev:   api.Event{Seq: 5, Op: "label_removed", IssueID: "gt-1", OldValue: str("gt:agent")},
			want: []string{"update", "gt-1", "--remove-label=gt:agent"},
		},
		"closed with a reason": {
			ev:   api.Event{Seq: 6, Op: "closed", IssueID: "gt-1", Comment: str("done")},
			want: []string{"close", "gt-1", "--reason=done"},
		},
		"closed without a reason": {
			ev:   api.Event{Seq: 7, Op: "closed", IssueID: "gt-1"},
			want: []string{"close", "gt-1"},
		},
		"deleted": {
			ev:   api.Event{Seq: 8, Op: "deleted", IssueID: "gt-1"},
			want: []string{"delete", "gt-1", "--force"},
		},
		"dependency added": {
			ev:   api.Event{Seq: 9, Op: "dependency_added", IssueID: "gt-1", NewValue: str("gt-2"), Comment: str("blocks")},
			want: []string{"dep", "add", "gt-1", "gt-2", "--type=blocks"},
		},
		"dependency removed": {
			ev:   api.Event{Seq: 10, Op: "dependency_removed", IssueID: "gt-1", OldValue: str("gt-2"), Comment: str("blocks")},
			want: []string{"dep", "remove", "gt-1", "gt-2"},
		},
		"title updated": {
			ev:   api.Event{Seq: 11, Op: "updated", IssueID: "gt-1", NewValue: str("New title"), Comment: str("title")},
			want: []string{"update", "gt-1", "--title=New title"},
		},
		"priority updated": {
			ev:   api.Event{Seq: 12, Op: "updated", IssueID: "gt-1", NewValue: str("1"), Comment: str("priority")},
			want: []string{"update", "gt-1", "--priority=1"},
		},
		"assignee cleared": {
			ev:   api.Event{Seq: 13, Op: "updated", IssueID: "gt-1", NewValue: str(""), Comment: str("assignee")},
			want: []string{"update", "gt-1", "--assignee="},
		},
		"commented": {
			ev:   api.Event{Seq: 14, Op: "commented", IssueID: "gt-1", NewValue: str("working on it")},
			want: []string{"comment", "gt-1", "--", "working on it"},
		},
		"comment that opens with a dash": {
			ev:   api.Event{Seq: 15, Op: "commented", IssueID: "gt-1", NewValue: str("-x is not a flag")},
			want: []string{"comment", "gt-1", "--", "-x is not a flag"},
		},
		"description updated": {
			ev:   api.Event{Seq: 16, Op: "updated", IssueID: "gt-1", NewValue: str("new body"), Comment: str("description")},
			want: []string{"update", "gt-1", "--description=new body"},
		},
		"description cleared": {
			ev:   api.Event{Seq: 17, Op: "updated", IssueID: "gt-1", NewValue: str(""), Comment: str("description")},
			want: []string{"update", "gt-1", "--description="},
		},
		"acceptance updated": {
			ev:   api.Event{Seq: 18, Op: "updated", IssueID: "gt-1", NewValue: str("[ ] a box"), Comment: str("acceptance_criteria")},
			want: []string{"update", "gt-1", "--acceptance=[ ] a box"},
		},
		"notes appended": {
			ev:   api.Event{Seq: 19, Op: "updated", IssueID: "gt-1", NewValue: str("a line"), Comment: str("notes")},
			want: []string{"update", "gt-1", "--append-notes=a line"},
		},
		"promoted to persistent": {
			ev:   api.Event{Seq: 20, Op: "updated", IssueID: "gt-1", Comment: str("promoted to persistent")},
			want: []string{"update", "gt-1", "--persistent"},
		},
	}
	for name, tc := range cases {
		got, err := Command(tc.ev)
		if err != nil {
			t.Errorf("%s: Command = %v, want %v", name, err, tc.want)
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: Command = %q, want %q", name, got, tc.want)
		}
	}
}

func TestCommandRefusesWhatTheFeedCannotReplayAndNamesKindAndSeq(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		ev   api.Event
		kind string
	}{
		"comment text is not in the feed": {
			ev:   api.Event{Seq: 60, Op: "commented", IssueID: "gt-1"},
			kind: "commented",
		},
		"description update has no new value": {
			ev:   api.Event{Seq: 61, Op: "updated", IssueID: "gt-1", Comment: str("description")},
			kind: "updated/description",
		},
		"acceptance update has no new value": {
			ev:   api.Event{Seq: 62, Op: "updated", IssueID: "gt-1", Comment: str("acceptance_criteria")},
			kind: "updated/acceptance_criteria",
		},
		"notes update has no new value": {
			ev:   api.Event{Seq: 63, Op: "updated", IssueID: "gt-1", Comment: str("notes")},
			kind: "updated/notes",
		},
		"deferral update carries no replayable value": {
			ev:   api.Event{Seq: 64, Op: "updated", IssueID: "gt-1", Comment: str("defer_until")},
			kind: "updated/defer_until",
		},
		"priority outside 0-4": {
			ev:   api.Event{Seq: 23, Op: "updated", IssueID: "gt-1", NewValue: str("9"), Comment: str("priority")},
			kind: "updated/priority",
		},
		"unknown event kind": {
			ev:   api.Event{Seq: 24, Op: "frobnicated", IssueID: "gt-1"},
			kind: "frobnicated",
		},
		"dependency without its type": {
			ev:   api.Event{Seq: 25, Op: "dependency_added", IssueID: "gt-1", NewValue: str("gt-2")},
			kind: "dependency_added",
		},
		"creation without a title": {
			ev:   api.Event{Seq: 26, Op: "created", IssueID: "gt-1"},
			kind: "created",
		},
		"status change without the new status": {
			ev:   api.Event{Seq: 27, Op: "status_changed", IssueID: "gt-1"},
			kind: "status_changed",
		},
		"label added without its name": {
			ev:   api.Event{Seq: 28, Op: "label_added", IssueID: "gt-1"},
			kind: "label_added",
		},
		"label removed without its name": {
			ev:   api.Event{Seq: 29, Op: "label_removed", IssueID: "gt-1"},
			kind: "label_removed",
		},
		"dependency added without its target": {
			ev:   api.Event{Seq: 30, Op: "dependency_added", IssueID: "gt-1", Comment: str("blocks")},
			kind: "dependency_added",
		},
		"dependency removed without its target": {
			ev:   api.Event{Seq: 31, Op: "dependency_removed", IssueID: "gt-1"},
			kind: "dependency_removed",
		},
		"issue id that bd would read as a flag": {
			ev:   api.Event{Seq: 32, Op: "deleted", IssueID: "--force"},
			kind: "deleted",
		},
		"issue id holding a space": {
			ev:   api.Event{Seq: 33, Op: "status_changed", IssueID: "gt 1", NewValue: str("open")},
			kind: "status_changed",
		},
		"dependency target that bd would read as a flag": {
			ev:   api.Event{Seq: 34, Op: "dependency_added", IssueID: "gt-1", NewValue: str("-x"), Comment: str("blocks")},
			kind: "dependency_added",
		},
	}
	for name, tc := range cases {
		argv, err := Command(tc.ev)
		var u *UnreplayableError
		if !errors.As(err, &u) {
			t.Errorf("%s: Command = %q, %v; want *UnreplayableError", name, argv, err)
			continue
		}
		if u.Seq != tc.ev.Seq || u.Kind != tc.kind {
			t.Errorf("%s: refused seq %d kind %q, want seq %d kind %q", name, u.Seq, u.Kind, tc.ev.Seq, tc.kind)
		}
	}
}

// failingSource is a feed that cannot be read.
type failingSource struct{ err error }

func (f failingSource) Events(_ context.Context, _ int64, _ int) (api.EventsPage, error) {
	return api.EventsPage{}, f.err
}

func TestReplayReturnsAFeedReadFailureWithoutRunningAnything(t *testing.T) {
	t.Parallel()
	readErr := errors.New("tracker unreachable")
	run := &recorder{}
	res, err := Replay(context.Background(), failingSource{err: readErr}, run.apply, 3, "gt-", false, nil)
	if !errors.Is(err, readErr) || res != (Result{Applied: 3}) || len(run.calls) != 0 {
		t.Errorf("Replay over an unreadable feed = %+v, %v with %d calls; want the read error, Applied 3 and no calls", res, err, len(run.calls))
	}
}

func TestReplayDryRunPrintsEachBdCallAndRunsNone(t *testing.T) {
	t.Parallel()
	src := &fakeSource{pageSize: 1, events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("Title with space")},
		{Seq: 2, Op: "status_changed", IssueID: "gt-1", NewValue: str("in_progress")},
	}}
	var out bytes.Buffer
	res, err := Replay(context.Background(), src, nil, 0, "gt-", true, &out)
	if err != nil {
		t.Fatalf("Replay dry run: %v", err)
	}
	want := "bd create --id=gt-1 '--title=Title with space'\nbd update gt-1 --status=in_progress\n"
	if out.String() != want {
		t.Errorf("dry run printed %q, want %q", out.String(), want)
	}
	if res != (Result{Applied: 2, Commands: 2, Defaulted: 1}) {
		t.Errorf("dry run result = %+v, want Applied 2 Commands 2 Defaulted 1 (the one creation)", res)
	}
}

func TestReplayStopsAtTheFirstFailingCommandAndKeepsItsErrorOutOfTheMessage(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("a")},
		{Seq: 2, Op: "status_changed", IssueID: "gt-1", NewValue: str("open")},
		{Seq: 3, Op: "closed", IssueID: "gt-1"},
	}}
	run := &recorder{failAt: 2, err: errors.New("database locked: token sk-secret-value")}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)

	var ae *ApplyError
	if !errors.As(err, &ae) || ae.Seq != 2 || ae.Verb != "update" {
		t.Fatalf("Replay error = %v, want *ApplyError at seq 2 verb update", err)
	}
	if strings.Contains(err.Error(), "sk-secret") {
		t.Errorf("error message carries the runner's output: %q", err.Error())
	}
	if res != (Result{Applied: 1, Commands: 1, Defaulted: 1}) {
		t.Errorf("result = %+v, want Applied 1 (the last seq that ran) Commands 1 Defaulted 1", res)
	}
	if len(run.calls) != 2 {
		t.Errorf("runner was called %d times, want 2: the close after the failure must not run", len(run.calls))
	}
}

func TestReplayRefusesAnUnknownKindAfterRunningTheCommandsBeforeIt(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("a")},
		{Seq: 2, Op: "frobnicated", IssueID: "gt-1"},
		{Seq: 3, Op: "created", IssueID: "gt-2", NewValue: str("b")},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)

	var u *UnreplayableError
	if !errors.As(err, &u) || u.Seq != 2 || u.Kind != "frobnicated" {
		t.Fatalf("Replay error = %v, want unreplayable frobnicated at seq 2", err)
	}
	if res.Applied != 1 || len(run.calls) != 1 {
		t.Errorf("result %+v with %d calls, want seq 1 applied and nothing after the refusal", res, len(run.calls))
	}
}

func TestReplayResumesAfterSinceAndAppliesTheRest(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "deleted", IssueID: "gt-0"},
		{Seq: 2, Op: "deleted", IssueID: "gt-1"},
		{Seq: 3, Op: "deleted", IssueID: "gt-2"},
		{Seq: 4, Op: "deleted", IssueID: "gt-3"},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 2, "gt-", false, nil)
	if err != nil {
		t.Fatalf("Replay from 2: %v", err)
	}
	want := [][]string{{"delete", "gt-2", "--force"}, {"delete", "gt-3", "--force"}}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("applied %q, want %q: a rerun from the last applied seq starts after it", run.calls, want)
	}
	if res.Applied != 4 {
		t.Errorf("Applied = %d, want 4", res.Applied)
	}
}

// The feed carries every rig on the tracker; a replay applies only its own
// rig's events, in seq order, and counts the rest as skipped (gt-0n2s6).
func TestReplaySkipsEventsForOtherRigsAndCountsThem(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("a")},
		{Seq: 2, Op: "created", IssueID: "dv-1", NewValue: str("b")},
		{Seq: 3, Op: "status_changed", IssueID: "gt-1", NewValue: str("in_progress")},
		{Seq: 4, Op: "closed", IssueID: "dv-1"},
		{Seq: 5, Op: "closed", IssueID: "gt-2"},
		{Seq: 6, Op: "deleted", IssueID: "dv-2"},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	if err != nil {
		t.Fatalf("Replay over a feed of two rigs = %v, want no error", err)
	}
	want := [][]string{
		{"create", "--id=gt-1", "--title=a"},
		{"update", "gt-1", "--status=in_progress"},
		{"close", "gt-2"},
	}
	if !reflect.DeepEqual(run.calls, want) {
		t.Errorf("applied %q, want only the gt- events in seq order: %q", run.calls, want)
	}
	if res != (Result{Applied: 5, Commands: 3, Defaulted: 1, Skipped: 3}) {
		t.Errorf("result = %+v, want the last applied seq 5, 3 commands, 1 defaulted creation and 3 skipped; the dv- creation takes no defaulted count", res)
	}
}

// A foreign event of a kind Command refuses is skipped, not replayed and not a
// stop: the run pages past it and applies the rig's own event that follows.
func TestReplaySkipsAForeignEventItCouldNotReplay(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{
		{Events: []api.Event{
			{Seq: 1, Op: "frobnicated", IssueID: "dv-1"},
			{Seq: 2, Op: "deleted", IssueID: "--force"},
		}, FirstSeq: 1, NextAfter: 2},
		{Events: []api.Event{{Seq: 3, Op: "closed", IssueID: "gt-1"}}, FirstSeq: 1, NextAfter: 3},
		{FirstSeq: 1, NextAfter: 3},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	if err != nil {
		t.Fatalf("Replay over a foreign unreplayable kind and a foreign flag-like id = %v, want both skipped", err)
	}
	if want := [][]string{{"close", "gt-1"}}; !reflect.DeepEqual(run.calls, want) {
		t.Errorf("applied %q, want %q: the foreign events must not reach Command", run.calls, want)
	}
	if res != (Result{Applied: 3, Commands: 1, Skipped: 2}) {
		t.Errorf("result = %+v, want seq 3 applied and 2 skipped", res)
	}
	if want := []int64{0, 2, 3}; !reflect.DeepEqual(src.asked, want) {
		t.Errorf("pages were asked for after %v, want %v: a skipped event still moves the cursor", src.asked, want)
	}
}

// An event with no issue id names no rig, so the run stops on it rather than
// passing it over as another rig's; the cursor is not moved past it.
func TestReplayRefusesAnEventWithNoIssueIdInsteadOfSkippingIt(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "closed", IssueID: "gt-1"},
		{Seq: 2, Op: "closed", IssueID: ""},
		{Seq: 3, Op: "closed", IssueID: "gt-3"},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	var u *UnreplayableError
	if !errors.As(err, &u) || u.Seq != 2 || u.Kind != "closed" {
		t.Fatalf("Replay over an event with no issue id = %v, want unreplayable closed at seq 2", err)
	}
	if res != (Result{Applied: 1, Commands: 1}) || len(run.calls) != 1 {
		t.Errorf("result %+v with %d calls, want seq 1 applied and nothing after the id-less event", res, len(run.calls))
	}
}

// A dry run of a shared feed prints only the rig's own bd calls.
func TestReplayDryRunPrintsNoCallForAnotherRigsEvent(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("a")},
		{Seq: 2, Op: "created", IssueID: "dv-1", NewValue: str("b")},
	}}
	var out bytes.Buffer
	res, err := Replay(context.Background(), src, nil, 0, "gt-", true, &out)
	if err != nil {
		t.Fatalf("Replay dry run: %v", err)
	}
	if want := "bd create --id=gt-1 --title=a\n"; out.String() != want {
		t.Errorf("dry run printed %q, want %q", out.String(), want)
	}
	if res != (Result{Applied: 1, Commands: 1, Defaulted: 1, Skipped: 1}) {
		t.Errorf("result = %+v, want 1 command on the last own seq and 1 skipped", res)
	}
}

// A replay with no prefix could not tell its rig's events from the others, so
// it refuses before reading the feed rather than applying another rig's writes.
func TestReplayRefusesAnEmptyPrefix(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{{Seq: 1, Op: "closed", IssueID: "gt-1"}}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 3, "-", false, nil)
	if err == nil || !strings.Contains(err.Error(), "beads prefix") {
		t.Fatalf("Replay with no prefix = %v, want a refusal naming the missing prefix", err)
	}
	if len(run.calls) != 0 || res != (Result{Applied: 3}) {
		t.Errorf("result %+v with %d calls, want nothing applied", res, len(run.calls))
	}
}

func TestReplayRefusesAFeedPrunedPastSince(t *testing.T) {
	t.Parallel()
	src := &fakeSource{firstSeq: 5, events: []api.Event{
		{Seq: 5, Op: "deleted", IssueID: "gt-5"},
	}}
	run := &recorder{}
	_, err := Replay(context.Background(), src, run.apply, 2, "gt-", false, nil)
	var tr *TruncatedError
	if !errors.As(err, &tr) || tr.Since != 2 || tr.Floor != 5 {
		t.Fatalf("Replay from 2 with oldest retained 5 = %v, want *TruncatedError{Since:2, Floor:5}", err)
	}
	if len(run.calls) != 0 {
		t.Errorf("runner was called %d times on a pruned feed, want 0", len(run.calls))
	}
	if _, err := Replay(context.Background(), src, run.apply, 4, "gt-", false, nil); err != nil {
		t.Errorf("Replay from 4 with oldest retained 5 = %v, want no error: nothing is missing", err)
	}
}

// pageSource serves the pages it was given in order, whatever cursor it is
// asked for, so a test can script a feed that changes shape between reads.
type pageSource struct {
	pages []api.EventsPage
	asked []int64
}

func (p *pageSource) Events(_ context.Context, after int64, _ int) (api.EventsPage, error) {
	p.asked = append(p.asked, after)
	if len(p.asked) > len(p.pages) {
		return api.EventsPage{}, nil
	}
	return p.pages[len(p.asked)-1], nil
}

func TestReplayRefusesAPrunedFeedWhoseFirstPageIsEmpty(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{{FirstSeq: 5}}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 2, "gt-", false, nil)
	var tr *TruncatedError
	if !errors.As(err, &tr) || tr.Since != 2 || tr.Floor != 5 {
		t.Fatalf("Replay from 2 over an empty page with oldest retained 5 = %v, want *TruncatedError{Since:2, Floor:5}", err)
	}
	if len(run.calls) != 0 || res != (Result{Applied: 2}) {
		t.Errorf("result %+v with %d calls, want nothing applied: events 3 and 4 were pruned", res, len(run.calls))
	}
}

func TestReplayRefusesAFeedPrunedWhileItReads(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{
		{Events: []api.Event{{Seq: 2, Op: "deleted", IssueID: "gt-2"}}, FirstSeq: 1, NextAfter: 2},
		{FirstSeq: 6, NextAfter: 2},
	}}
	run := &recorder{}
	_, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	var tr *TruncatedError
	if !errors.As(err, &tr) || tr.Since != 2 || tr.Floor != 6 {
		t.Fatalf("Replay whose feed pruned past its cursor = %v, want *TruncatedError{Since:2, Floor:6}", err)
	}
	if len(run.calls) != 1 {
		t.Errorf("runner was called %d times, want 1: seq 2 ran before the gap was seen", len(run.calls))
	}
}

func TestReplayRefusesAnEmptyPageThatSaysMoreEventsFollow(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{
		{Events: []api.Event{{Seq: 1, Op: "deleted", IssueID: "gt-1"}}, FirstSeq: 1, NextAfter: 1},
		{NextAfter: 9, FirstSeq: 1},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	if err == nil || !strings.Contains(err.Error(), "empty page") || !strings.Contains(err.Error(), "9") {
		t.Fatalf("Replay over an empty page that points at seq 9 = %v, want a refusal naming it", err)
	}
	if res != (Result{Applied: 1, Commands: 1}) {
		t.Errorf("result = %+v, want seq 1 applied and the feed not read as ended", res)
	}
	if len(src.asked) != 2 || src.asked[1] != 1 {
		t.Errorf("pages were asked for after %v, want the second after 1: the cursor follows the last event applied", src.asked)
	}
}

func TestReplayEndsOnAnEmptyPageThatHoldsItsCursor(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{
		{Events: []api.Event{{Seq: 4, Op: "deleted", IssueID: "gt-4"}}, FirstSeq: 1, NextAfter: 4},
		{FirstSeq: 1, NextAfter: 4},
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	if err != nil {
		t.Fatalf("Replay over an empty page that holds its cursor = %v, want no error", err)
	}
	if res != (Result{Applied: 4, Commands: 1}) {
		t.Errorf("result = %+v, want seq 4 applied and the feed read as ended", res)
	}
}

func TestReplayRefusesAnEmptyPageThatDoesNotHoldItsCursor(t *testing.T) {
	t.Parallel()
	src := &pageSource{pages: []api.EventsPage{
		{Events: []api.Event{{Seq: 4, Op: "deleted", IssueID: "gt-4"}}, FirstSeq: 1, NextAfter: 4},
		{}, // neither a cursor nor a log: a feed that stalled or lost its place
	}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	if err == nil || !strings.Contains(err.Error(), "empty page") || !strings.Contains(err.Error(), "cursor") {
		t.Fatalf("Replay over an empty page holding no cursor = %v, want a refusal naming the stalled feed", err)
	}
	if res != (Result{Applied: 4, Commands: 1}) {
		t.Errorf("result = %+v, want seq 4 applied and the feed not read as ended", res)
	}
}

func TestReplayRefusesAnEmptyFeedThatHoldsNoEventsAtAll(t *testing.T) {
	t.Parallel()
	// The cursor is echoed, so the page looks like the end of the feed, but a
	// log with no events cannot account for the seqs a replay from 3 is past.
	src := &pageSource{pages: []api.EventsPage{{NextAfter: 3, FirstSeq: 0}}}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 3, "gt-", false, nil)
	if err == nil || !strings.Contains(err.Error(), "holding no events") {
		t.Fatalf("Replay from 3 over an empty feed = %v, want a refusal naming the missing log", err)
	}
	if len(run.calls) != 0 || res != (Result{Applied: 3}) {
		t.Errorf("result %+v with %d calls, want nothing applied", res, len(run.calls))
	}
}

// endlessSource hands out one new event per call, forever.
type endlessSource struct{}

func (endlessSource) Events(_ context.Context, after int64, _ int) (api.EventsPage, error) {
	next := after + 1
	return api.EventsPage{Events: []api.Event{{Seq: next, Op: "deleted", IssueID: "gt-x"}}, NextAfter: next}, nil
}

func TestReplayStopsAFeedThatNeverEnds(t *testing.T) {
	t.Parallel()
	run := &recorder{}
	res, err := Replay(context.Background(), endlessSource{}, run.apply, 0, "gt-", false, nil)
	if err == nil || !strings.Contains(err.Error(), "did not end") {
		t.Fatalf("Replay over an endless feed = %v, want a refusal naming the page bound", err)
	}
	if res.Applied != maxPages || len(run.calls) != maxPages {
		t.Errorf("result %+v with %d calls, want the run stopped after %d events", res, len(run.calls), maxPages)
	}
}

func TestReplayDryRunQuotesAValueTheShellWouldRewrite(t *testing.T) {
	t.Parallel()
	src := &fakeSource{events: []api.Event{
		{Seq: 1, Op: "created", IssueID: "gt-1", NewValue: str("line one\nline two")},
		{Seq: 2, Op: "created", IssueID: "gt-2", NewValue: str("café")},
		{Seq: 3, Op: "created", IssueID: "gt-3", NewValue: str("$(id) `id`; a|b&c >out")},
		{Seq: 4, Op: "created", IssueID: "gt-4", NewValue: str("it's a *glob*")},
		{Seq: 5, Op: "created", IssueID: "gt-5", NewValue: str("$HOME")},
		{Seq: 6, Op: "created", IssueID: "gt-6", NewValue: str("a|b&c;d")},
	}}
	var out bytes.Buffer
	if _, err := Replay(context.Background(), src, nil, 0, "gt-", true, &out); err != nil {
		t.Fatalf("Replay dry run: %v", err)
	}
	want := "bd create --id=gt-1 '--title=line one\nline two'\n" +
		"bd create --id=gt-2 '--title=café'\n" +
		"bd create --id=gt-3 '--title=$(id) `id`; a|b&c >out'\n" +
		"bd create --id=gt-4 '--title=it'\\''s a *glob*'\n" +
		"bd create --id=gt-5 '--title=$HOME'\n" +
		"bd create --id=gt-6 '--title=a|b&c;d'\n"
	if out.String() != want {
		t.Errorf("dry run printed %q, want %q", out.String(), want)
	}
}

func TestReplayReadsEveryPageOfTheFeed(t *testing.T) {
	t.Parallel()
	var evs []api.Event
	for i := int64(1); i <= 5; i++ {
		evs = append(evs, api.Event{Seq: i, Op: "deleted", IssueID: "gt-x"})
	}
	src := &fakeSource{pageSize: 2, events: evs}
	run := &recorder{}
	res, err := Replay(context.Background(), src, run.apply, 0, "gt-", false, nil)
	if err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if res != (Result{Applied: 5, Commands: 5}) || len(run.calls) != 5 {
		t.Errorf("result %+v with %d calls, want all 5 events applied across pages", res, len(run.calls))
	}
}

// unorderedSource hands out a seq that does not move the feed forward. It
// ends the feed after a few pages, so a replay that stops checking ordering
// fails the test instead of looping on seq 2 forever.
type unorderedSource struct{ calls int }

func (u *unorderedSource) Events(_ context.Context, _ int64, _ int) (api.EventsPage, error) {
	u.calls++
	switch {
	case u.calls == 1:
		return api.EventsPage{Events: []api.Event{{Seq: 3, Op: "deleted", IssueID: "gt-3"}}}, nil
	case u.calls <= 3:
		return api.EventsPage{Events: []api.Event{{Seq: 2, Op: "deleted", IssueID: "gt-2"}}}, nil
	default:
		return api.EventsPage{}, nil
	}
}

func TestReplayRefusesAFeedThatGoesBackwards(t *testing.T) {
	t.Parallel()
	run := &recorder{}
	_, err := Replay(context.Background(), &unorderedSource{}, run.apply, 0, "gt-", false, nil)
	if err == nil || !strings.Contains(err.Error(), "not ascending") {
		t.Fatalf("Replay over a backwards feed = %v, want a not-ascending refusal", err)
	}
	if len(run.calls) != 1 {
		t.Errorf("runner was called %d times, want 1: only the ordered event runs", len(run.calls))
	}
}
