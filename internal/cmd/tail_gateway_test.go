package cmd

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/bdgateway"
)

// gatewayAnswer is one scripted answer to a calls read.
type gatewayAnswer struct {
	page bdgateway.CallsPage
	err  error
}

// fakeGatewayReader answers each read with the next scripted answer and repeats
// the last one once the script is spent. Each read's since and limit are
// recorded so a test can check the cursor and the page size asked for.
type fakeGatewayReader struct {
	answers []gatewayAnswer
	sinces  []time.Time
	limits  []int
}

func (f *fakeGatewayReader) Calls(_ context.Context, since time.Time, limit int) (bdgateway.CallsPage, error) {
	f.sinces = append(f.sinces, since)
	f.limits = append(f.limits, limit)
	a := f.answers[0]
	if len(f.answers) > 1 {
		f.answers = f.answers[1:]
	}
	return a.page, a.err
}

// gatewayTestNow is the clock the source reads; the lines it stamps with now
// carry this time.
func gatewayTestNow() time.Time { return tailNow }

// gatewayWriteAndFailedRead is the call log of two calls: a write that
// succeeded and a read that exited non-zero. Its flags and error text must not
// reach a line.
func gatewayWriteAndFailedRead() bdgateway.CallsPage {
	return bdgateway.CallsPage{Entries: []bdgateway.CallEntry{
		{TS: "2026-09-30T13:50:00Z", Caller: "daemon", Verb: "update", Beads: []string{"gt-abc"}, Write: true, Flags: []string{"--status=open"}, DurationMS: 12},
		{TS: "2026-09-30T13:51:00Z", Caller: "witness", Verb: "show", Beads: []string{"gt-def"}, ExitCode: 1, DurationMS: 40, Error: "bd: no such bead"},
	}}
}

// gatewayReadsWriteAndFailedRead is three successful reads, a write and a read
// that exited non-zero. The summary counts the three; the write and the failed
// read each get a line.
func gatewayReadsWriteAndFailedRead() bdgateway.CallsPage {
	return bdgateway.CallsPage{Entries: []bdgateway.CallEntry{
		{TS: "2026-09-30T13:49:00Z", Caller: "witness", Verb: "show", Beads: []string{"gt-aaa"}, DurationMS: 5},
		{TS: "2026-09-30T13:49:30Z", Caller: "witness", Verb: "list", Flags: []string{"--json"}, DurationMS: 6},
		{TS: "2026-09-30T13:49:45Z", Caller: "mayor", Verb: "ready", DurationMS: 7},
		{TS: "2026-09-30T13:50:00Z", Caller: "daemon", Verb: "update", Beads: []string{"gt-abc"}, Write: true, Flags: []string{"--status=open"}, DurationMS: 12},
		{TS: "2026-09-30T13:51:00Z", Caller: "witness", Verb: "show", Beads: []string{"gt-def"}, ExitCode: 1, DurationMS: 40, Error: "bd: no such bead"},
	}}
}

// TestGatewaySourcePollReturnsWriteFailureAndSummary: the write gets its own
// line, the failed read gets a line in the failure class, and the summary counts
// the successful reads only.
func TestGatewaySourcePollReturnsWriteFailureAndSummary(t *testing.T) {
	t.Parallel()
	reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: gatewayReadsWriteAndFailedRead()}}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)

	lines := s.Poll()
	want := []string{"town gateway daemon update gt-abc", "town gateway witness show gt-def failed exit 1, 40ms", "town gateway 3 gateway reads"}
	if got := texts(lines); !reflect.DeepEqual(got, want) {
		t.Fatalf("lines = %q, want %q", got, want)
	}
	if lines[1].Class != tailClassFailure {
		t.Fatalf("failed call's class = %v, want tailClassFailure", lines[1].Class)
	}
	for _, l := range lines {
		if l.Rig != "town" || l.Kind != tailKindGateway {
			t.Fatalf("line %q is %s/%s, want town/gateway", l.Text, l.Rig, l.Kind)
		}
	}
}

