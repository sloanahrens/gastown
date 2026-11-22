package land

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// skipsLines returns the skips payload of every line of the reviews file that has one.
func skipsLines(t *testing.T, f *ReviewsFile) []FollowupSkips {
	t.Helper()
	raw, err := os.ReadFile(f.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var out []FollowupSkips
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var rec ReviewRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		if rec.Skips != nil {
			out = append(out, *rec.Skips)
		}
	}
	return out
}

// TestFollowupSkipsRecordCarriesNoText: a pass that skips findings appends one
// skips line holding each with class, file, reason and fingerprint, and the raw
// line holds neither the title nor the detail; a pass that skips nothing appends
// none (gt-vl0bl6).
func TestFollowupSkipsRecordCarriesNoText(t *testing.T) {
	t.Parallel()
	docNit := Finding{ID: "1", Severity: "minor", Path: "internal/land/note.go", Line: 230, Title: "wording nit", Detail: "the sentence reads badly"}
	protected := Finding{ID: "2", Severity: "minor", Path: "internal/land/hook.go", Line: 12, Title: "rename the hook", Detail: "the name is unclear"}
	for _, tc := range []struct {
		name     string
		findings []Finding
		skip     func(Work, FollowupCandidate) string
		want     []FollowupSkipped
	}{
		{"two skipped findings: one line, both, no text", []Finding{docNit, protected},
			skipByTitle("policy", "wording nit", "rename the hook"),
			[]FollowupSkipped{
				{Class: "class:" + docNit.Path, File: docNit.Path, Reason: "policy", Fingerprint: Fingerprint(docNit.Path, docNit.Title)},
				{Class: "class:" + protected.Path, File: protected.Path, Reason: "policy", Fingerprint: Fingerprint(protected.Path, protected.Title)},
			}},
		{"nothing skipped: no skips line", []Finding{docNit}, skipByTitle("policy", "no such title"), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFollowupFixture(t, 0.8, tc.findings)
			f.lander.FollowupSkip = tc.skip
			f.lander.FollowupClass = func(c FollowupCandidate) string { return "class:" + c.Path }
			f.run()
			got := skipsLines(t, f.reviews)
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("skips lines = %+v, want none", got)
				}
				return
			}
			if len(got) != 1 || len(got[0].Findings) != len(tc.want) {
				t.Fatalf("skips lines = %+v, want one line holding %d findings", got, len(tc.want))
			}
			for i, w := range tc.want {
				if got[0].Findings[i] != w {
					t.Errorf("finding %d = %+v, want %+v", i, got[0].Findings[i], w)
				}
			}
			raw, err := os.ReadFile(f.reviews.Path)
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{docNit.Title, docNit.Detail, protected.Title, protected.Detail} {
				if strings.Contains(string(raw), text) {
					t.Errorf("the reviews file holds the finding text %q", text)
				}
			}
		})
	}
}

// TestFollowupSkipsRecordFailureIsLogged: an append that fails (here a finding
// that alone does not fit a line) is logged once naming the bead and the error,
// is not retried, and the landing still files what the policy kept (gt-vl0bl6).
func TestFollowupSkipsRecordFailureIsLogged(t *testing.T) {
	t.Parallel()
	f := newFollowupFixture(t, 0.8, []Finding{
		minorAt(strings.Repeat("x", maxReviewLineBytes), 1, "wording nit"),
		minorFinding("name the error"),
	})
	f.lander.FollowupSkip = skipByTitle("policy", "wording nit")
	log := f.run()
	if n := strings.Count(log, "could not record 1 finding for gt-abc in the reviews log ("); n != 1 || !strings.Contains(log, "does not fit a review line") {
		t.Errorf("the log names the failed record %d times, want once with the error:\n%s", n, log)
	}
	if len(f.fake.created) != 1 {
		t.Errorf("filed %d beads, want the kept finding's one", len(f.fake.created))
	}
	if got := skipsLines(t, f.reviews); len(got) != 0 {
		t.Errorf("skips lines = %+v, want none written after a failed append", got)
	}
}
