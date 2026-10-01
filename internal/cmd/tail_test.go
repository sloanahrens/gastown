package cmd

import (
	"reflect"
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

func TestRenderTailLine(t *testing.T) {
	t.Parallel()
	l := tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "gastown", Kind: "events", Text: "close gt-1\nforged line\tx\x1b[31m"}
	got := renderTailLine(l, tailTestLoc)
	want := "2026-09-30T09:05:06-05:00 gastown events close gt-1 forged line x [31m"
	if got != want {
		t.Fatalf("render =\n%q\nwant\n%q", got, want)
	}
	if got := renderTailLine(tailLine{At: at("2026-09-30T14:05:06Z"), Text: "x"}, tailTestLoc); got != "2026-09-30T09:05:06-05:00 - - x" {
		t.Fatalf("empty rig/kind render = %q", got)
	}
	if got := renderTailLine(tailLine{At: at("2026-09-30T14:05:06Z"), Rig: "a b", Kind: "events", Text: "x"}, tailTestLoc); got != "2026-09-30T09:05:06-05:00 a_b events x" {
		t.Fatalf("spaced rig render = %q", got)
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
