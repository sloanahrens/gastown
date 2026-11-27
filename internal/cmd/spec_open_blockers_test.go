package cmd

import (
	"errors"
	"fmt"
	"maps"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/specdispatch"
)

// The set holds exactly the blocker ids the read lists for gt-a, each once, and
// is empty for a bead with none (creator cite 2/4).
func TestOpenBlockerSet(t *testing.T) {
	t.Parallel()
	blockers := map[string][]string{"gt-a": {"gt-b", "gt-c", "gt-b"}}
	read := func(id string) (specdispatch.Spec, []string, error) {
		return specdispatch.Spec{ID: id}, blockers[id], nil
	}
	got, err := openBlockerSet(read, "gt-a")
	if err != nil {
		t.Fatalf("openBlockerSet: %v", err)
	}
	if want := map[string]bool{"gt-b": true, "gt-c": true}; !maps.Equal(got, want) {
		t.Errorf("openBlockerSet = %v, want %v", got, want)
	}
	got, err = openBlockerSet(read, "gt-none")
	if err != nil || len(got) != 0 {
		t.Errorf("openBlockerSet for a bead with no blockers = %v, %v, want an empty set", got, err)
	}
}

// A failed read of the bead is an error naming it, and no set (creator cite 2/4).
func TestOpenBlockerSetUnread(t *testing.T) {
	t.Parallel()
	read := func(string) (specdispatch.Spec, []string, error) {
		return specdispatch.Spec{}, nil, errors.New("store down")
	}
	got, err := openBlockerSet(read, "gt-a")
	if err == nil || !strings.Contains(err.Error(), "gt-a") {
		t.Fatalf("openBlockerSet error = %v, want one naming the bead", err)
	}
	if got != nil {
		t.Errorf("openBlockerSet = %v, want no set on a failed read", got)
	}
}

// A blocker two edges out is in the set, and a cycle back to a walked bead ends
// the walk, with gt-a not its own blocker (creator cite 2/4).
func TestOpenBlockerSetWalksTransitiveAndCycle(t *testing.T) {
	t.Parallel()
	blockers := map[string][]string{"gt-a": {"gt-b"}, "gt-b": {"gt-c"}, "gt-c": {"gt-b", "gt-a"}}
	read := func(id string) (specdispatch.Spec, []string, error) {
		return specdispatch.Spec{ID: id}, blockers[id], nil
	}
	got, err := openBlockerSet(read, "gt-a")
	if err != nil {
		t.Fatalf("openBlockerSet: %v", err)
	}
	if want := map[string]bool{"gt-b": true, "gt-c": true}; !maps.Equal(got, want) {
		t.Errorf("openBlockerSet = %v, want %v", got, want)
	}
}

// The walk stops at maxBlockerDepth: the ninth blocker is not in the set (creator
// cite 2/4).
func TestOpenBlockerSetDepthCap(t *testing.T) {
	t.Parallel()
	blockers := map[string][]string{"gt-a": {"gt-b1"}}
	for i := 1; i <= 9; i++ {
		blockers[fmt.Sprintf("gt-b%d", i)] = []string{fmt.Sprintf("gt-b%d", i+1)}
	}
	read := func(id string) (specdispatch.Spec, []string, error) {
		return specdispatch.Spec{ID: id}, blockers[id], nil
	}
	got, err := openBlockerSet(read, "gt-a")
	if err != nil {
		t.Fatalf("openBlockerSet: %v", err)
	}
	want := map[string]bool{}
	for i := 1; i <= 8; i++ {
		want[fmt.Sprintf("gt-b%d", i)] = true
	}
	if !maps.Equal(got, want) {
		t.Errorf("openBlockerSet = %v, want the first eight blockers %v", got, want)
	}
}

// A transitive blocker the read fails on fails the set and names it, never a
// partial set (creator cite 2/4).
func TestOpenBlockerSetUnreadBlocker(t *testing.T) {
	t.Parallel()
	blockers := map[string][]string{"gt-a": {"gt-b", "gt-c"}, "gt-c": {"gt-d"}}
	read := func(id string) (specdispatch.Spec, []string, error) {
		if id == "gt-d" {
			return specdispatch.Spec{}, nil, errors.New("store down")
		}
		return specdispatch.Spec{ID: id}, blockers[id], nil
	}
	got, err := openBlockerSet(read, "gt-a")
	if err == nil || !strings.Contains(err.Error(), "gt-d") {
		t.Fatalf("openBlockerSet error = %v, want one naming the unreadable blocker gt-d", err)
	}
	if got != nil {
		t.Errorf("openBlockerSet = %v, want no set beside a failed blocker read", got)
	}
}
