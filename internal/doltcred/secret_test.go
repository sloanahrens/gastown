package doltcred

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
)

// secretCanary is distinctive text that no printing path may show.
const secretCanary = "canary-4f8a2c9e-should-never-print"

// secretScrub hides the canary and its hex form in a failure message, so a
// leaking case is reported without its value.
func secretScrub(s string) string {
	s = strings.ReplaceAll(s, hex.EncodeToString([]byte(secretCanary)), secretMask)
	return strings.ReplaceAll(s, secretCanary, secretMask)
}

type secretVisibleHolder struct {
	Secret Secret
}

type secretHiddenHolder struct {
	secret Secret
}

type secretHiddenPtrHolder struct {
	secret *Secret
}

type secretNestedHolder struct {
	inner secretHiddenHolder
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
		// value is printed with every verb. masked reports that fmt reaches
		// Format for it, so the mask itself must appear; reflection-only cases
		// print an address and are checked for the canary alone.
		value  any
		masked bool
	}{
		{"direct", s, true},
		{"pointer", &s, true},
		{"exported field", secretVisibleHolder{Secret: s}, true},
		{"unexported field", secretHiddenHolder{secret: s}, false},
		{"holder inside holder", secretNestedHolder{inner: secretHiddenHolder{secret: s}}, false},
		{"pointer in unexported field", secretHiddenPtrHolder{secret: &s}, false},
		{"slice of holders", []secretVisibleHolder{{Secret: s}}, true},
		{"map of holders", map[string]secretHiddenHolder{"k": {secret: s}}, false},
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
					t.Errorf("fmt.Sprintf(%q, %s) = %q, want the mask %q", verb, tc.name, secretScrub(out), secretMask)
				}
			})
		}
	}
	// Format writes exactly the mask for every verb. Without it fmt falls back
	// to String or a %!verb diagnostic, which still hides the value but does
	// not hold Format's per-verb promise.
	for _, verb := range verbs {
		if out := fmt.Sprintf(verb, s); out != secretMask {
			t.Errorf("fmt.Sprintf(%q, secret) = %q, want exactly %q", verb, secretScrub(out), secretMask)
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
		t.Errorf("Reveal() = %q, want the value it was built from", secretScrub(got))
	}
	var zero Secret
	if got := zero.Reveal(); got != "" {
		t.Errorf("zero Secret Reveal() = %q, want the empty string", got)
	}
}
