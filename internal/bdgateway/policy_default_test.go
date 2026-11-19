package bdgateway

import (
	"errors"
	"reflect"
	"testing"
)

// TestIsReadCallClasses: every verb path deploy/bd/verbs.json classes as a
// read gives IsReadCall true, and every write or admin verb false, so a class
// that drifts from the file cannot retry a write.
func TestIsReadCallClasses(t *testing.T) {
	t.Parallel()
	p := loadDeployPolicy(t)
	var reads, others int
	for _, name := range sortedVerbNames(p) {
		v := p.verbs[name]
		want := v.Class == ClassRead
		if got := IsReadCall(v.Path); got != want {
			t.Errorf("IsReadCall(%q) = %v, want %v (the file classes %q as %s)", v.Path, got, want, name, v.Class)
		}
		if want {
			reads++
		} else {
			others++
		}
	}
	// A policy with one class only would let this test pass without comparing
	// anything against the other class.
	if reads == 0 || others == 0 {
		t.Fatalf("deploy/bd/verbs.json holds %d read and %d non-read verbs, want both", reads, others)
	}
}

// TestIsReadCallDoubtful: every argv the policy cannot read as a listed read
// verb gives false, so a doubtful call is never retried as a read.
func TestIsReadCallDoubtful(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		argv []string
	}{
		{"an empty argv", nil},
		{"an argv of empty strings", []string{"", ""}},
		{"a verb the file does not list", []string{"frobnicate"}},
		{"a command tree the file does not list", []string{"dolt", "push"}},
		{"flags with no verb word", []string{"--json"}},
		{"a flag refused on every verb", []string{"show", "--db=x"}},
		{"a refused flag before the verb", []string{"--sandbox", "list"}},
		{"a flag the scan cannot place", []string{"--frobnicate", "show"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if IsReadCall(tc.argv) {
				t.Errorf("IsReadCall(%q) = true, want false", tc.argv)
			}
		})
	}
	// The embedded file parses, so the failed-load refusal is driven through
	// the loader seam IsReadCall delegates to.
	t.Run("a policy that failed to load", func(t *testing.T) {
		t.Parallel()
		failing := func() (*Policy, error) { return nil, ErrBadVerbsFile }
		if isReadCall(failing, []string{"show"}) {
			t.Error("isReadCall with a loader that fails = true, want false")
		}
	})
}

// TestDefaultPolicyMatchesFile: DefaultPolicy parses the embedded copy of
// deploy/bd/verbs.json, and that copy holds the policy the file on disk does.
func TestDefaultPolicyMatchesFile(t *testing.T) {
	t.Parallel()
	got, err := DefaultPolicy()
	if err != nil {
		t.Fatalf("DefaultPolicy() = %v, want the embedded file to parse", err)
	}
	want := loadDeployPolicy(t)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DefaultPolicy() = %+v, want the policy loaded from deploy/bd/verbs.json %+v", got, want)
	}
}

// TestEmbeddedPolicyBadFile: a policy that failed to parse returns its error
// and no policy on every call, never a nil policy a caller reads as clean
// (DefaultPolicy's refusal, driven on its own holder because the embedded file
// cannot be made to fail).
func TestEmbeddedPolicyBadFile(t *testing.T) {
	t.Parallel()
	h := embeddedPolicy{raw: []byte(`{"refused_flags": [], "verbs": []}`)}
	for call := 1; call <= 2; call++ {
		p, err := h.load()
		if !errors.Is(err, ErrBadVerbsFile) {
			t.Fatalf("load call %d = %v, want ErrBadVerbsFile", call, err)
		}
		if p != nil {
			t.Fatalf("load call %d = %v beside the error, want no policy", call, p)
		}
	}
}
