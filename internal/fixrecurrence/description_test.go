package fixrecurrence

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

func TestClassifyDescriptionTightRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		desc       string
		id         string
		want       Relationship
		wantConfid float64
	}{
		{
			name:       "recurrence of the id",
			desc:       "This is a recurrence of gt-0nk: the check fails open",
			id:         "gt-0nk",
			want:       Recurrence,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "id then the signal",
			desc:       "gt-0nk persists: the guard still lets it through",
			id:         "gt-0nk",
			want:       Recurrence,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "id then not fixed",
			desc:       "there is no new evidence that gt-0nk was not fixed by the port",
			id:         "gt-0nk",
			want:       Recurrence,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "id then the signal after a full stop",
			desc:       "gt-0nk. Not fixed by the port",
			id:         "gt-0nk",
			want:       Recurrence,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "ids fix is incomplete",
			desc:       "gt-0nk's fix is incomplete; the flag still leaks",
			id:         "gt-0nk",
			want:       Recurrence,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "ids fix did not hold",
			desc:       "gt-0nk's fix did not cover the flag",
			id:         "gt-0nk",
			want:       Recurrence,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "recurrence of the id ends the sentence",
			desc:       "A recurrence of gt-0nk.",
			id:         "gt-0nk",
			want:       Recurrence,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "same class as the id",
			desc:       "same class as gt-4mnd: another flag",
			id:         "gt-4mnd",
			want:       SameClass,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "same bug class as the id",
			desc:       "same bug class as gt-4mnd: another flag",
			id:         "gt-4mnd",
			want:       SameClass,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "id then regression",
			desc:       "gt-prk regression after the merge",
			id:         "gt-prk",
			want:       Regression,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "id then a commit then broke",
			desc:       "gt-prk (bb9c373) broke the guard",
			id:         "gt-prk",
			want:       Regression,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "id then follow-up",
			desc:       "gt-me9t follow-up: port the fix",
			id:         "gt-me9t",
			want:       FollowUp,
			wantConfid: descriptionTightConfidence,
		},
		{
			name:       "follow-up to the id",
			desc:       "Follow-up to gt-me9t: port the fix",
			id:         "gt-me9t",
			want:       FollowUp,
			wantConfid: descriptionTightConfidence,
		},
		{
			name: "background still",
			desc: "the check still fails, see gt-0nk for background",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "background since again",
			desc: "since gt-0nk we changed the flag again",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "empty id",
			desc: "the check: recurrence of the guard",
			id:   "",
			want: Reference,
		},
		{
			name: "another id carries the signal",
			desc: "recurrence of gt-zz9: the check fails open",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "a longer id is not the cited one",
			desc: "gt-0nk2 persists",
			id:   "gt-0nk",
			want: Reference,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, confidence := ClassifyDescription(tt.desc, tt.id)
			if got != tt.want {
				t.Errorf("ClassifyDescription(%q, %q) = %q, want %q", tt.desc, tt.id, got, tt.want)
			}
			if confidence != tt.wantConfid {
				t.Errorf("ClassifyDescription(%q, %q) confidence = %v, want %v", tt.desc, tt.id, confidence, tt.wantConfid)
			}
		})
	}
}

func TestSharedTestToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		a    string
		b    string
		want bool
	}{
		{
			name: "both titles name the same test",
			a:    "TestVerifyNamesEveryRig fails on a missing rig",
			b:    "flaky: TestVerifyNamesEveryRig times out",
			want: true,
		},
		{
			name: "only the first title names it",
			a:    "TestVerifyNamesEveryRig fails on a missing rig",
			b:    "the check fails on a missing rig",
			want: false,
		},
		{
			name: "only the second title names it",
			a:    "the check fails on a missing rig",
			b:    "TestVerifyNamesEveryRig times out",
			want: false,
		},
		{
			name: "neither title names a test",
			a:    "the check fails on a missing rig",
			b:    "another check fails",
			want: false,
		},
		{
			name: "each title names a different test",
			a:    "TestVerifyNamesEveryRig fails",
			b:    "TestVerifySkipsUnknownRig fails",
			want: false,
		},
		{
			name: "the first title names it twice",
			a:    "TestVerifyNamesEveryRig fails; TestVerifyNamesEveryRig flakes",
			b:    "the check fails",
			want: false,
		},
		{
			name: "a lowercase name is not a test identifier",
			a:    "testVerifyNamesEveryRig fails",
			b:    "testVerifyNamesEveryRig times out",
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := SharedTestToken(tt.a, tt.b); got != tt.want {
				t.Errorf("SharedTestToken(%q, %q) = %t, want %t", tt.a, tt.b, got, tt.want)
			}
		})
	}
}
