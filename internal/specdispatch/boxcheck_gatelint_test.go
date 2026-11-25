package specdispatch

import (
	"strings"
	"testing"
)

// gateLintHint is the sentence a refusal for the landing gate's lint ends with.
const gateLintHint = "the landing worker runs the gate's lint after the seat submits, so leave it out of the boxes; the seat's own check is the narrow go vet or go test -run command"

// A box making the landing gate's lint a condition the seat must verify is
// refused at the top level, nested, and beside go vet, with the hint (gt-n9p12q).
func TestBoxCheckRefusesGateLint(t *testing.T) {
	t.Parallel()
	for _, text := range []string{
		"- [ ] the landing gate's lint passes\n- [ ] a named test passes",
		"- [ ] a named test passes\n  - [ ] the landing gate's lint passes",
		"- [ ] go vet ./internal/specdispatch and the landing gate's lint passes",
		// A typographic apostrophe (U+2019) is how a keyboard writes the
		// possessive; it must match the same phrase (gt-8cwcen).
		"- [ ] the landing gate’s lint passes",
	} {
		t.Run(text, func(t *testing.T) {
			t.Parallel()
			v := lintAcceptance(text)
			if v.Route != RouteRefuse || v.Field != acceptanceBoxField {
				t.Fatalf("Lint = %+v, want a refusal on %s", v, acceptanceBoxField)
			}
			if !strings.Contains(v.Reason, `"gate's lint"`) || !strings.HasSuffix(v.Reason, gateLintHint) {
				t.Errorf("reason %q does not name the phrase and end with the hint", v.Reason)
			}
		})
	}
}

// A box naming a lint the seat runs itself, or none, passes; an existing phrase
// keeps its wording plus the hint, and a hint-less one keeps its reason (gt-n9p12q).
func TestBoxCheckGateLintAllowed(t *testing.T) {
	t.Parallel()
	for _, item := range []string{
		"gt spec lint accepts the bead",
		"the docs lint test in internal/hygiene passes",
		"a named test passes",
	} {
		t.Run(item, func(t *testing.T) {
			t.Parallel()
			if v := lintAcceptance("- [ ] " + item); !v.Clean() {
				t.Fatalf("%q refused: %s", item, v.Line("gt-good"))
			}
		})
	}
	t.Run("existing phrase keeps its wording", func(t *testing.T) {
		t.Parallel()
		want := `item "make gate passes" names "make gate", a step a polecat cannot run; the overseer-run steps belong in the Gate, as owed by the overseer; ` + makeTargetHint
		if got := lintAcceptance("- [ ] make gate passes").Reason; got != want {
			t.Errorf("reason = %q, want %q", got, want)
		}
	})
	t.Run("phrase with no hint keeps its reason", func(t *testing.T) {
		t.Parallel()
		want := `item "docker ps shows the pool" names "docker", a step a polecat cannot run; the overseer-run steps belong in the Gate, as owed by the overseer`
		if got := lintAcceptance("- [ ] docker ps shows the pool").Reason; got != want {
			t.Errorf("reason = %q, want %q", got, want)
		}
	})
}
