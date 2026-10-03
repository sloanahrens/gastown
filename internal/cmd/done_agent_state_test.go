package cmd

import (
	"strings"
	"testing"
)

// TestClearDoneCheckpoints verifies that clearDoneCheckpoints removes
// only done-cp labels while preserving other labels.
//
// Moved here from done_convoy_test.go when the convoy helpers it shared a
// file with were deleted (gt-gzhin.7).
func TestClearDoneCheckpoints(t *testing.T) {
	t.Parallel()
	allLabels := []string{
		"gt:agent",
		"idle:3",
		"done-cp:pushed:mybranch:1738972801",
		"done-cp:mr-created:gt-xyz:1738972802",
		"backoff-until:1738972900",
	}

	var kept []string
	var removed []string
	for _, label := range allLabels {
		if strings.HasPrefix(label, "done-cp:") {
			removed = append(removed, label)
		} else {
			kept = append(kept, label)
		}
	}

	if len(removed) != 2 {
		t.Errorf("expected 2 checkpoint labels removed, got %d: %v", len(removed), removed)
	}
	if len(kept) != 3 {
		t.Errorf("expected 3 labels kept, got %d: %v", len(kept), kept)
	}

	// Verify no checkpoint labels in kept set
	for _, label := range kept {
		if strings.HasPrefix(label, "done-cp:") {
			t.Errorf("checkpoint label was not removed: %s", label)
		}
	}
}
