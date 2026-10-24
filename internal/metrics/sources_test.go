package metrics

import (
	"reflect"
	"sort"
	"testing"
)

// The registry test lists names only: it never calls Collect, so it runs no
// command and reads no file whatever the sources do. Each source's own test
// covers its Collect.
func TestSourcesRegistersTheEightGroups(t *testing.T) {
	t.Parallel()
	want := []string{"attention", "config", "host", "money", "placement", "queue", "resources", "review"}
	var got []string
	for _, src := range Sources() {
		got = append(got, src.Group())
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered groups = %v, want %v", got, want)
	}
}
