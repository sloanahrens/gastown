package tiersweep

import (
	"reflect"
	"testing"
)

// TestSerialUnits: a package's shards run as one unit where the first of them
// stood, in their order, and every other task is a unit of one.
func TestSerialUnits(t *testing.T) {
	t.Parallel()
	shard := func(name string, n int) *task { return &task{name: name, shard: &shardPart{index: n, of: 2}} }
	plain := func(name string) *task { return &task{name: name} }
	a1, a2, b1, b2 := shard("./a", 1), shard("./a", 2), shard("./b", 1), shard("./b", 2)
	// plainA prints a sharded package's name but carries no shard: it stays alone.
	p1, p2, plainA := plain("./p1"), plain("./p2"), plain("./a")
	cases := []struct {
		name string
		in   []*task
		want [][]*task
	}{
		{"shards of two packages interleaved with plain tasks", []*task{a2, p1, plainA, b1, a1, b2, p2}, [][]*task{{a2, a1}, {p1}, {plainA}, {b1, b2}, {p2}}},
		{"nil list", nil, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := serialUnits(c.in)
			// DeepEqual also tells a nil result from an empty one.
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("serialUnits = %v, want %v", unitLabels(got), unitLabels(c.want))
			}
		})
	}
}

func unitLabels(units [][]*task) [][]string {
	out := make([][]string, len(units))
	for i, u := range units {
		for _, t := range u {
			out[i] = append(out[i], t.label())
		}
	}
	return out
}
