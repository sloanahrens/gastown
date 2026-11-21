package doltcred

import (
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

// TestMain runs the unit tier's run-time checks: no goroutine a test started
// may outlive the tests, and no test may start an external tool
// (docs/testing.md, "The rules").
func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// secretCanary is distinctive text that no printing path may show.
const secretCanary = "canary-4f8a2c9e-should-never-print"

// scrubCanary hides the canary and its hex form in a failure message, so a
// leaking case is reported without its value.
func scrubCanary(s string) string {
	s = strings.ReplaceAll(s, hex.EncodeToString([]byte(secretCanary)), secretMask)
	return strings.ReplaceAll(s, secretCanary, secretMask)
}

type visibleHolder struct {
	Secret Secret
}

type hiddenHolder struct {
	secret Secret
}

type hiddenPtrHolder struct {
	secret *Secret
}

type nestedHolder struct {
	inner hiddenHolder
}

// TestSecretFormatHidesCanary prints a Secret with %v, %+v, %#v, %s, %q, %d,
// %x and %t, directly, through a pointer, as an exported and an unexported
// field, in a holder inside a holder, as a *Secret in an unexported field, and
// in a slice and a map of holders. No case may print the canary or its hex
// form.
func TestSecretFormatHidesCanary(t *testing.T) {
	t.Parallel()
	s := NewSecret(secretCanary)
	cases := []struct {
		name string
		// value is printed with every verb.
		value any
		// masked reports that fmt can reach Secret's Format method for this
		// value, so the mask itself must appear; reflection-only cases print
		// an address instead and are checked for the canary alone.
		masked bool
	}{
		{"direct", s, true},
		{"pointer", &s, true},
		{"exported field", visibleHolder{Secret: s}, true},
		{"unexported field", hiddenHolder{secret: s}, false},
		{"holder inside holder", nestedHolder{inner: hiddenHolder{secret: s}}, false},
		{"pointer in unexported field", hiddenPtrHolder{secret: &s}, false},
		{"slice of holders", []visibleHolder{{Secret: s}}, true},
		{"map of holders", map[string]hiddenHolder{"k": {secret: s}}, false},
	}
	verbs := []string{"%v", "%+v", "%#v", "%s", "%q", "%d", "%x", "%t"}
	hexCanary := hex.EncodeToString([]byte(secretCanary))
	for _, tc := range cases {
		for _, verb := range verbs {
			t.Run(tc.name+" "+verb, func(t *testing.T) {
				out := fmt.Sprintf(verb, tc.value)
				if strings.Contains(out, secretCanary) || strings.Contains(out, hexCanary) {
					t.Errorf("fmt.Sprintf(%q, %s) prints the secret", verb, tc.name)
				}
				if tc.masked && !strings.Contains(out, secretMask) {
					t.Errorf("fmt.Sprintf(%q, %s) = %q, want the mask %q", verb, tc.name, scrubCanary(out), secretMask)
				}
			})
		}
	}
	// Format writes exactly the mask for every verb. Without it fmt falls back
	// to String or a %!verb diagnostic, which still hides the value but does
	// not hold Format's per-verb promise.
	for _, verb := range verbs {
		if out := fmt.Sprintf(verb, s); out != secretMask {
			t.Errorf("fmt.Sprintf(%q, secret) = %q, want exactly %q", verb, scrubCanary(out), secretMask)
		}
	}
}

// TestSecretDirectCallsHideCanary calls String, GoString and MarshalText
// directly, because fmt prefers Format to all three.
func TestSecretDirectCallsHideCanary(t *testing.T) {
	t.Parallel()
	s := NewSecret(secretCanary)
	for _, tc := range []struct {
		name string
		got  string
	}{
		{"String", s.String()},
		{"GoString", s.GoString()},
	} {
		if tc.got != secretMask {
			t.Errorf("%s() does not return the mask", tc.name)
		}
	}
	text, err := s.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText() error = %v", err)
	}
	if string(text) != secretMask {
		t.Errorf("MarshalText() does not return the mask")
	}
}

// TestSecretReveal checks the one accessor: it returns the canary, and the zero
// Secret reveals the empty string.
func TestSecretReveal(t *testing.T) {
	t.Parallel()
	if got := NewSecret(secretCanary).Reveal(); got != secretCanary {
		t.Errorf("Reveal() = %q, want the value it was built from", scrubCanary(got))
	}
	var zero Secret
	if got := zero.Reveal(); got != "" {
		t.Errorf("zero Secret Reveal() = %q, want the empty string", got)
	}
}
