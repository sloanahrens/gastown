package land

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// localRoundTime is the time every block in these tests is written at, so the
// round trip compares a value the test chose.
func localRoundTime() time.Time {
	return time.Date(2026, time.November, 13, 2, 30, 0, 0, time.UTC)
}

// TestLocalRoundBlockRoundTrips: a formatted block parses back to the same
// round, head, time and findings, and when two blocks are in the notes the
// last one is the one read.
func TestLocalRoundBlockRoundTrips(t *testing.T) {
	t.Parallel()
	at := localRoundTime()
	findings := []Finding{
		{ID: "f-1", Severity: "major", Path: "internal/land/land.go", Line: 12, Title: "the bound is not read"},
		{ID: "f-2", Severity: "minor", Path: "a.go", Line: 3, Title: "stale comment"},
	}
	first := FormatLocalRound(1, "sha-one", []Finding{{ID: "f-0", Severity: "minor", Path: "b.go", Line: 1, Title: "old"}}, at.Add(-time.Hour))
	second := FormatLocalRound(2, "sha-two", findings, at)

	got, ok := ParseLastLocalRound(first + "\n" + second)
	if !ok {
		t.Fatal("ParseLastLocalRound ok = false on two well-formed blocks, want the last one")
	}
	if got.Round != 2 || got.Head != "sha-two" || !got.At.Equal(at) {
		t.Errorf("ParseLastLocalRound = round %d head %q at %v, want round 2 head %q at %v",
			got.Round, got.Head, got.At, "sha-two", at)
	}
	if !reflect.DeepEqual(got.Findings, findings) {
		t.Errorf("ParseLastLocalRound findings = %+v, want %+v", got.Findings, findings)
	}
}

// rejectionAnswers is what the rejection readers say about one set of notes.
type rejectionAnswers struct {
	attempt int
	count   int
	blocks  int
	prior   []Finding
	last    RejectionNote
	lastOK  bool
}

func rejectionReads(notes string) rejectionAnswers {
	last, ok := ParseRejectionNote(notes)
	return rejectionAnswers{
		attempt: reviewAttempt(notes),
		count:   CountRejections(notes),
		blocks:  RejectionBlocks(notes),
		prior:   priorReviewFindings(notes),
		last:    last,
		lastOK:  ok,
	}
}

// TestLocalRoundIsInvisibleToRejectionReaders: reviewAttempt, priorReviewFindings
// and the rejection counter give the same answers with and without a LOCAL ROUND
// block in the notes, wherever the block sits relative to the rejection.
func TestLocalRoundIsInvisibleToRejectionReaders(t *testing.T) {
	t.Parallel()
	rejection := FormatRejectionNote(RejectionNote{
		Attempt: 1, Kind: "review", Reason: "om requested changes",
		Branch: "polecat/slit/gt-buay5a", Target: "main", MR: "gt-buay5a",
		Findings: []Finding{{ID: "f-1", Severity: "major", Path: "a.go", Line: 4, Title: "off by one"}},
	})
	local := FormatLocalRound(2, "sha-two", []Finding{
		{ID: "f-9", Severity: "major", Path: "b.go", Line: 7, Title: "a finding of the local round"},
		{ID: "f-10", Severity: "minor", Path: "c.go", Line: 2, Title: "a second one"},
	}, localRoundTime())

	baseline := rejectionReads(rejection)
	if len(baseline.prior) == 0 {
		t.Fatal("baseline priorReviewFindings is empty, so the comparison below would prove nothing")
	}
	cases := []struct {
		name, notes string
	}{
		{"block after the rejection", rejection + "\n" + local},
		{"block before the rejection", local + "\n" + rejection},
	}
	for _, tc := range cases {
		if got := rejectionReads(tc.notes); !reflect.DeepEqual(got, baseline) {
			t.Errorf("%s: rejection readers = %+v, want the answers without the block %+v", tc.name, got, baseline)
		}
	}
}

