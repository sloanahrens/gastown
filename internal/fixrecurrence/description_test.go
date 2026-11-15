package fixrecurrence

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

const (
	// wantTitleStrict is the confidence the fix-recurrence design gives a
	// title classifier's strict, id-adjacent rule (gt-e9ur8q). The rows state
	// it here rather than reading the implementation, so a rule whose
	// confidence drifts from its title counterpart fails.
	wantTitleStrict = 0.9
	// wantDescriptionScale is a description's weight against the title it
	// reuses: each confidence is the title rule's times 0.9.
	wantDescriptionScale = 0.9
)

func TestClassifyDescriptionTightRules(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		desc string
		id   string
		want Relationship
		// titleConfidence is the confidence the title classifier gives the rule
		// this row exercises, or 0 for a row no rule matches.
		titleConfidence float64
	}{
		{
			name:            "recurrence of the id",
			desc:            "This is a recurrence of gt-0nk: the check fails open",
			id:              "gt-0nk",
			want:            Recurrence,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "id then the signal",
			desc:            "gt-0nk persists: the guard still lets it through",
			id:              "gt-0nk",
			want:            Recurrence,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "id then not fixed",
			desc:            "there is no new evidence that gt-0nk was not fixed by the port",
			id:              "gt-0nk",
			want:            Recurrence,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "id then the signal after a full stop",
			desc:            "gt-0nk. Not fixed by the port",
			id:              "gt-0nk",
			want:            Recurrence,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "ids fix is incomplete",
			desc:            "gt-0nk's fix is incomplete; the flag still leaks",
			id:              "gt-0nk",
			want:            Recurrence,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "ids fix did not hold",
			desc:            "gt-0nk's fix did not cover the flag",
			id:              "gt-0nk",
			want:            Recurrence,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "recurrence of the id ends the sentence",
			desc:            "A recurrence of gt-0nk.",
			id:              "gt-0nk",
			want:            Recurrence,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "same class as the id",
			desc:            "same class as gt-4mnd: another flag",
			id:              "gt-4mnd",
			want:            SameClass,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "same bug class as the id",
			desc:            "same bug class as gt-4mnd: another flag",
			id:              "gt-4mnd",
			want:            SameClass,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "id then regression",
			desc:            "gt-prk regression after the merge",
			id:              "gt-prk",
			want:            Regression,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "id then a commit then broke",
			desc:            "gt-prk (bb9c373) broke the guard",
			id:              "gt-prk",
			want:            Regression,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "id then follow-up",
			desc:            "gt-me9t follow-up: port the fix",
			id:              "gt-me9t",
			want:            FollowUp,
			titleConfidence: wantTitleStrict,
		},
		{
			name:            "follow-up to the id",
			desc:            "Follow-up to gt-me9t: port the fix",
			id:              "gt-me9t",
			want:            FollowUp,
			titleConfidence: wantTitleStrict,
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
		{
			name: "a hyphen joins the id to a longer token on the left",
			desc: "x-gt-0nk persists",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "the id is glued to a longer token on the right",
			desc: "gt-0nk-persists",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "the signal is glued into a longer word",
			desc: "gt-0nk: the nonrecurrence check is noisy",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "the signal opens an unrelated later sentence",
			desc: "gt-0nk shipped. The weather persists.",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "no recurrence",
			desc: "gt-0nk: no recurrence since the port",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "no longer persists",
			desc: "gt-0nk: no longer persists under load",
			id:   "gt-0nk",
			want: Reference,
		},
		{
			name: "not the same class",
			desc: "this is not the same class as gt-4mnd: another flag",
			id:   "gt-4mnd",
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
			// Each rule reports its title counterpart's confidence times the
			// description scale the work item names.
			if want := tt.titleConfidence * wantDescriptionScale; confidence != want {
				t.Errorf("ClassifyDescription(%q, %q) confidence = %v, want %v (%v x %v)",
					tt.desc, tt.id, confidence, want, tt.titleConfidence, wantDescriptionScale)
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
			name: "both titles name it and the first names it twice",
			a:    "TestVerifyNamesEveryRig fails; TestVerifyNamesEveryRig flakes",
			b:    "TestVerifyNamesEveryRig times out",
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
