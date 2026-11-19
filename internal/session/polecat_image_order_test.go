package session

import (
	"slices"
	"testing"
)

// TestPolecatImageCopiesPinnedBdBeforeGt pins the layer order that keeps the
// pinned bd out of every build's push: a layer is rebuilt when an earlier layer
// or its own content changes, and gt changes with every commit where the pinned
// bd does not, so bd must be copied first for its layer to stay cached
// (gt-kgm97c).
//
// Mutation: swap the two COPY lines and this goes red.
func TestPolecatImageCopiesPinnedBdBeforeGt(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)
	bd := polecatImageCopyIndex(t, ins, "bd")
	gt := polecatImageCopyIndex(t, ins, "gt")
	if bd > gt {
		t.Errorf("the Dockerfile copies bin/gt at instruction %d before bin/bd at %d: bd is the pinned binary, so it goes first and its layer stops being re-pushed on every gt change", gt, bd)
	}
}

// TestPolecatImageKeepsPerCommitCopiesAfterBd pins that the binaries that change
// with a commit — gt, polecat-image and sweep-run — still follow the pinned bd
// in their order, each to the same destination with its mode from the COPY
// (gt-kgm97c).
//
// Mutation: move any of the three above the bd COPY, drop one, or change its
// destination or mode, and this goes red.
func TestPolecatImageKeepsPerCommitCopiesAfterBd(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t)
	bd := polecatImageCopyIndex(t, ins, "bd")

	prev := bd
	for _, bin := range []string{"gt", "polecat-image", "sweep-run"} {
		idx := polecatImageCopyIndex(t, ins, bin)
		if idx < prev {
			t.Errorf("the Dockerfile copies bin/%s at instruction %d, not after the pinned bd and the per-commit binaries that precede it (last at %d): the changing binaries must stay below the pinned bd for its layer to cache", bin, idx, prev)
		}
		prev = idx
	}
}

// polecatImageCopyIndex returns the index in ins of the COPY of bin/<bin> to
// /usr/local/bin/<bin> with mode 0755, and fails the test if that exact
// instruction is absent.
func polecatImageCopyIndex(t *testing.T, ins []string, bin string) int {
	t.Helper()
	want := "COPY --chmod=0755 bin/" + bin + " /usr/local/bin/" + bin
	idx := slices.Index(ins, want)
	if idx < 0 {
		t.Fatalf("the Dockerfile has no %q", want)
	}
	return idx
}
