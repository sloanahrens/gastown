package beads

import "testing"

func TestIsValidBeadID(t *testing.T) {
	t.Parallel()
	for id, want := range map[string]bool{
		"gt-abc":        true,
		"hq-cv-x_1.2":   true,
		"":              false,
		"gt abc":        false,
		"gt-abc'; --":   false,
		"external:om:x": false,
	} {
		if got := IsValidBeadID(id); got != want {
			t.Errorf("IsValidBeadID(%q) = %v, want %v", id, got, want)
		}
	}
}

func TestIsBeadIDToken(t *testing.T) {
	t.Parallel()
	for s, want := range map[string]bool{
		"gt-abc":               true,
		"om":                   true,
		"":                     false,
		"-gt":                  false,
		".hidden":              false,
		"om-gate coverage: om": false,
		"has/slash":            false,
	} {
		if got := IsBeadIDToken(s); got != want {
			t.Errorf("IsBeadIDToken(%q) = %v, want %v", s, got, want)
		}
	}
}
