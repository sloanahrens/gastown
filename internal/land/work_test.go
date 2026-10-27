package land

import (
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func sampleWork() Work {
	return Work{BeadID: "gt-abc", Rig: "gastown", Branch: "polecat/opal/gt-abc+x1", Head: "0123456789abcdef0123456789abcdef01234567", Target: "main", Worker: "opal"}
}

func TestReadyNoteRoundTrip(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	got, ok := ParseReadyNote("earlier notes\n" + FormatReadyNote(w))
	if !ok {
		t.Fatal("ParseReadyNote found no block")
	}
	w.BeadID, w.Rig = "", "" // not carried by the note
	if got != w {
		t.Fatalf("round trip = %+v, want %+v", got, w)
	}
}

func TestReadyNoteLastBlockWins(t *testing.T) {
	t.Parallel()
	old := sampleWork()
	old.Head = strings.Repeat("a", 40)
	cur := sampleWork()
	got, ok := ParseReadyNote(FormatReadyNote(old) + "\n" + FormatReadyNote(cur))
	if !ok || got.Head != cur.Head {
		t.Fatalf("got %+v ok=%v, want head %s", got, ok, cur.Head)
	}
}

func TestReadyNoteFieldsCannotForgeLines(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	w.Worker = "opal\nHead: " + strings.Repeat("f", 40)
	got, ok := ParseReadyNote(FormatReadyNote(w))
	if !ok || got.Head != sampleWork().Head {
		t.Fatalf("forged head leaked: %+v", got)
	}
}

func TestReadyNoteFieldsCannotForgeASubmissionTime(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	w.Submitted = time.Date(2026, 10, 1, 13, 45, 0, 0, time.UTC)
	w.Worker = "opal\nSubmitted: 1999-01-01T00:00:00Z"
	got, ok := ParseReadyNote(FormatReadyNote(w))
	if !ok || !got.Submitted.Equal(w.Submitted) {
		t.Fatalf("forged submitted leaked: %+v", got)
	}
}

// The landing worker orders the queue by the submission time; a block that
// carries none is a bead submitted before the field existed.
func TestReadyNoteRoundTripsTheSubmissionTime(t *testing.T) {
	t.Parallel()
	w := sampleWork()
	w.Submitted = time.Date(2026, 10, 1, 13, 45, 0, 0, time.UTC)
	got, ok := ParseReadyNote("earlier notes\n" + FormatReadyNote(w))
	if !ok {
		t.Fatal("ParseReadyNote found no block")
	}
	if !got.Submitted.Equal(w.Submitted) {
		t.Fatalf("Submitted = %v, want %v", got.Submitted, w.Submitted)
	}
	if plain, ok := ParseReadyNote(FormatReadyNote(sampleWork())); !ok || !plain.Submitted.IsZero() {
		t.Fatalf("a block with no Submitted = %v (ok=%v), want the zero time", plain.Submitted, ok)
	}
}

// A time this code cannot read orders as if the block carried none: the work
// still lands.
func TestReadyNoteIgnoresAnUnreadableSubmissionTime(t *testing.T) {
	t.Parallel()
	block := strings.Replace(FormatReadyNote(sampleWork()), "\nWorker: opal", "\nWorker: opal\nSubmitted: yesterday", 1)
	got, ok := ParseReadyNote(block)
	if !ok || got.Branch != sampleWork().Branch || !got.Submitted.IsZero() {
		t.Fatalf("ParseReadyNote = %+v ok=%v; want the block read with no time", got, ok)
	}
}

func TestWorkFromBeadNeedsLabelAndBlock(t *testing.T) {
	t.Parallel()
	note := FormatReadyNote(sampleWork())
	if _, err := WorkFromBead(&beads.Issue{ID: "gt-abc", Notes: note}, "gastown"); err == nil {
		t.Error("bead without the ready label accepted")
	}
	if _, err := WorkFromBead(&beads.Issue{ID: "gt-abc", Labels: []string{LabelReadyToLand}}, "gastown"); err == nil {
		t.Error("bead without a READY TO LAND block accepted")
	}
	w, err := WorkFromBead(&beads.Issue{ID: "gt-abc", Labels: []string{LabelReadyToLand}, Notes: note}, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if w != sampleWork() {
		t.Fatalf("WorkFromBead = %+v, want %+v", w, sampleWork())
	}
}

// TestOverseerReviewedNeedsTheLabelAndTheHead: the review counts only with
// the label and a note naming the exact head submitted (gt-g8t3m).
func TestOverseerReviewedNeedsTheLabelAndTheHead(t *testing.T) {
	t.Parallel()
	head := "0123456789abcdef0123456789abcdef01234567"
	note := "READY TO LAND\nHead: x\n" + OverseerReviewedMarker + " " + head + "\n"
	for _, tc := range []struct {
		name  string
		issue *beads.Issue
		want  bool
	}{
		{"label and note", &beads.Issue{Labels: []string{LabelOverseerReviewed}, Notes: note}, true},
		{"note without label", &beads.Issue{Notes: note}, false},
		{"label without note", &beads.Issue{Labels: []string{LabelOverseerReviewed}, Notes: "READY TO LAND"}, false},
		{"review of another head", &beads.Issue{Labels: []string{LabelOverseerReviewed}, Notes: OverseerReviewedMarker + " " + head[:39] + "0"}, false},
		{"short sha", &beads.Issue{Labels: []string{LabelOverseerReviewed}, Notes: OverseerReviewedMarker + " " + head[:8]}, false},
		{"no bead", nil, false},
	} {
		if got := OverseerReviewed(tc.issue, head); got != tc.want {
			t.Errorf("%s: OverseerReviewed = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// HasOverseerReviewNote answers only for a review line naming exactly head:
// the om-bypass marker (OVERSEER REVIEWED), a short sha, another head, and a
// verdict the line does not carry are all not it (gt-vsct7.4).
func TestHasOverseerReviewNote(t *testing.T) {
	t.Parallel()
	head := "0123456789abcdef0123456789abcdef01234567"
	other := "0123456789abcdef0123456789abcdef01234568"
	for _, tc := range []struct {
		name  string
		notes string
		want  bool
	}{
		{"PASS of this head", "x\n" + OverseerReviewMarker + " " + head + " PASS\n", true},
		{"FAIL of this head does not answer", OverseerReviewMarker + " " + head + " FAIL", false},
		{"FAIL naming its findings does not answer", OverseerReviewMarker + " " + head + " FAIL findings=gt-a,gt-b", false},
		{"PASS after a FAIL answers", OverseerReviewMarker + " " + head + " FAIL\n" + OverseerReviewMarker + " " + head + " PASS", true},
		{"FAIL after a PASS does not answer", OverseerReviewMarker + " " + head + " PASS\n" + OverseerReviewMarker + " " + head + " FAIL", false},
		{"PASS of another head after a FAIL does not answer", OverseerReviewMarker + " " + head + " FAIL\n" + OverseerReviewMarker + " " + other + " PASS", false},
		{"WAIVED with a reason", OverseerReviewMarker + " " + head + " WAIVED: docs-only change", true},
		{"AUDITED with a reason", "x\n" + OverseerReviewMarker + " " + head + " AUDITED: file-level audit", true},
		{"WAIVED without a reason", OverseerReviewMarker + " " + head + " WAIVED", false},
		{"WAIVED with an empty reason", OverseerReviewMarker + " " + head + " WAIVED:   ", false},
		{"AUDITED without a colon", OverseerReviewMarker + " " + head + " AUDITED looked fine", false},
		{"WAIVED of another head", OverseerReviewMarker + " " + other + " WAIVED: docs-only change", false},
		{"verdict extended by a word", OverseerReviewMarker + " " + head + " WAIVEDX: why", false},
		{"the om-bypass marker is not a review", OverseerReviewedMarker + " " + head, false},
		{"review of another head", OverseerReviewMarker + " " + other + " PASS", false},
		{"short sha", OverseerReviewMarker + " " + head[:8] + " PASS", false},
		{"verdict extended by a word", OverseerReviewMarker + " " + head + " PASSED", false},
		{"no note", "LANDING RECORD\nlanded_commit: abc", false},
	} {
		if got := HasOverseerReviewNote(tc.notes, head); got != tc.want {
			t.Errorf("%s: HasOverseerReviewNote = %v, want %v", tc.name, got, tc.want)
		}
	}
	if HasOverseerReviewNote(OverseerReviewMarker+" "+head+" PASS", "") {
		t.Error("an empty head must not be answered")
	}
}

// OverseerReviewVerdict names the verdict of the latest review line for the
// head, whatever other heads' lines come around it (gt-j84bq).
func TestOverseerReviewVerdict(t *testing.T) {
	t.Parallel()
	head := "0123456789abcdef0123456789abcdef01234567"
	other := "0123456789abcdef0123456789abcdef01234568"
	for _, tc := range []struct {
		name  string
		notes string
		want  string
		ok    bool
	}{
		{"PASS", OverseerReviewMarker + " " + head + " PASS", "PASS", true},
		{"FAIL after another head's PASS", OverseerReviewMarker + " " + other + " PASS\n" + OverseerReviewMarker + " " + head + " FAIL", "FAIL", true},
		{"WAIVED", OverseerReviewMarker + " " + head + " WAIVED: docs only", "WAIVED", true},
		{"AUDITED", OverseerReviewMarker + " " + head + " AUDITED: file-level audit", "AUDITED", true},
		{"PASS after FAIL", OverseerReviewMarker + " " + head + " FAIL\n" + OverseerReviewMarker + " " + other + " FAIL\n" + OverseerReviewMarker + " " + head + " PASS", "PASS", true},
		{"FAIL naming its findings", OverseerReviewMarker + " " + head + " FAIL findings=gt-a,gt-b", "FAIL", true},
		{"FAIL naming one finding", OverseerReviewMarker + " " + head + " FAIL findings=gt-a", "FAIL", true},
		{"only another head", OverseerReviewMarker + " " + other + " PASS", "", false},
		{"WAIVED without a reason", OverseerReviewMarker + " " + head + " WAIVED", "", false},
	} {
		got, ok := OverseerReviewVerdict(tc.notes, head)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: OverseerReviewVerdict = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := OverseerReviewVerdict(OverseerReviewMarker+" "+head+" PASS", ""); ok {
		t.Error("an empty head must not be answered")
	}
}

// PolecatReviewVerdict names the verdict of the latest verdict line a review
// document carries for the head, and passes over a document that reviewed
// another commit (gt-b9j3o.1).
func TestPolecatReviewVerdict(t *testing.T) {
	t.Parallel()
	head := "0123456789abcdef0123456789abcdef01234567"
	other := "fedcba9876543210fedcba9876543210fedcba98"
	for _, tc := range []struct {
		name string
		doc  string
		want string
		ok   bool
	}{
		{"PASS", "review\n" + PolecatReviewMarker + " " + head + " PASS\n", "PASS", true},
		{"FAIL with its defect", "review\n" + PolecatReviewMarker + " " + head + " FAIL: internal/x.go:12 ignores the error\n", "FAIL", true},
		{"an abbreviated head names it", PolecatReviewMarker + " " + head[:13] + " PASS", "PASS", true},
		{"a seven-digit abbreviation names it", PolecatReviewMarker + " " + head[:7] + " FAIL: defect", "FAIL", true},
		{"an abbreviation of another head does not", PolecatReviewMarker + " " + other[:13] + " PASS", "", false},
		{"FAIL after PASS", PolecatReviewMarker + " " + head + " PASS\n" + PolecatReviewMarker + " " + head + " FAIL: defect", "FAIL", true},
		{"PASS after FAIL", PolecatReviewMarker + " " + head + " FAIL: defect\n" + PolecatReviewMarker + " " + head + " PASS", "PASS", true},
		{"only another head", PolecatReviewMarker + " " + other + " PASS", "", false},
		{"a marker inside prose is not a verdict line", "the note says " + PolecatReviewMarker + " " + head + " PASS", "", false},
		{"PASSED is not PASS", PolecatReviewMarker + " " + head + " PASSED", "", false},
		{"no verdict line", "review of the change, nothing decided", "", false},
	} {
		got, ok := PolecatReviewVerdict(tc.doc, head)
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: PolecatReviewVerdict = %q, %v; want %q, %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if _, ok := PolecatReviewVerdict(PolecatReviewMarker+" "+head+" PASS", ""); ok {
		t.Error("an empty head must not be answered")
	}
	// A head under seven digits names too many commits to match a review line,
	// which itself may abbreviate only to seven.
	for _, abbrev := range []string{head[:3], head[:6]} {
		if _, ok := PolecatReviewVerdict(PolecatReviewMarker+" "+head+" PASS", abbrev); ok {
			t.Errorf("a head abbreviated to %q matched the review, want no match under seven digits", abbrev)
		}
	}
}

// OverseerReviewFindings reads the finding beads off the latest review line
// naming exactly head: what a FAIL's " findings=" suffix carries. A FAIL
// written before the suffix existed, and a line that names no findings, both
// read as none, so the note keeps its meaning (gt-86uzs).
func TestOverseerReviewFindings(t *testing.T) {
	t.Parallel()
	head := "0123456789abcdef0123456789abcdef01234567"
	other := "0123456789abcdef0123456789abcdef01234568"
	for _, tc := range []struct {
		name  string
		notes string
		want  []string
	}{
		{"no review line", "LANDING RECORD\nlanded_commit: abc", nil},
		{"a PASS names none", OverseerReviewMarker + " " + head + " PASS", nil},
		{"a FAIL with no suffix names none", OverseerReviewMarker + " " + head + " FAIL", nil},
		{"a FAIL naming one finding", OverseerReviewMarker + " " + head + " FAIL findings=gt-a", []string{"gt-a"}},
		{"a FAIL naming two findings", OverseerReviewMarker + " " + head + " FAIL findings=gt-a,gt-b", []string{"gt-a", "gt-b"}},
		{"leading and trailing spaces", "x\n  " + OverseerReviewMarker + " " + head + " FAIL findings=gt-a  \n", []string{"gt-a"}},
		{"an empty suffix names none", OverseerReviewMarker + " " + head + " FAIL findings=", nil},
		{"another head's finding", OverseerReviewMarker + " " + other + " FAIL findings=gt-a", nil},
		{"a suffix that is not a clean list names none", OverseerReviewMarker + " " + head + " FAIL findings=gt-a;rm -rf", nil},
		{"a trailing comma is not a clean list", OverseerReviewMarker + " " + head + " FAIL findings=gt-a,", nil},
		{"an empty entry between commas is not a clean list", OverseerReviewMarker + " " + head + " FAIL findings=gt-a,,gt-b", nil},
		{"the latest line wins", OverseerReviewMarker + " " + head + " FAIL findings=gt-a\n" + OverseerReviewMarker + " " + head + " PASS", nil},
		{"another head's PASS does not close it", OverseerReviewMarker + " " + head + " FAIL findings=gt-a\n" + OverseerReviewMarker + " " + other + " PASS", []string{"gt-a"}},
		{"a later FAIL replaces the findings", OverseerReviewMarker + " " + head + " FAIL findings=gt-a\n" + OverseerReviewMarker + " " + head + " FAIL findings=gt-b,gt-c", []string{"gt-b", "gt-c"}},
	} {
		if got := strings.Join(OverseerReviewFindings(tc.notes, head), ","); got != strings.Join(tc.want, ",") {
			t.Errorf("%s: OverseerReviewFindings = %q, want %q", tc.name, got, strings.Join(tc.want, ","))
		}
	}
	if got := OverseerReviewFindings(OverseerReviewMarker+" "+head+" FAIL findings=gt-a", ""); got != nil {
		t.Errorf("an empty head must read as no findings, got %v", got)
	}
}
