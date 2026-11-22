package land

import (
	"reflect"
	"strings"
	"testing"
)

// TestFollowupSkipsKeep: followupSkips keeps, for each finding the skip hook
// dropped, the audit entry the skips line will carry: the class FollowupClass names,
// the file, the hook's reason and the fingerprint, never the title or the claim, in
// the order skipped. A finding read twice is kept once and asks the class function
// once, a nil class function gives the empty class, and a class function that
// panics gives the empty class, the finding is still recorded and the panic is the
// one failure line of the landing (gt-xwdiiy).
func TestFollowupSkipsKeep(t *testing.T) {
	t.Parallel()
	w := Work{BeadID: "gt-abc", Rig: "gastown"}
	findings := followupCandidates([]Finding{
		minorAt("internal/land/note.go", 230, "wording nit"),
		minorAt("internal/land/hook.go", 12, "protected path"),
		minorAt("internal/land/keep.go", 3, "keep me"),
	})
	hook := func(_ Work, c FollowupCandidate) string {
		switch c.Title {
		case "wording nit":
			return "doc-nit"
		case "protected path":
			return "protected-path"
		}
		return ""
	}
	entry := func(c FollowupCandidate, class, reason string) FollowupSkipped {
		return FollowupSkipped{Class: class, File: c.Path, Reason: reason, Fingerprint: Fingerprint(c.Path, c.Title)}
	}
	named := func(c FollowupCandidate) string { return "class-of-" + c.Title }
	for _, tc := range []struct {
		name        string
		class       func(FollowupCandidate) string
		reads       int
		want        []FollowupSkipped
		wantCalls   int
		wantFailure string
	}{
		{"two skipped findings are kept in order", named, 1,
			[]FollowupSkipped{entry(findings[0], "class-of-wording nit", "doc-nit"), entry(findings[1], "class-of-protected path", "protected-path")}, 2, ""},
		{"read twice: kept once, the class asked once", named, 2,
			[]FollowupSkipped{entry(findings[0], "class-of-wording nit", "doc-nit"), entry(findings[1], "class-of-protected path", "protected-path")}, 2, ""},
		{"no class function: the empty class", nil, 1,
			[]FollowupSkipped{entry(findings[0], "", "doc-nit"), entry(findings[1], "", "protected-path")}, 0, ""},
		{"a class function that panics: the empty class, still recorded", func(FollowupCandidate) string { panic("class table is gone") }, 1,
			[]FollowupSkipped{entry(findings[0], "", "doc-nit"), entry(findings[1], "", "protected-path")}, 2, "the follow-up class function failed (class table is gone); recording the empty class"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			l := &Lander{FollowupSkip: hook}
			if tc.class != nil {
				l.FollowupClass = func(c FollowupCandidate) string {
					calls++
					return tc.class(c)
				}
			}
			skips := newFollowupSkips()
			var kept []FollowupCandidate
			for i := 0; i < tc.reads; i++ {
				kept = l.skippedCandidates(w, findings, skips)
			}
			if len(kept) != 1 || kept[0].Title != "keep me" {
				t.Errorf("kept %v, want only the finding the hook did not skip", kept)
			}
			if got := skips.skipped(); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("skipped() = %+v, want %+v", got, tc.want)
			}
			if tc.class != nil && calls != tc.wantCalls {
				t.Errorf("the class function was called %d times, want %d (once per skipped finding)", calls, tc.wantCalls)
			}
			if !strings.Contains(skips.failure, tc.wantFailure) || (tc.wantFailure == "") != (skips.failure == "") {
				t.Errorf("failure = %q, want %q", skips.failure, tc.wantFailure)
			}
		})
	}
}
