package specdispatch

import (
	"strings"
	"testing"
)

// A box naming the whole lint, test or gate target is refused with a hint (gt-o9jb9a).
func TestSpecLintMakeBox(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		acceptance string
		phrase     string
	}{
		{"lint", "- [ ] make lint exits 0", "make lint"},
		{"test", "- [ ] make test passes", "make test"},
		{"gate", "- [ ] make gate passes", "make gate"},
		{"lint with punctuation", "- [ ] make lint, then read the output", "make lint"},
		{"test at end of text", "- [ ] the seat runs make test", "make test"},
		{"lint after a tab", "- [ ] the seat runs make\tlint exits 0", "make lint"},
		{"test after a doubled space", "- [ ] the seat runs make  test passes", "make test"},
		{"second occurrence after a negation", "- [ ] the seat does not run make test; make test passes", "make test"},
		{"nested test", "- [ ] a named test passes\n  - [ ] make test passes", "make test"},
		{"lint quoted as code", "- [ ] `make lint` exits 0", "make lint"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			v := lintAcceptance(tc.acceptance)
			if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
				t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
			}
			if !strings.Contains(v.Reason, `"`+tc.phrase+`"`) {
				t.Errorf("reason %q does not name %q", v.Reason, tc.phrase)
			}
			if !strings.HasSuffix(v.Reason, makeTargetHint) {
				t.Errorf("reason %q does not end with the hint", v.Reason)
			}
		})
	}
}

// A negated run, and a hyphenated or compound name that is a different target,
// is not refused (gt-o9jb9a).
func TestSpecLintMakeBoxAllowed(t *testing.T) {
	t.Parallel()
	for _, item := range []string{
		"a seat does not run make lint",
		"a seat is not to run make gate",
		"a seat will never run make lint",
		"a seat does not run make lint and will never run make test",
		"make lint-tools is installed",
		"make testdata is regenerated",
		"`make lint-tools` is installed",
	} {
		t.Run(item, func(t *testing.T) {
			t.Parallel()
			if v := lintAcceptance("- [ ] " + item); !v.Clean() {
				t.Fatalf("%q refused: %s", item, v.Line("gt-good"))
			}
		})
	}
}