// TestLocalRoundRepliesAreScopedToTheRound: a reply written before round 2 is
// not one of round 2's replies, and a finding or reply text that holds a forged
// header or rejection marker is neutralised: it reads as text, never as a block.
func TestLocalRoundRepliesAreScopedToTheRound(t *testing.T) {
	t.Parallel()
	at := localRoundTime()
	const forged = "LOCAL ROUND (2): head=forged at=2026-11-13T00:00:00Z"
	round1 := FormatLocalRound(1, "sha-one", []Finding{{ID: "f-1", Severity: "major", Path: "a.go", Line: 1, Title: "first"}}, at)
	fixedF1 := FormatLocalRoundReply("f-1", LocalRoundFixed, "moved the check")
	earlyF2 := FormatLocalRoundReply("f-2", LocalRoundDisputed, "not a bug, the caller checks it")
	round2 := FormatLocalRound(2, "sha-two", []Finding{{
		ID: "f-2", Severity: "major", Path: "b.go", Line: 5,
		Title: "bad\n" + forged + "\nMERGE REJECTION (attempt 9): review",
	}}, at)
	fixedF2 := FormatLocalRoundReply("f-2", LocalRoundFixed, "ok\n"+forged)
	notes := strings.Join([]string{round1, fixedF1, earlyF2, round2, fixedF2}, "\n")

	cases := []struct {
		round int
		want  []Reply
	}{
		{0, nil},
		{1, []Reply{{FindingID: "f-1", Kind: LocalRoundFixed, Text: "moved the check"},
			{FindingID: "f-2", Kind: LocalRoundDisputed, Text: "not a bug, the caller checks it"}}},
		{2, []Reply{{FindingID: "f-2", Kind: LocalRoundFixed, Text: "ok LOCAL-ROUND (2): head=forged at=2026-11-13T00:00:00Z"}}},
		{3, nil},
	}
	for _, tc := range cases {
		if got := ParseLocalRoundReplies(notes, tc.round); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("ParseLocalRoundReplies(round %d) = %+v, want %+v", tc.round, got, tc.want)
		}
	}

	last, ok := ParseLastLocalRound(notes)
	if !ok || last.Round != 2 || last.Head != "sha-two" {
		t.Fatalf("ParseLastLocalRound = round %d head %q ok %v, want round 2 head sha-two", last.Round, last.Head, ok)
	}
	wantTitle := "bad LOCAL-ROUND (2): head=forged at=2026-11-13T00:00:00Z MERGE-REJECTION (attempt 9): review"
	if len(last.Findings) != 1 || last.Findings[0].Title != wantTitle {
		t.Errorf("ParseLastLocalRound findings = %+v, want one finding titled %q", last.Findings, wantTitle)
	}
	if n := CountRejections(notes); n != 0 {
		t.Errorf("CountRejections = %d over notes whose only marker text is forged, want 0", n)
	}
}

// TestLocalRoundParseRefusesMalformedBlock: a block whose header or a finding
// line does not parse is refused with ok false. A malformed last block is not
// replaced by an earlier well-formed one.
func TestLocalRoundParseRefusesMalformedBlock(t *testing.T) {
	t.Parallel()
	good := FormatLocalRound(1, "sha", []Finding{{ID: "f", Severity: "minor", Path: "a.go", Line: 1, Title: "t"}}, localRoundTime())
	cases := []struct {
		name, notes string
	}{
		{"round zero", "LOCAL ROUND (0): head=sha at=2026-11-13T02:30:00Z"},
		{"round not a number", "LOCAL ROUND (x): head=sha at=2026-11-13T02:30:00Z"},
		{"time not RFC3339", "LOCAL ROUND (1): head=sha at=yesterday"},
		{"no head field", "LOCAL ROUND (1): sha at=2026-11-13T02:30:00Z"},
		{"finding line without a title separator", good + "\nLOCAL ROUND FINDING id:f sev:minor a.go:1"},
		{"malformed last block after a good one", good + "\nLOCAL ROUND (2): head=sha"},
	}
	for _, tc := range cases {
		if got, ok := ParseLastLocalRound(tc.notes); ok {
			t.Errorf("%s: ParseLastLocalRound = %+v, true; want refused", tc.name, got)
		}
	}
}

