package polecat

import (
	"fmt"
	"slices"
	"testing"
)

// TestIdlePolecats_YieldsEveryReusableSeatInOrder guards gt-pwn2s: a sling that
// loses the first reusable seat to a concurrent sling has to find the next one
// from the same read. IdlePolecats yields every seat FindIdlePolecat would
// accept, so a full rig of 30 directories with most of them reusable offers all
// of those and none of the parked ones; FindIdlePolecat stays the first.
func TestIdlePolecats_YieldsEveryReusableSeatInOrder(t *testing.T) {
	t.Parallel()
	names := make([]string, 30)
	for i := range names {
		names[i] = fmt.Sprintf("p%02d", i)
	}
	mgr, _, _, _, _ := canonicalWithPolecats(t, true, names...)
	parked := map[string]bool{"p00": true, "p01": true, "p17": true}
	for n := range parked {
		parkPolecat(t, mgr, n)
	}
	var want []string
	for _, n := range names {
		if !parked[n] {
			want = append(want, n)
		}
	}

	seats, err := mgr.IdlePolecats()
	if err != nil {
		t.Fatalf("IdlePolecats: %v", err)
	}
	var got []string
	for p := range seats {
		got = append(got, p.Name)
	}
	if !slices.Equal(got, want) {
		t.Errorf("IdlePolecats = %v, want %v", got, want)
	}

	first, err := mgr.FindIdlePolecat()
	if err != nil {
		t.Fatalf("FindIdlePolecat: %v", err)
	}
	if first == nil || first.Name != want[0] {
		t.Errorf("FindIdlePolecat = %v, want %s", first, want[0])
	}
}
