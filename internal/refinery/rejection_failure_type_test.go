package refinery

import (
	"strings"
	"testing"
)

// TestClassifyRejectionFailureType pins the vocabulary a rejection's class
// comes from, and the two answers that are not a class: an empty value (the
// caller cannot say what broke) and an unrecognized one (refused rather than
// recorded under a name no reader knows).
func TestClassifyRejectionFailureType(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "   ", want: ""},
		{in: "tests", want: FailureTypeTests},
		{in: "BUILD", want: FailureTypeBuild},
		{in: " typecheck ", want: FailureTypeTypecheck},
		{in: "lint", want: FailureTypeLint},
		{in: "editorial", want: FailureTypeEditorial},
		// mol-refinery-patrol's FIX_NEEDED bodies spell this class
		// "om-editorial"; one class must not become two entries in a bead's
		// history.
		{in: "om-editorial", want: FailureTypeEditorial},
		{in: "regression", wantErr: true},
		{in: "editorial tests", wantErr: true},
		{in: "tests;build", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ClassifyRejectionFailureType(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ClassifyRejectionFailureType(%q) = %q, want an error", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ClassifyRejectionFailureType(%q) error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ClassifyRejectionFailureType(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestRejectionRequest_ClassComesFromTheCaller is gt-1jig at the Manager seam
// both its writers go through: a rejection nobody classified reaches the note
// and the RECOVERED_BEAD mail carrying no class, where every rejection used to
// carry "editorial" — including the test failures and compile breaks whose
// real cause "editorial" points away from.
func TestRejectionRequest_ClassComesFromTheCaller(t *testing.T) {
	t.Parallel()
	mr := &MergeRequest{
		ID:           "gt-mr1",
		Branch:       "polecat/nux/gt-src1+abc123",
		TargetBranch: "main",
		IssueID:      "gt-src1",
		Worker:       "polecats/nux",
	}

	cases := []struct {
		name string
		rec  *RejectionRecord
		want string
	}{
		{name: "no record at all", rec: nil, want: ""},
		{name: "record with no class", rec: &RejectionRecord{Attempt: 2}, want: ""},
		{name: "class stated", rec: &RejectionRecord{FailureType: FailureTypeBuild}, want: FailureTypeBuild},
		{name: "class stated in another spelling", rec: &RejectionRecord{FailureType: "om-editorial"}, want: FailureTypeEditorial},
		{name: "class outside the vocabulary is dropped", rec: &RejectionRecord{FailureType: "probably-tests"}, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rejectionRequest(mr, "testrig", "gate failed", tc.rec).FailureType
			if got != tc.want {
				t.Errorf("FailureType = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestFormatMergeRejectionNote_UnclassifiedClassIsOmitted covers the note the
// polecat resume path and the attempt counters read back. The class is a
// prefix on the header line and nothing else: an unclassified rejection must
// still open with the marker and the attempt number, and still carry the
// Branch/Target/MR lines that mol-polecat-work greps for.
func TestFormatMergeRejectionNote_UnclassifiedClassIsOmitted(t *testing.T) {
	t.Parallel()
	req := deadWorkerReq()

	classified := formatMergeRejectionNote(req)
	if !strings.HasPrefix(classified, MergeRejectionNoteMarker+" (attempt 1): "+req.FailureType+" - "+req.ErrorMsg) {
		t.Errorf("classified note header = %q", classified)
	}

	req.FailureType = ""
	unclassified := formatMergeRejectionNote(req)
	if !strings.HasPrefix(unclassified, MergeRejectionNoteMarker+" (attempt 1): "+req.ErrorMsg) {
		t.Errorf("unclassified note header = %q, want the reason with no class before it", unclassified)
	}
	for _, line := range []string{
		"Branch: " + req.Branch,
		"Target: " + req.Target,
		"MR: " + req.MRID,
	} {
		if !strings.Contains(unclassified, "\n"+line) {
			t.Errorf("unclassified note lost its %q line:\n%s", line, unclassified)
		}
	}
}
