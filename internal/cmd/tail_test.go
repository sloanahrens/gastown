package cmd

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

var tailTestLoc = time.FixedZone("CDT", -5*3600)

func at(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestMergeTail_OrdersByTimeStably(t *testing.T) {
	t.Parallel()
	events := []tailLine{
		{At: at("2026-09-30T14:00:02Z"), Rig: "gastown", Kind: "events", Text: "e2"},
		{At: at("2026-09-30T14:00:00Z"), Rig: "gastown", Kind: "events", Text: "e0"},
	}
	daemon := []tailLine{
		{At: at("2026-09-30T14:00:00Z"), Rig: "town", Kind: "daemon", Text: "d0"},
		{At: at("2026-09-30T14:00:01Z"), Rig: "town", Kind: "daemon", Text: "d1"},
	}
	var got []string
	for _, l := range mergeTail(events, daemon) {
		got = append(got, l.Text)
	}
	if want := []string{"e0", "d0", "d1", "e2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("merge = %v, want %v", got, want)
	}
}

func TestRenderLine_FieldsAndControlCharacters(t *testing.T) {
	t.Parallel()
	full := tailView{Loc: tailTestLoc, FullSource: true}
	l := tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "gastown", Kind: "events", Text: "close gt-1\nforged line\tx\x1b[31m"}
	got := full.renderLine(l)
	want := "2026-09-30T09:05:06-05:00 gastown events close gt-1 forged line x [31m"
	if got != want {
		t.Fatalf("render =\n%q\nwant\n%q", got, want)
	}
	if got := full.renderLine(tailLine{At: at("2026-09-30T14:05:06Z"), Text: "x"}); got != "2026-09-30T09:05:06-05:00 - - x" {
		t.Fatalf("empty rig/kind render = %q", got)
	}
	if got := full.renderLine(tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "a b", Kind: "events", Text: "x"}); got != "2026-09-30T09:05:06-05:00 a_b events x" {
		t.Fatalf("spaced rig render = %q", got)
	}
}

// TestRenderLine_SourceTagIsShort: the default line carries the rig as one
// short tag ("town" for the daemon) and no kind word, so a typical line does
// not wrap; --verbose keeps the <rig> <kind> columns.
func TestRenderLine_SourceTagIsShort(t *testing.T) {
	t.Parallel()
	short := tailView{Loc: tailTestLoc}
	long := tailView{Loc: tailTestLoc, FullSource: true}
	cases := []struct {
		line        tailLine
		want        string
		wantVerbose string
	}{
		{
			tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "town", Kind: tailKindDaemon, Text: "Heartbeat complete (#53)"},
			"2026-09-30T09:05:06-05:00 town Heartbeat complete (#53)",
			"2026-09-30T09:05:06-05:00 town daemon Heartbeat complete (#53)",
		},
		{
			tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "gastown", Kind: tailKindLandings, Text: "landed gt-1"},
			"2026-09-30T09:05:06-05:00 gastown landed gt-1",
			"2026-09-30T09:05:06-05:00 gastown landings landed gt-1",
		},
	}
	for _, c := range cases {
		if got := short.renderLine(c.line); got != c.want {
			t.Errorf("default render = %q, want %q", got, c.want)
		}
		if got := long.renderLine(c.line); got != c.wantVerbose {
			t.Errorf("--verbose render = %q, want %q", got, c.wantVerbose)
		}
	}
}

