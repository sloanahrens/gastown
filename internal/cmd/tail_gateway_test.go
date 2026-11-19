package cmd

import (
	"context"
	"errors"
	"reflect"
	"strings"
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
// the last one once the script is spent. It ignores nothing: each read's since
// is recorded so a test can check the cursor.
type fakeGatewayReader struct {
	answers []gatewayAnswer
	sinces  []time.Time
}

func (f *fakeGatewayReader) Calls(_ context.Context, since time.Time, _ int) (bdgateway.CallsPage, error) {
	f.sinces = append(f.sinces, since)
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

// TestGatewaySourcePollReturnsWriteFailureAndSummary: the write gets its own
// line, the failed read gets a line in the failure class, and the poll's read
// count follows both.
func TestGatewaySourcePollReturnsWriteFailureAndSummary(t *testing.T) {
	t.Parallel()
	reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: gatewayWriteAndFailedRead()}}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)

	lines := s.Poll()
	want := []string{"town gateway daemon update gt-abc", "town gateway witness show gt-def failed exit 1, 40ms", "town gateway 2 gateway reads"}
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

// TestGatewaySourceUnreadableStampIsAnError: a call whose stamp cannot be read
// is a read failure, not a line shown or a call skipped without a word, and the
// cursor does not move past it.
func TestGatewaySourceUnreadableStampIsAnError(t *testing.T) {
	t.Parallel()
	reader := &fakeGatewayReader{answers: []gatewayAnswer{{page: bdgateway.CallsPage{Entries: []bdgateway.CallEntry{
		{TS: "not-a-time", Caller: "daemon", Verb: "update", Write: true},
	}}}}}
	s := newTailGatewaySource(reader, at("2026-09-30T13:00:00Z"), gatewayTestNow)

	got := texts(s.Poll())
	if len(got) != 1 || !strings.HasPrefix(got[0], "town gateway cannot read the gateway's calls: call 0 has no readable time") {
		t.Fatalf("unreadable stamp = %q, want one read error", got)
	}
	if !reader.sinces[0].Equal(at("2026-09-30T13:00:00Z")) {
		t.Fatalf("cursor moved on an unreadable stamp: since %v", reader.sinces[0])
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