// TestGatewaySourceSecondPollRepeatsNothing: the reader serves the same two
// calls again, and the second poll shows none of them. Its read starts at the
// newest stamp seen, so a reader that honors since reads only what is new.
func TestGatewaySourceSecondPollRepeatsNothing(t *testing.T) {
	t.Parallel()
	reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: gatewayWriteAndFailedRead()}}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)
	s.Poll()

	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("second poll = %q, want no lines", texts(got))
	}
	if len(reader.sinces) != 2 || !reader.sinces[1].Equal(at("2026-09-30T13:51:00Z")) {
		t.Fatalf("second read's since = %v, want the newest stamp 13:51", reader.sinces)
	}
}

// TestGatewaySourceKeepsTheCallsAtTheCursorStamp: a call stamped at the cursor
// that the reader serves again is not shown again, and a new call at that same
// stamp is shown.
func TestGatewaySourceKeepsTheCallsAtTheCursorStamp(t *testing.T) {
	t.Parallel()
	shown := gatewayWriteAndFailedRead()
	later := bdgateway.CallEntry{TS: "2026-09-30T13:51:00Z", Caller: "mayor", Verb: "close", Beads: []string{"gt-ghi"}, Write: true}
	reader := &fakeGatewayReader{answers: []gatewayAnswer{
		{page: shown},
		{page: bdgateway.CallsPage{Entries: append(shown.Entries, later)}},
	}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)
	s.Poll()

	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"town gateway mayor close gt-ghi"}) {
		t.Fatalf("second poll = %q, want only the new call", got)
	}
}

// TestGatewaySourceErrorsOnePerDistinctText: a read error prints once per
// distinct text and goes silent until a read succeeds, as tailOnce does for the
// other sources. A read that errors with a new text prints a second line.
func TestGatewaySourceErrorsOnePerDistinctText(t *testing.T) {
	t.Parallel()
	reader := &fakeGatewayReader{answers: []gatewayAnswer{
		{err: errors.New("gateway down")},
		{err: errors.New("gateway down")},
		{page: bdgateway.CallsPage{}},
		{err: errors.New("token refused")},
	}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)

	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"town gateway cannot read the gateway's calls: gateway down"}) {
		t.Fatalf("first error = %q", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("repeated error = %q, want silence", texts(got))
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("successful read = %q, want no lines", texts(got))
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"town gateway cannot read the gateway's calls: token refused"}) {
		t.Fatalf("new error after a success = %q", got)
	}
}

// TestGatewaySourceSummaryCountsOnlySuccessfulReads: a failed read is a line of
// its own, so a poll whose only read failed has no summary, and a poll with no
// calls has none either.
func TestGatewaySourceSummaryCountsOnlySuccessfulReads(t *testing.T) {
	t.Parallel()
	failedRead := bdgateway.CallEntry{TS: "2026-09-30T13:51:00Z", Caller: "witness", Verb: "show", Beads: []string{"gt-def"}, ExitCode: 1, DurationMS: 40}
	cases := []struct {
		name string
		page bdgateway.CallsPage
		want []string
	}{
		{name: "only a failed read", page: bdgateway.CallsPage{Entries: []bdgateway.CallEntry{failedRead}},
			want: []string{"town gateway witness show gt-def failed exit 1, 40ms"}},
		{name: "no calls", page: bdgateway.CallsPage{}, want: nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: c.page}}}
			s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)
			if got := texts(s.Poll()); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("lines = %q, want %q", got, c.want)
			}
		})
	}
}

