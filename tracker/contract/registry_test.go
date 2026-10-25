package contract

import (
	"slices"
	"testing"
)

func TestAddRefusesADuplicateNameAndKeepsOrder(t *testing.T) {
	t.Parallel() // unit tier: a local set, no global state
	var set caseSet
	set.add(Case{Name: "a"})
	set.add(Case{Name: "b"})
	if got := []string{set.cases[0].Name, set.cases[1].Name}; !slices.Equal(got, []string{"a", "b"}) {
		t.Errorf("order = %v, want registration order", got)
	}
	defer func() {
		if recover() == nil {
			t.Error("add accepted a duplicate name")
		}
		if len(set.cases) != 2 {
			t.Errorf("a refused add changed the set: %d cases", len(set.cases))
		}
	}()
	set.add(Case{Name: "a"})
}

// The package registry itself: every case has a name and a Run, names are
// unique (register would have panicked at init), the fake's subset is
// not empty, and the core create/get case stays in it.
func TestPackageRegistryIsWellFormed(t *testing.T) {
	t.Parallel()
	fake := 0
	for _, c := range registry.cases {
		if c.Name == "" || c.Run == nil {
			t.Errorf("case %+v needs a name and a Run", c)
		}
		if c.Fake {
			fake++
		}
	}
	if fake == 0 {
		t.Errorf("no case is flagged for the fake out of %d; RunFakeContract would run nothing", len(registry.cases))
	}
	if !slices.Contains(Names(), "create then get returns the issue created") {
		t.Error("the core create/get case is missing from the registry")
	}
}
