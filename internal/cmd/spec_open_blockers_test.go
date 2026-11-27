package cmd

import (
	"errors"
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
