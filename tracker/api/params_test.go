package api

import "testing"

// TestInfraTypesAreTheOnesBDHides pins the list's default exclusion: the
// town's own records, and nothing else.
func TestInfraTypesAreTheOnesBDHides(t *testing.T) {
	t.Parallel()
	for _, typ := range []string{"agent", "role", "message"} {
		if !IsInfraType(typ) {
			t.Errorf("%s should be an infrastructure type: bd list hides it by default", typ)
		}
	}
	for _, typ := range []string{"task", "bug", "epic", "molecule", "convoy", "event"} {
		if IsInfraType(typ) {
			t.Errorf("%s is not an infrastructure type", typ)
		}
	}
}