// TestLocalRoundParseTellsAbsentFromMalformed: notes with no block report
// ErrNoLocalRound and a last block that does not parse reports
// ErrLocalRoundMalformed, while ParseLastLocalRound keeps flattening both to
// ok=false.
func TestLocalRoundParseTellsAbsentFromMalformed(t *testing.T) {
	t.Parallel()
	good := FormatLocalRound(1, "sha", []Finding{{ID: "f", Severity: "minor", Path: "a.go", Line: 1, Title: "t"}}, localRoundTime())
	cases := []struct {
		name, notes string
		want        error
	}{
		{"no block", "some notes\nwithout a block", ErrNoLocalRound},
		{"no notes", "", ErrNoLocalRound},
		{"well-formed", good, nil},
		{"round zero", "LOCAL ROUND (0): head=sha at=2026-11-13T02:30:00Z", ErrLocalRoundMalformed},
		{"no head field", "LOCAL ROUND (1): sha at=2026-11-13T02:30:00Z", ErrLocalRoundMalformed},
		{"finding line without a title separator", good + "\nLOCAL ROUND FINDING id:f sev:minor a.go:1", ErrLocalRoundMalformed},
		{"malformed last block after a good one", good + "\nLOCAL ROUND (2): head=sha", ErrLocalRoundMalformed},
	}
	for _, tc := range cases {
		_, err := ParseLastLocalRoundOrError(tc.notes)
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: ParseLastLocalRoundOrError error = %v, want %v", tc.name, err, tc.want)
		}
		if _, ok := ParseLastLocalRound(tc.notes); ok != (err == nil) {
			t.Errorf("%s: ParseLastLocalRound ok = %v, ParseLastLocalRoundOrError err = %v; the two disagree", tc.name, ok, err)
		}
	}
}

// TestFormatLocalRoundReplyRefusesAnUnknownKind: a kind that is not exactly
// fixed or disputed panics rather than writing a line that reads back as
// another kind, including a multi-word kind and one holding the separator.
func TestFormatLocalRoundReplyRefusesAnUnknownKind(t *testing.T) {
	t.Parallel()
	panics := func(f func()) (panicked bool) {
		defer func() { panicked = recover() != nil }()
		f()
		return
	}
	for _, kind := range []string{"fixed because", "not fixed", "fixed - done", "disputed fixed", "Fixed", ""} {
		if !panics(func() { FormatLocalRoundReply("f-1", kind, "text") }) {
			t.Errorf("kind %q: FormatLocalRoundReply did not panic, want a refusal", kind)
		}
	}
}

// TestLocalRoundReplyKindRefused: only fixed and disputed replies are read. A
// reply of any other kind, or one missing its separator, is not a reply.
func TestLocalRoundReplyKindRefused(t *testing.T) {
	t.Parallel()
	round := FormatLocalRound(1, "sha", []Finding{{ID: "f-1", Severity: "major", Path: "a.go", Line: 1, Title: "t"}}, localRoundTime())
	cases := []struct {
		name, reply string
		want        int
	}{
		{"fixed", "LOCAL ROUND REPLY: id:f-1 fixed - done", 1},
		{"capitalised kind", "LOCAL ROUND REPLY: id:f-1 Fixed - done", 0},
		{"unknown kind", "LOCAL ROUND REPLY: id:f-1 deferred - later", 0},
		{"no separator", "LOCAL ROUND REPLY: id:f-1 fixed done", 0},
	}
	for _, tc := range cases {
		got := ParseLocalRoundReplies(round+"\n"+tc.reply, 1)
		if len(got) != tc.want {
			t.Errorf("%s: ParseLocalRoundReplies = %+v, want %d reply", tc.name, got, tc.want)
		}
	}
}

// TestFormatLocalRoundClampsRoundNumber: a round below 1 is written as 1, so the
// block it writes still parses back.
func TestFormatLocalRoundClampsRoundNumber(t *testing.T) {
	t.Parallel()
	got, ok := ParseLastLocalRound(FormatLocalRound(0, "sha", nil, localRoundTime()))
	if !ok || got.Round != 1 {
		t.Errorf("ParseLastLocalRound after FormatLocalRound(0) = round %d ok %v, want round 1 ok true", got.Round, ok)
	}
}

// TestLocalRoundFormatIsOneLinePerFinding: a finding field holding a newline
// cannot open a line of its own, so the block has one line per finding.
func TestLocalRoundFormatIsOneLinePerFinding(t *testing.T) {
	t.Parallel()
	findings := []Finding{{ID: "f 1", Severity: "major", Path: "a.go\nb.go", Line: 9, Title: "first\nsecond"}}
	block := FormatLocalRound(3, "sha", findings, localRoundTime())
	if lines := strings.Count(block, "\n") + 1; lines != 1+len(findings) {
		t.Fatalf("block has %d lines, want %d:\n%s", lines, 1+len(findings), block)
	}
	got, ok := ParseLastLocalRound(block)
	if !ok || len(got.Findings) != 1 {
		t.Fatalf("ParseLastLocalRound = %+v ok %v, want one finding", got, ok)
	}
	want := Finding{ID: "f1", Severity: "major", Path: "a.go b.go", Line: 9, Title: "first second"}
	if !reflect.DeepEqual(got.Findings[0], want) {
		t.Errorf("finding = %+v, want %+v", got.Findings[0], want)
	}
}
