package omlessons

import (
	"fmt"
	"hash/fnv"
	"slices"
	"testing"
	"time"
)

func holdOutBeads() (ids []string) {
	for i := range 2000 {
		ids = append(ids, fmt.Sprintf("gt-%04d", i))
	}
	return ids
}

// documentedBucket is the rule the hold-out publishes: the FNV-1a hash of the
// bead id modulo 100. The test computes it itself so it finds the boundary
// beads without asserting the package's bucket helper.
func documentedBucket(bead string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(bead))
	return h.Sum32() % 100
}

// TestHeldOutShareAndClasses: a fifth of beads are held out per hold-out class, none elsewhere.
func TestHeldOutShareAndClasses(t *testing.T) {
	t.Parallel()
	in := HoldOutStart().Add(24 * time.Hour)
	beads := holdOutBeads()
	for _, class := range HoldOutClasses() {
		held := 0
		for _, id := range beads {
			if HeldOut(id, class, in) {
				held++
			}
		}
		if share := float64(held) * 100 / 2000; share < 17 || share > 23 {
			t.Errorf("class %s: %.1f%% held out, want 17 to 23", class, share)
		}
	}
	for _, id := range beads {
		if HeldOut(id, "fail-open", in) {
			t.Fatalf("bead %s held out in fail-open", id)
		}
	}
}

// TestHeldOutShareIsBelowThePercent: bucket HoldOutPercent is filed, the one below held out.
func TestHeldOutShareIsBelowThePercent(t *testing.T) {
	t.Parallel()
	in, class := HoldOutStart().Add(time.Hour), HoldOutClasses()[0]
	edge, below := "", ""
	for _, id := range holdOutBeads() {
		switch documentedBucket(id) {
		case uint32(HoldOutPercent):
			edge = id
		case uint32(HoldOutPercent) - 1:
			below = id
		}
	}
	if edge == "" || below == "" {
		t.Fatalf("no bead of 2000 lands in buckets %d and %d", HoldOutPercent, HoldOutPercent-1)
	}
	if HeldOut(edge, class, in) || !HeldOut(below, class, in) {
		t.Errorf("bucket %d must be filed and bucket %d held out", HoldOutPercent, HoldOutPercent-1)
	}
}

// TestHoldOutClassesIsACopy: a caller's rewrite of the returned classes leaves
// the hold-out withholding that class.
func TestHoldOutClassesIsACopy(t *testing.T) {
	t.Parallel()
	class, in := HoldOutClasses()[0], HoldOutStart().Add(time.Hour)
	bead := ""
	for _, id := range holdOutBeads() {
		if HeldOut(id, class, in) {
			bead = id
			break
		}
	}
	if bead == "" {
		t.Fatalf("no bead of 2000 is held out in %s inside the window", class)
	}
	classes := HoldOutClasses()
	for i := range classes {
		classes[i] = "fail-open"
	}
	if !HeldOut(bead, class, in) {
		t.Errorf("after the caller rewrote its copy, %s is no longer held out in %s", bead, class)
	}
}

// TestHeldOutIsDeterministicAndWindowed: a held bead keeps its answer in every zone and window.
func TestHeldOutIsDeterministicAndWindowed(t *testing.T) {
	t.Parallel()
	class, in := HoldOutClasses()[0], HoldOutStart().Add(time.Hour)
	i := slices.IndexFunc(holdOutBeads(), func(id string) bool { return HeldOut(id, class, in) })
	if i < 0 {
		t.Fatalf("no bead of 2000 is held out in %s inside the window", class)
	}
	bead := fmt.Sprintf("gt-%04d", i)
	start, end := HoldOutStart(), HoldOutStart().AddDate(0, 0, HoldOutDays)
	cases := []struct {
		name string
		at   time.Time
		want bool
	}{
		{"day before start", start.AddDate(0, 0, -1), false},
		{"start", start, true},
		{"last instant inside the window", end.Add(-time.Nanosecond), true},
		{"start plus 28 days", end, false},
		{"start in UTC+9", start.In(time.FixedZone("UTC+9", 9*3600)), true},
	}
	for _, c := range cases {
		for range 3 {
			if got := HeldOut(bead, class, c.at); got != c.want {
				t.Errorf("%s: HeldOut(%s, %s) = %v, want %v", c.name, bead, class, got, c.want)
			}
		}
	}
}
