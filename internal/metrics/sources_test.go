package metrics

import (
	"context"
	"reflect"
	"sort"
	"testing"
)

func TestSourcesRegistersEightGroupsAsEmptyStubs(t *testing.T) {
	t.Parallel()
	want := []string{"attention", "config", "host", "money", "placement", "queue", "resources", "review"}
	var got []string
	for _, src := range Sources() {
		got = append(got, src.Group())
		cols, err := src.Collect(context.Background(), Env{})
		if err != nil || cols == nil || len(cols) != 0 {
			t.Errorf("%s stub = %v, %v, want an empty non-nil map", src.Group(), cols, err)
		}
	}
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered groups = %v, want %v", got, want)
	}
}
