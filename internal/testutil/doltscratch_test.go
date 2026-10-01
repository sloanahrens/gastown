//go:build !windows

package testutil

import (
	"slices"
	"testing"
)

// A lease's reset drops exactly what the test added: the image's databases
// and anything the container started with stay.
func TestDatabasesAdded(t *testing.T) {
	t.Parallel()
	baseline := []string{"gt_test", "information_schema", "mysql"}
	now := []string{"gt_test", "hq", "information_schema", "mysql", "testrig"}
	if got := databasesAdded(baseline, now); !slices.Equal(got, []string{"hq", "testrig"}) {
		t.Fatalf("databasesAdded = %q, want [hq testrig]", got)
	}
	if got := databasesAdded(baseline, baseline); len(got) != 0 {
		t.Fatalf("databasesAdded on an untouched catalog = %q, want none", got)
	}
}