// TestGatewaySourceUnreadableTimeDoesNotPinTheCursor: a call with no readable
// time is skipped and counted, the readable calls around it are shown, and the
// cursor moves past them, so the next poll shows nothing new and the count line
// is said once across the two polls.
func TestGatewaySourceUnreadableTimeDoesNotPinTheCursor(t *testing.T) {
	t.Parallel()
	page := bdgateway.CallsPage{Entries: []bdgateway.CallEntry{
		{TS: "not-a-time", Caller: "daemon", Verb: "update", Write: true},
		{TS: "2026-09-30T13:50:00Z", Caller: "daemon", Verb: "update", Beads: []string{"gt-abc"}, Write: true},
		{TS: "2026-09-30T13:51:00Z", Caller: "mayor", Verb: "close", Beads: []string{"gt-ghi"}, Write: true},
	}}
	reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: page}}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)

	want := []string{
		"town gateway daemon update gt-abc",
		"town gateway mayor close gt-ghi",
		"town gateway 1 calls had an unreadable time and were skipped",
	}
	first := texts(s.Poll())
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first poll = %q, want %q", first, want)
	}
	if got := texts(s.Poll()); len(got) != 0 {
		t.Fatalf("second poll = %q, want nothing new", got)
	}
	if !reader.sinces[1].Equal(at("2026-09-30T13:51:00Z")) {
		t.Fatalf("second read's since = %v, want the newest readable stamp 13:51", reader.sinces[1])
	}
}

// TestGatewaySourceUnreadableOnlyPageMovesCursorToThePollClock: a page whose
// calls all lack a readable time has no stamp to move the cursor to, so the
// cursor moves to this poll's clock.
func TestGatewaySourceUnreadableOnlyPageMovesCursorToThePollClock(t *testing.T) {
	t.Parallel()
	reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: bdgateway.CallsPage{Entries: []bdgateway.CallEntry{
		{TS: "not-a-time", Caller: "daemon", Verb: "update", Write: true},
	}}}}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)

	s.Poll()
	s.Poll()
	if !reader.sinces[1].Equal(tailNow) {
		t.Fatalf("second read's since = %v, want this poll's clock %v", reader.sinces[1], tailNow)
	}
}

// TestGatewaySourceFullPageSaysMoreCallsWait: a page at the limit keeps the
// oldest calls after the cursor, so the cursor moves to its last call, the
// rest come on the next poll, and the feed says so. The read asks for the
// gateway's maximum.
func TestGatewaySourceFullPageSaysMoreCallsWait(t *testing.T) {
	t.Parallel()
	base := at("2026-09-30T13:00:00Z")
	entries := make([]bdgateway.CallEntry, tailGatewayLimit)
	for i := range entries {
		entries[i] = bdgateway.CallEntry{
			TS:     base.Add(time.Duration(i+1) * time.Second).Format(time.RFC3339Nano),
			Caller: "daemon", Verb: "update", Beads: []string{"gt-abc"}, Write: true,
		}
	}
	reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: bdgateway.CallsPage{Entries: entries}}}}
	s := newTailGatewaySource(reader, base, gatewayTestNow)

	got := texts(s.Poll())
	if len(got) == 0 || got[len(got)-1] != "town gateway more calls waiting" {
		t.Fatalf("full page's lines do not end with the more-calls-waiting line: %d lines", len(got))
	}
	if reader.limits[0] != 500 {
		t.Fatalf("read limit = %d, want the gateway's maximum 500", reader.limits[0])
	}
	if !reader.sinces[0].Equal(base) {
		t.Fatalf("first read's since = %v, want the cutoff", reader.sinces[0])
	}
	s.Poll()
	if last := at(entries[len(entries)-1].TS); !reader.sinces[1].Equal(last) {
		t.Fatalf("second read's since = %v, want the page's last call %v", reader.sinces[1], last)
	}
}

