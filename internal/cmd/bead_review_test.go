package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/land"
)

// reviewHeads are full 40-hex heads, the shape a landing record and the
// review note carry.
const (
	firstHead  = "0123456789abcdef0123456789abcdef01234567"
	secondHead = "89abcdef0123456789abcdef0123456789abcdef"
)

// reviewTestTown is a town root whose gt- prefix routes to the gastown rig,
// so a bead's landing record resolves to .runtime/landings/gastown.jsonl.
// Landings are appended through land.RigLandingsFile, the same writer the
// landing worker uses.
func reviewTestTown(t *testing.T) string {
	t.Helper()
	town := t.TempDir()
	beadsDir := filepath.Join(town, ".beads")
	if err := os.MkdirAll(beadsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	route := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(beadsDir, beads.RoutesFileName), []byte(route), 0o644); err != nil {
		t.Fatal(err)
	}
	return town
}

// landBead appends one landing record for beadID to the town's gastown
// landings file.
func landBead(t *testing.T, town, beadID, head string, at time.Time) {
	t.Helper()
	lf, err := land.RigLandingsFile(town, "gastown")
	if err != nil {
		t.Fatal(err)
	}
	if err := lf.Append(land.LandingRecord{BeadID: beadID, Rig: "gastown", Head: head, LandedAt: at}); err != nil {
		t.Fatal(err)
	}
}

// testReviewVerbs returns verbs whose review reads landings from town, with
// the bead database aimed at id.
func testReviewVerbs(t *testing.T, town, id string) (*beadVerbs, *beads.Issue) {
	t.Helper()
	v, fake, _ := testBeadVerbs("overseer")
	v.reviewHead = func(gotID, sha string) (string, error) { return landedOverseerHead(town, gotID, sha) }
	is := mustCreateBead(t, fake, beads.CreateOptions{ID: id, Title: "landed work"})
	return v, is
}

// A review reads the head the bead's landing record names and appends the
// line the attention queue's risk-path collector answers to (gt-eorhf).
func TestBeadReviewRecordsTheLandedHead(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-eorhf")
	landBead(t, town, is.ID, secondHead, time.Now())

	if err := v.review(is.ID, "PASS", "", ""); err != nil {
		t.Fatal(err)
	}
	notes := mustShowBead(t, v.client, is.ID).Notes
	if want := land.OverseerReviewMarker + " " + secondHead + " PASS"; notes != want {
		t.Fatalf("notes = %q, want %q", notes, want)
	}
	// The queue's own predicate must accept what the verb wrote.
	if !land.HasOverseerReviewNote(notes, secondHead) {
		t.Errorf("HasOverseerReviewNote rejects the notes the verb wrote: %q", notes)
	}
	if land.HasOverseerReviewNote(notes, firstHead) {
		t.Error("the review answers a head the landing record does not name")
	}
}

