package ring

import (
	"testing"
)

// TestRemoveDeletesTheRingAndKeepsEverySection: after Remove the ring section
// is gone and every other section of mayor/town.json is as it was.
func TestRemoveDeletesTheRingAndKeepsEverySection(t *testing.T) {
	t.Parallel()
	rec, err := Sign(Record{ForgedAt: now, ExpiresAt: now.Add(Lease), ForgedBy: "mayor"}, testKey)
	if err != nil {
		t.Fatal(err)
	}
	root := townWith(t, rec)
	path := mustTownPath(t, root)
	before := sections(t, path)

	removed, err := Remove(root)
	if err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if !removed {
		t.Fatal("Remove of a town with a ring reported nothing removed")
	}
	after := sections(t, path)
	if _, ok := after[SectionKey]; ok {
		t.Fatal("the ring section is still in mayor/town.json after Remove")
	}
	for key, value := range before {
		if key == SectionKey {
			continue
		}
		if after[key] != value {
			t.Errorf("section %q changed on Remove", key)
		}
	}
}

// TestRemoveReportsATownWithNoRing: removing a ring that is not there is not
// an error, and reports that nothing was removed.
func TestRemoveReportsATownWithNoRing(t *testing.T) {
	t.Parallel()
	root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
	removed, err := Remove(root)
	if err != nil {
		t.Fatalf("Remove of a town with no ring: %v", err)
	}
	if removed {
		t.Fatal("Remove of a town with no ring reported a removal")
	}
}

// TestRemoveRefusesATownWithNoFile: a town whose mayor/town.json is missing is
// an error to remove from, not a town with no ring.
func TestRemoveRefusesATownWithNoFile(t *testing.T) {
	t.Parallel()
	if _, err := Remove(t.TempDir()); err == nil {
		t.Fatal("Remove of a town with no mayor/town.json = nil error, want a refusal")
	}
}

// TestRemoveRefusesAnUnregisteredSection: a layout that does not register the
// ring section is refused, not guessed at.
func TestRemoveRefusesAnUnregisteredSection(t *testing.T) {
	t.Parallel()
	root := tempTown(t, map[string]any{"type": "town", "version": 2, "name": "test"})
	unregistered := func(string) (string, bool) { return "", false }
	if _, err := remove(root, unregistered); err == nil {
		t.Fatal("remove with an unregistered ring section = nil error, want a refusal")
	}
}
