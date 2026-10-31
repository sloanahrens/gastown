package tiersweep

import (
	"reflect"
	"regexp"
	"sort"
	"testing"
)

// shardSums is the wall total of each shard, in the order the shards come back.
func shardSums(shards [][]string, walls map[string]float64) []float64 {
	var sums []float64
	for _, s := range shards {
		var total float64
		for _, n := range s {
			total += walls[n]
		}
		sums = append(sums, total)
	}
	return sums
}

// normalizeShards sorts the names inside each shard and then the shards by their
// first name, so two partitions compare equal when they hold the same names.
func normalizeShards(shards [][]string) [][]string {
	out := make([][]string, 0, len(shards))
	for _, s := range shards {
		c := append([]string(nil), s...)
		sort.Strings(c)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0] < out[j][0] })
	return out
}

// TestPartitionCoversEveryNameOnce: for each input, the names in all shards,
// taken together, are the input's names with each one counted as many times as
// it was given, so no name is lost or repeated.
func TestPartitionCoversEveryNameOnce(t *testing.T) {
	t.Parallel()
	ten := []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF", "TestG", "TestH", "TestI", "TestJ"}
	skewed := map[string]float64{
		"TestA": 900, "TestB": 5, "TestC": 5, "TestD": 5, "TestE": 5,
		"TestF": 300, "TestG": 1, "TestH": 1, "TestI": 40, "TestJ": 2,
	}
	cases := []struct {
		name  string
		names []string
		walls map[string]float64
		k     int
	}{
		{name: "empty", names: nil, walls: nil, k: 3},
		{name: "one name", names: []string{"TestOnly"}, walls: nil, k: 2},
		{name: "k greater than names", names: []string{"TestA", "TestB", "TestC"}, walls: map[string]float64{"TestA": 3}, k: 8},
		{name: "ten names skewed walls", names: ten, walls: skewed, k: 3},
		{name: "k zero reads as one", names: ten, walls: skewed, k: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var flat []string
			for _, s := range Partition(tc.names, tc.walls, tc.k) {
				flat = append(flat, s...)
			}
			want := append([]string(nil), tc.names...)
			sort.Strings(flat)
			sort.Strings(want)
			if !reflect.DeepEqual(flat, want) {
				t.Fatalf("Partition(%v, k=%d) holds %v, want every input name once: %v", tc.names, tc.k, flat, want)
			}
		})
	}
}

// TestPartitionBalancesByWall: walls 100, 60, 50, 40, 30, 20 split over two
// shards. Longest first gives 160 and 140: the optimum is 150 and 150, and the
// bead allows the LPT result within its bound. Sorting ascending instead gives
// 180 and 120, so this row goes red on that mutation.
func TestPartitionBalancesByWall(t *testing.T) {
	t.Parallel()
	walls := map[string]float64{
		"TestA": 100, "TestB": 60, "TestC": 50, "TestD": 40, "TestE": 30, "TestF": 20,
	}
	names := []string{"TestA", "TestB", "TestC", "TestD", "TestE", "TestF"}
	got := shardSums(Partition(names, walls, 2), walls)
	sort.Float64s(got)
	if want := []float64{140, 160}; !reflect.DeepEqual(got, want) {
		t.Fatalf("shard sums = %v, want %v", got, want)
	}
}

// TestPartitionUnknownWallIsMedian: TestD has no wall. The median of the known
// walls 100, 60, 50 is 60, so TestD weighs like TestB and the two share a shard.
// Read as 0 instead, TestD would join TestA.
func TestPartitionUnknownWallIsMedian(t *testing.T) {
	t.Parallel()
	walls := map[string]float64{"TestA": 100, "TestB": 60, "TestC": 50}
	names := []string{"TestA", "TestB", "TestC", "TestD"}
	got := normalizeShards(Partition(names, walls, 2))
	want := [][]string{{"TestA", "TestC"}, {"TestB", "TestD"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Partition with TestD unknown = %v, want %v", got, want)
	}
}

// TestPartitionOrderIndependent: the same names and walls given in reverse order
// give the same shards, so a rerun of the planner never reshuffles the tests.
func TestPartitionOrderIndependent(t *testing.T) {
	t.Parallel()
	walls := map[string]float64{"TestA": 7, "TestB": 7, "TestC": 3, "TestD": 9, "TestE": 1}
	forward := []string{"TestA", "TestB", "TestC", "TestD", "TestE"}
	reverse := []string{"TestE", "TestD", "TestC", "TestB", "TestA"}
	if got, want := Partition(reverse, walls, 2), Partition(forward, walls, 2); !reflect.DeepEqual(got, want) {
		t.Fatalf("reversed input gave %v, forward input gave %v", got, want)
	}
}

// TestPartitionDropsEmptyShards: three names over five shards yield three shards
// of one name each, not five with two empty.
func TestPartitionDropsEmptyShards(t *testing.T) {
	t.Parallel()
	names := []string{"TestA", "TestB", "TestC"}
	got := Partition(names, nil, 5)
	if len(got) != 3 {
		t.Fatalf("Partition of 3 names over k=5 gave %d shards %v, want 3", len(got), got)
	}
	for i, s := range got {
		if len(s) != 1 {
			t.Fatalf("shard %d = %v, want one name", i, s)
		}
	}
}

// TestRunPatternAnchorsAndEscapes: the pattern for a shard of names with regexp
// metacharacters matches exactly those names, and none of the decoys that only
// contain them, or differ from them by a metacharacter that stands for itself.
func TestRunPatternAnchorsAndEscapes(t *testing.T) {
	t.Parallel()
	shard := []string{"TestA(b)", "TestX.y", "Test[1]", "TestPlus+", "Test$"}
	decoys := []string{
		"TestA(b)extra", "xTestA(b)", "TestA(b", "TestXzy", "TestX.yz",
		"Test1", "Test[1]x", "TestPlus", "TestPlusPlus", "Test$$", "TestA",
	}
	pattern := RunPattern(shard)
	inShard := map[string]bool{}
	for _, n := range shard {
		inShard[n] = true
	}
	for _, n := range append(append([]string(nil), shard...), decoys...) {
		matched, err := regexp.MatchString(pattern, n)
		if err != nil {
			t.Fatalf("RunPattern(%v) = %q does not compile: %v", shard, pattern, err)
		}
		if matched != inShard[n] {
			t.Fatalf("pattern %q matches %q = %v, want %v", pattern, n, matched, inShard[n])
		}
	}
}

// TestRunPatternEmptyShardSelectsNothing: an empty shard must not give an empty
// pattern, which go test reads as every test. No test name is empty, so ^$
// selects none.
func TestRunPatternEmptyShardSelectsNothing(t *testing.T) {
	t.Parallel()
	pattern := RunPattern(nil)
	for _, n := range []string{"TestA", "Test", "TestMain", "x"} {
		if matched, _ := regexp.MatchString(pattern, n); matched {
			t.Fatalf("empty shard pattern %q matches %q", pattern, n)
		}
	}
}

// TestRunPatternRefusesSubtestNames: a name with a '/' is a subtest path that no
// shard pattern can select, so RunPattern panics for it, alone or beside a
// top-level name. Dropping it would leave a shard that looks complete (gt-m1h7x).
func TestRunPatternRefusesSubtestNames(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		shard []string
	}{
		{name: "subtest only", shard: []string{"TestB/sub"}},
		{name: "mixed", shard: []string{"TestA", "TestB/sub"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Fatalf("RunPattern(%v) returned a pattern instead of refusing", c.shard)
				}
			}()
			RunPattern(c.shard)
		})
	}
}