// The newest landing of a bead landed twice is the one reviewed.
func TestBeadReviewUsesTheNewestLanding(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-eorhf")
	landBead(t, town, is.ID, firstHead, time.Now().Add(-time.Hour))
	landBead(t, town, is.ID, secondHead, time.Now())

	if err := v.review(is.ID, "FAIL", "", ""); err != nil {
		t.Fatal(err)
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != land.OverseerReviewMarker+" "+secondHead+" FAIL" {
		t.Errorf("notes = %q, want the newest head %s", notes, secondHead)
	}
}

// --sha reaches an older landing of the same bead.
func TestBeadReviewSHAReachesAnOlderLanding(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-eorhf")
	landBead(t, town, is.ID, firstHead, time.Now().Add(-time.Hour))
	landBead(t, town, is.ID, secondHead, time.Now())

	if err := v.review(is.ID, "PASS", firstHead, ""); err != nil {
		t.Fatal(err)
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != land.OverseerReviewMarker+" "+firstHead+" PASS" {
		t.Errorf("notes = %q, want the --sha head %s", notes, firstHead)
	}
}

// A --sha that is not a full 40-hex head in one of the bead's landing
// records is refused with exit 1, and nothing is written.
func TestBeadReviewRefusesABadSHA(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	// gt-eorhf landed once, gt-other landed at the same head: naming the
	// other bead's landing is not this bead's review.
	otherHead := "fedcba9876543210fedcba9876543210fedcba98"
	for _, tc := range []struct {
		name string
		sha  string
	}{
		{"a short sha", firstHead[:8]},
		{"a non-hex sha", strings.Repeat("z", 40)},
		{"a 40-hex head this bead never landed", otherHead},
	} {
		v, is := testReviewVerbs(t, town, "gt-eorhf")
		landBead(t, town, is.ID, firstHead, time.Now())
		if tc.name == "a 40-hex head this bead never landed" {
			landBead(t, town, "gt-other", otherHead, time.Now())
		}
		err := v.review(is.ID, "PASS", tc.sha, "")
		if err == nil {
			t.Fatalf("%s: review succeeded", tc.name)
		}
		if code := exitCodeForError(err); code != 1 {
			t.Errorf("%s: exit code %d, want 1", tc.name, code)
		}
		if notes := mustShowBead(t, v.client, is.ID).Notes; notes != "" {
			t.Errorf("%s: refused review wrote %q", tc.name, notes)
		}
	}
}

// A bead with no landing record has no head for the queue to answer, so the
// review is refused rather than guessed.
func TestBeadReviewRefusesAnUnlandedBead(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-eorhf")

	err := v.review(is.ID, "PASS", "", "")
	if err == nil || !strings.Contains(err.Error(), "no landing record") {
		t.Fatalf("review of an unlanded bead: %v", err)
	}
	if code := exitCodeForError(err); code != 1 {
		t.Errorf("exit code %d, want 1", code)
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != "" {
		t.Errorf("refused review wrote %q", notes)
	}
}

// A second review of the same head writes no second line.
func TestBeadReviewIsIdempotent(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-eorhf")
	landBead(t, town, is.ID, secondHead, time.Now())

	for i := 0; i < 2; i++ {
		if err := v.review(is.ID, "PASS", "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != land.OverseerReviewMarker+" "+secondHead+" PASS" {
		t.Errorf("after two reviews, notes = %q", notes)
	}
}

// The verdict is PASS or FAIL, and nothing else.
func TestBeadReviewRefusesABadVerdict(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-eorhf")
	landBead(t, town, is.ID, secondHead, time.Now())

	for _, verdict := range []string{"", "PASSED", "maybe", "pass fail"} {
		if err := v.review(is.ID, verdict, "", ""); err == nil {
			t.Errorf("verdict %q accepted", verdict)
		}
	}
	// A lowercase verdict is the same verdict.
	if err := v.review(is.ID, " pass ", "", ""); err != nil {
		t.Fatalf("lowercase verdict: %v", err)
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != land.OverseerReviewMarker+" "+secondHead+" PASS" {
		t.Errorf("notes = %q", notes)
	}
}

// A town-level bead (hq-) has no rig, so there is no landings file to read.
func TestBeadReviewRefusesATownLevelBead(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "hq-abc")

	err := v.review(is.ID, "PASS", "", "")
	if err == nil || !strings.Contains(err.Error(), "which rig") {
		t.Fatalf("review of a town-level bead: %v", err)
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != "" {
		t.Errorf("refused review wrote %q", notes)
	}
}

// WAIVED and AUDITED write "OVERSEER REVIEW <head> <VERDICT>: <reason>" for
// the landed head, in the form the attention queue's predicate answers to
// (gt-r5ne9).
func TestBeadReviewRecordsWaivedAndAudited(t *testing.T) {
	t.Parallel()
	for _, verdict := range []string{"WAIVED", "AUDITED"} {
		town := reviewTestTown(t)
		v, is := testReviewVerbs(t, town, "gt-r5ne9")
		landBead(t, town, is.ID, secondHead, time.Now())

		if err := v.review(is.ID, strings.ToLower(verdict), "", "file-level audit,\n no findings"); err != nil {
			t.Fatalf("%s: %v", verdict, err)
		}
		notes := mustShowBead(t, v.client, is.ID).Notes
		want := land.OverseerReviewMarker + " " + secondHead + " " + verdict + ": file-level audit, no findings"
		if notes != want {
			t.Errorf("%s: notes = %q, want %q", verdict, notes, want)
		}
		if !land.HasOverseerReviewNote(notes, secondHead) {
			t.Errorf("%s: HasOverseerReviewNote rejects the notes the verb wrote: %q", verdict, notes)
		}
		if land.HasOverseerReviewNote(notes, firstHead) {
			t.Errorf("%s: the review answers a head the landing record does not name", verdict)
		}
	}
}

// WAIVED and AUDITED without a non-empty reason are refused with exit 1, and
// nothing is written.
func TestBeadReviewRefusesWaivedAndAuditedWithoutAReason(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-r5ne9")
	landBead(t, town, is.ID, secondHead, time.Now())

	for _, verdict := range []string{"WAIVED", "AUDITED"} {
		for _, reason := range []string{"", "   ", "\n\t"} {
			err := v.review(is.ID, verdict, "", reason)
			if err == nil || !strings.Contains(err.Error(), "--reason") {
				t.Fatalf("%s with reason %q: err = %v, want a --reason error", verdict, reason, err)
			}
			if code := exitCodeForError(err); code != 1 {
				t.Errorf("%s with reason %q: exit code %d, want 1", verdict, reason, code)
			}
		}
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != "" {
		t.Errorf("refused reviews wrote %q", notes)
	}
}

// A reason belongs to WAIVED and AUDITED. PASS and FAIL stay as they were, so
// a reason passed with one is refused rather than dropped.
func TestBeadReviewRefusesAReasonWithPassOrFail(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-r5ne9")
	landBead(t, town, is.ID, secondHead, time.Now())

	for _, verdict := range []string{"PASS", "FAIL"} {
		if err := v.review(is.ID, verdict, "", "because"); err == nil {
			t.Errorf("%s with a reason accepted", verdict)
		}
	}
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != "" {
		t.Errorf("refused reviews wrote %q", notes)
	}
}

// Recording the same WAIVED or AUDITED verdict twice writes one line, and a
// second verdict for a head that already has one is left alone, as for PASS.
func TestBeadReviewWaivedAndAuditedAreIdempotentPerHead(t *testing.T) {
	t.Parallel()
	town := reviewTestTown(t)
	v, is := testReviewVerbs(t, town, "gt-r5ne9")
	landBead(t, town, is.ID, secondHead, time.Now())

	for i := 0; i < 2; i++ {
		if err := v.review(is.ID, "WAIVED", "", "docs only"); err != nil {
			t.Fatal(err)
		}
	}
	if err := v.review(is.ID, "AUDITED", "", "audited later"); err != nil {
		t.Fatal(err)
	}
	want := land.OverseerReviewMarker + " " + secondHead + " WAIVED: docs only"
	if notes := mustShowBead(t, v.client, is.ID).Notes; notes != want {
		t.Errorf("notes = %q, want one line %q", notes, want)
	}
}
