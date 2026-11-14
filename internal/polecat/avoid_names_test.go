package polecat

import "testing"

// TestAllocateAndAddSkipsAnAvoidedName is the dispatcher's draw at the pool:
// a name whose gateway sandbox is stale is marked in use for this allocation,
// so the pool hands out the next free name instead (gt-zapy97).
func TestAllocateAndAddSkipsAnAvoidedName(t *testing.T) {
	t.Parallel()
	mgr, _, _, _ := canonicalRig(t)
	names, err := mgr.namePool.themeNames()
	if err != nil {
		t.Fatalf("theme names: %v", err)
	}
	if len(names) < 2 {
		t.Fatalf("theme names = %v, want at least two to tell an avoided draw from the next", names)
	}

	name, _, err := mgr.AllocateAndAdd(AddOptions{AvoidNames: []string{names[0]}})
	if err != nil {
		t.Fatalf("AllocateAndAdd: %v", err)
	}
	if name == names[0] {
		t.Errorf("drew %q, the name the sling avoided", name)
	}
	if name != names[1] {
		t.Errorf("drew %q, want the next free name %q", name, names[1])
	}
}