func TestParseTailSince(t *testing.T) {
	t.Parallel()
	now := at("2026-09-30T14:00:00Z")
	cases := map[string]time.Time{
		"15m":                       now.Add(-15 * time.Minute),
		"2h":                        now.Add(-2 * time.Hour),
		"1d":                        now.Add(-24 * time.Hour),
		"0":                         now,
		"2026-09-30T10:00:00Z":      at("2026-09-30T10:00:00Z"),
		"2026-09-30T08:00:00-05:00": at("2026-09-30T13:00:00Z"),
		"2026-09-30T08:00:00":       at("2026-09-30T13:00:00Z"),
		"2026-09-30 08:00":          at("2026-09-30T13:00:00Z"),
		"2026-09-30":                at("2026-09-30T05:00:00Z"),
	}
	for in, want := range cases {
		got, err := parseTailSince(in, now, tailTestLoc)
		if err != nil || !got.Equal(want) {
			t.Errorf("parseTailSince(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "yesterday", "-5m", "5x", "1.5d", "99999999999d", "36501d"} {
		if _, err := parseTailSince(bad, now, tailTestLoc); err == nil {
			t.Errorf("parseTailSince(%q) accepted", bad)
		}
	}
}

func TestParseTailKinds(t *testing.T) {
	t.Parallel()
	got, err := parseTailKinds("events, daemon")
	if err != nil || !reflect.DeepEqual(got, map[string]bool{"events": true, "daemon": true}) {
		t.Fatalf("kinds = %v, %v", got, err)
	}
	for _, bad := range []string{"", "events,bogus", ","} {
		if _, err := parseTailKinds(bad); err == nil {
			t.Errorf("parseTailKinds(%q) accepted", bad)
		}
	}
}

// TestRenderLine_TitleVerdictAndWidth: a bead line names its bead, a verdict
// line says what the loop decided, and neither pushes the line past the
// terminal it is printed on.
func TestTailRenderLine_TitleVerdictAndWidth(t *testing.T) {
	t.Parallel()
	line := tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "gastown", Kind: tailKindEvents, Text: "close gt-1 status=closed"}
	cases := []struct {
		name string
		line tailLine
		view tailView
		want string
	}{
		{"title", tailLine{At: line.At, Rig: line.Rig, Kind: line.Kind, Text: line.Text, Title: "one line per thing that happened"},
			tailView{Loc: tailTestLoc, Layout: tailClockLayout},
			"09:05:06 gastown close gt-1 status=closed · one line per thing that happened"},
		{"verdict", tailLine{At: line.At, Rig: line.Rig, Kind: line.Kind, Text: "update gt-1 status=open",
			Title: "a title", Verdict: "OVERSEER REVIEW aaaa PASS: reads well"},
			tailView{Loc: tailTestLoc, Layout: tailClockLayout},
			"09:05:06 gastown update gt-1 status=open · OVERSEER REVIEW aaaa PASS: reads well"},
		{"trimmed fields carry the title", tailLine{At: line.At, Rig: line.Rig, Kind: line.Kind,
			Text: "close gt-1 status=closed actor=Sloan Ahrens seq=4", Title: "a title"},
			tailView{Loc: tailTestLoc, Layout: tailClockLayout, Trim: true, GitUser: "Sloan Ahrens"},
			"09:05:06 gastown close gt-1 status=closed · a title"},
		{"verbose keeps the raw line", tailLine{At: line.At, Rig: line.Rig, Kind: line.Kind, Text: "close gt-1 status=closed actor=Sloan Ahrens seq=4"},
			tailView{Loc: tailTestLoc, Layout: tailClockLayout, FullSource: true},
			"09:05:06 gastown events close gt-1 status=closed actor=Sloan Ahrens seq=4"},
	}
	for _, c := range cases {
		if got := c.view.renderLine(c.line); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}

	// A bound cuts the title, never the line's own words.
	long := "a title long enough to run past a narrow terminal, and then some more"
	bounded := tailView{Loc: tailTestLoc, Layout: tailClockLayout, Width: 60}
	got := bounded.renderLine(tailLine{At: line.At, Rig: line.Rig, Kind: line.Kind, Text: line.Text, Title: long})
	if !strings.HasPrefix(got, "09:05:06 gastown close gt-1 status=closed · ") {
		t.Errorf("the bound ate the line: %q", got)
	}
	if w := len([]rune(got)); w > 60 {
		t.Errorf("line is %d runes wide, bound is 60: %q", w, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("a cut title must show it was cut: %q", got)
	}
	// Too narrow for the annotation at all: the line loses it, not its text.
	tight := tailView{Loc: tailTestLoc, Layout: tailClockLayout, Width: 20}
	if got := tight.renderLine(tailLine{At: line.At, Rig: line.Rig, Kind: line.Kind, Text: line.Text, Title: long}); got != "09:05:06 gastown close gt-1 status=closed" {
		t.Errorf("a line with no room for a title: %q", got)
	}
}

// TestTailSummaryFieldsLine: the line names each field it could read and
// leaves out each one it could not.
func TestTailSummaryFieldsLine(t *testing.T) {
	t.Parallel()
	full := tailSummaryFields{
		HasSeats: true, SeatsUsed: 3, SeatsCap: 6,
		Seats:       []string{"gastown/malachite:gt-ufjol", "gastown/opal:gt-hk555"},
		HasReady:    true,
		ReadyToLand: 2,
		HasMain:     true, MainTip: "6a4fe5376de8", InstalledGT: "433697dd", Behind: 2,
		HasEscalate: true, Escalations: 1,
		HasSpend: true, SpendPerHour: 1.066,
	}
	want := "📊 seats 3/6 [gastown/malachite:gt-ufjol, gastown/opal:gt-hk555] · ready 2 · " +
		"main 6a4fe5376de8 vs gt 433697dd (+2 behind) · escalations 1 · DeepSeek $1.07/h"
	if got := full.line(); got != want {
		t.Fatalf("line = %q\nwant %q", got, want)
	}
	if got := (tailSummaryFields{}).line(); got != "" {
		t.Errorf("a summary that read nothing prints %q", got)
	}
	// A town the scheduler leaves uncapped reports the seats, not a cap of
	// zero or of the scheduler's -1 for "unset".
	uncapped := tailSummaryFields{HasSeats: true, SeatsUsed: 3, SeatsCap: 0}
	if got := uncapped.line(); got != "📊 seats 3" {
		t.Errorf("uncapped line = %q", got)
	}

	// More pairs than the line names are counted.
	many := tailSummaryFields{HasSeats: true, SeatsCap: 9}
	for i := 0; i < tailSummaryMaxSeats+2; i++ {
		many.Seats = append(many.Seats, string(rune('a'+i))+":gt-x")
	}
	got := many.line()
	if !strings.Contains(got, ", +2 more]") {
		t.Errorf("the seats past the bound must be counted: %q", got)
	}
}

// TestTailSummaryTracker_PrintsWhenAFieldMovesAndNotMoreOften: the summary is
// a state line, so it prints once, again when a field moves, and never twice
// inside the window.
func TestTailSummaryTracker_PrintsWhenAFieldMovesAndNotMoreOften(t *testing.T) {
	t.Parallel()
	now := at("2026-09-30T14:00:00Z")
	fields := tailSummaryFields{HasSeats: true, SeatsUsed: 1, SeatsCap: 6}
	reads := 0
	tracker := &tailSummaryTracker{
		read:     func() tailSummaryFields { reads++; return fields },
		now:      func() time.Time { return now },
		interval: tailSummaryFresh,
	}
	line, ok := tracker.next()
	if !ok || line.Text != "📊 seats 1/6" || line.Rig != "town" {
		t.Fatalf("first poll = %+v, %v", line, ok)
	}
	if _, ok := tracker.next(); ok {
		t.Error("an unchanged summary must not print again")
	}
	// The state read is the expensive half: inside the window the tracker
	// must not make one, or gt tail -f would pay for a town scan every poll.
	if reads != 1 {
		t.Errorf("the town was read %d times inside the window; want 1", reads)
	}
	now = now.Add(time.Minute)
	fields.SeatsUsed = 2
	if _, ok := tracker.next(); ok {
		t.Error("a change inside the window waits for the window")
	}
	now = now.Add(tailSummaryFresh)
	line, ok = tracker.next()
	if !ok || line.Text != "📊 seats 2/6" {
		t.Fatalf("the change after the window = %+v, %v", line, ok)
	}
	if _, ok := tracker.next(); ok {
		t.Error("the new state must not print twice")
	}
	// A summary that read nothing prints nothing — never an empty line.
	fields = tailSummaryFields{}
	now = now.Add(tailSummaryFresh)
	if _, ok := tracker.next(); ok {
		t.Error("a summary with no fields must not print")
	}
	// A tracker with no reader is what a non-following run gets.
	if _, ok := (&tailSummaryTracker{}).next(); ok {
		t.Error("a tracker with no reader must print nothing")
	}
}

// TestTailSummaryTracker_KeepsTheLineInsideTheTerminal: the seats list is the
// one field that can make the summary line longer than the terminal.
func TestTailSummaryTracker_KeepsTheLineInsideTheTerminal(t *testing.T) {
	t.Parallel()
	fields := tailSummaryFields{HasSeats: true, SeatsUsed: 8, SeatsCap: 8, HasReady: true, ReadyToLand: 3}
	for i := 0; i < 8; i++ {
		fields.Seats = append(fields.Seats, "gastown/polecats/malachite"+string(rune('a'+i))+":gt-ufjol")
	}
	tracker := &tailSummaryTracker{
		read:     func() tailSummaryFields { return fields },
		now:      func() time.Time { return at("2026-09-30T14:00:00Z") },
		interval: tailSummaryFresh,
		width:    80,
	}
	line, ok := tracker.next()
	if !ok {
		t.Fatal("the first summary must print")
	}
	if w := len([]rune(line.Text)); w > 80 {
		t.Fatalf("summary is %d runes wide: %q", w, line.Text)
	}
}
