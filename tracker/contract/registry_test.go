package contract

import (
	"slices"
	"testing"
)

func TestCaseNamesAreUniqueAndTheFakeHasASubset(t *testing.T) {
	t.Parallel() // unit tier: inspects the registry
	names := Names()
	sorted := slices.Clone(names)
	slices.Sort(sorted)
	if len(slices.Compact(sorted)) != len(names) {
		t.Errorf("duplicate case names in %v", names)
	}
	fake := 0
	for _, c := range registry {
		if c.Fake {
			fake++
		}
		if c.Run == nil || c.Name == "" {
			t.Errorf("case %+v needs a name and a Run", c)
		}
	}
	if fake == 0 || fake > len(registry) {
		t.Errorf("%d of %d cases are flagged for the fake; want at least one", fake, len(registry))
	}
}

func TestRegisteringADuplicateNamePanics(t *testing.T) {
	t.Parallel()
	defer func() {
		if recover() == nil {
			t.Error("register accepted a duplicate name")
		}
	}()
	register(Case{Name: registry[0].Name, Run: registry[0].Run})
}