// TestGatewaySourceDroppedCallsPrintTheGrowth: the answer's Dropped count is
// lines the gateway's writer lost. The first poll sets the baseline, a growth
// since the last poll prints its delta once, and no growth prints nothing.
func TestGatewaySourceDroppedCallsPrintTheGrowth(t *testing.T) {
	t.Parallel()
	reader := &fakeGatewayReader{answers: []gatewayAnswer{
		{page: bdgateway.CallsPage{Dropped: 0}},
		{page: bdgateway.CallsPage{Dropped: 7}},
		{page: bdgateway.CallsPage{Dropped: 7}},
	}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)

	if got := texts(s.Poll()); len(got) != 0 {
		t.Fatalf("first poll = %q, want no lines", got)
	}
	if got := texts(s.Poll()); !reflect.DeepEqual(got, []string{"town gateway 7 calls were not logged"}) {
		t.Fatalf("growth from 0 to 7 = %q, want one line", got)
	}
	if got := s.Poll(); len(got) != 0 {
		t.Fatalf("no growth = %q, want no lines", texts(got))
	}
}

// TestDashboardTailOptionsGatewaySourceFollowsTheReader: the feed adds the
// gateway source when a reader is set and leaves it out when none is.
func TestDashboardTailOptionsGatewaySourceFollowsTheReader(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	cutoff := at("2026-09-30T13:00:00Z")

	with := dashboardTailOptions(town, cutoff, tailTestLoc, dashboardTestRigs, nil, nil, &fakeGatewayReader{answers: []gatewayAnswer{{}}})
	if !with.kinds[tailKindGateway] {
		t.Fatal("the feed with a reader does not select the gateway kind")
	}
	sources, _, err := buildTailSources(with)
	if err != nil {
		t.Fatal(err)
	}
	if !hasGatewaySource(sources) {
		t.Fatal("buildTailSources built no gateway source for the feed with a reader")
	}

	without := dashboardTailOptions(town, cutoff, tailTestLoc, dashboardTestRigs, nil, nil, nil)
	if without.kinds[tailKindGateway] {
		t.Fatal("the feed with no reader selects the gateway kind")
	}
	sources, _, err = buildTailSources(without)
	if err != nil {
		t.Fatal(err)
	}
	if hasGatewaySource(sources) {
		t.Fatal("buildTailSources built a gateway source for the feed with no reader")
	}
}

// TestGatewayKindIsNotAGtTailKind: gt tail's --kind names only tailKinds, and
// the gateway kind is not one of them, so gt tail never shows the source.
func TestGatewayKindIsNotAGtTailKind(t *testing.T) {
	t.Parallel()
	if allTailKinds()[tailKindGateway] {
		t.Fatalf("gt tail's kinds include %q", tailKindGateway)
	}
}

// TestDashboardGatewayReaderNeedsURLAndToken: the feed reads the gateway only
// when both its URL and its token file are set.
func TestDashboardGatewayReaderNeedsURLAndToken(t *testing.T) {
	t.Parallel()
	env := func(vars map[string]string) func(string) string {
		return func(k string) string { return vars[k] }
	}
	cases := []struct {
		name string
		vars map[string]string
		want bool
	}{
		{name: "neither set", vars: map[string]string{}, want: false},
		{name: "URL only", vars: map[string]string{bdgateway.WorkspaceURLEnv: "http://gateway:8080"}, want: false},
		{name: "token only", vars: map[string]string{bdgateway.WorkspaceTokenEnv: "/tmp/token"}, want: false},
		{name: "URL and token", vars: map[string]string{
			bdgateway.WorkspaceURLEnv:   "http://gateway:8080",
			bdgateway.WorkspaceTokenEnv: "/tmp/token",
		}, want: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := dashboardGatewayReader(env(c.vars)) != nil; got != c.want {
				t.Fatalf("reader configured = %v, want %v", got, c.want)
			}
		})
	}
}

// hasGatewaySource reports whether sources holds a gateway source.
func hasGatewaySource(sources []tailSource) bool {
	for _, s := range sources {
		if _, ok := s.(*tailGatewaySource); ok {
			return true
		}
	}
	return false
}
