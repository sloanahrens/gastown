package sling

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// TestAdmitOverlapsDropsOnlyWhatWouldRefuse pins which overlaps a dispatcher
// admits: the ones Blocking would refuse. Shared files alone and closed
// duplicates are not refusals, so they stay in the list and still print as a
// warning (gt-e1ufo).
func TestAdmitOverlapsDropsOnlyWhatWouldRefuse(t *testing.T) {
	t.Parallel()
	candidate := Duplicate{ID: "gt-new", Status: "open", Priority: 2, CreatedAt: "2026-10-15T12:00:00Z"}
	matches := []DuplicateMatch{
		{Bead: Duplicate{ID: "gt-seat", Status: "in_progress", Priority: 2}, SharedTests: []string{"TestFoo"}},
		{Bead: Duplicate{ID: "gt-ahead", Status: "open", Priority: 2, CreatedAt: "2026-10-15T09:00:00Z"}, SharedTests: []string{"TestFoo"}},
		{Bead: Duplicate{ID: "gt-file", Status: "in_progress", Priority: 2}, SharedFiles: []string{"internal/x.go"}},
		{Bead: Duplicate{ID: "gt-closed", Status: "closed", Priority: 2}, SharedTests: []string{"TestFoo"}},
	}

	var kept []string
	for _, m := range AdmitOverlaps(candidate, matches) {
		kept = append(kept, m.Bead.ID)
	}
	if got, want := strings.Join(kept, " "), "gt-file gt-closed"; got != want {
		t.Fatalf("kept %q, want %q: an in-flight or unstarted test overlap is admitted, a file or closed overlap is not refused", got, want)
	}

	decision := DecideDuplicates(candidate, AdmitOverlaps(candidate, matches))
	if decision.Blocked {
		t.Fatalf("admitted overlaps still refuse:\n%s", decision.Message)
	}
	if strings.Contains(decision.Message, "gt-seat") || strings.Contains(decision.Message, "gt-ahead") {
		t.Errorf("message names an admitted overlap; it must say nothing about it:\n%s", decision.Message)
	}
}

// TestRunAdmitsAnOverlapOnlyWhenTheDispatcherAsks drives Run through the claim
// with an overlap the guard would refuse on work in flight. A manual sling
// (AdmitOverlap unset) keeps the refusal; the spec dispatcher's sling
// (AdmitOverlap set) hooks the bead anyway (gt-e1ufo).
func TestRunAdmitsAnOverlapOnlyWhenTheDispatcherAsks(t *testing.T) {
	t.Parallel()
	candidate := &Duplicate{ID: holdTestBeadID, Status: "open", Priority: 2}
	overlap := []DuplicateMatch{{
		Bead:        Duplicate{ID: "gt-seat", Status: "hooked", Priority: 2},
		SharedTests: []string{"TestFoo"},
	}}

	for _, tc := range []struct {
		name        string
		admit       bool
		wantRefused bool
	}{
		{"a manual sling refuses an overlap with work in flight", false, true},
		{"the spec dispatcher's sling admits it", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := &fakeSlingDeps{}
			deps := f.deps()
			deps.CheckDuplicates = func(string, string, *Bead) (*Duplicate, []DuplicateMatch, error) {
				return candidate, overlap, nil
			}
			opts := holdTestOptions()
			opts.SkipDuplicateCheck = false
			opts.AdmitOverlap = tc.admit

			_, err := Run(context.Background(), deps, opts)
			refused := errors.Is(err, ErrDuplicateContent)
			if refused != tc.wantRefused {
				t.Fatalf("refused = %v (err %v), want %v", refused, err, tc.wantRefused)
			}
			if !tc.wantRefused && (err != nil || f.hooks != 1) {
				t.Fatalf("admitted overlap: err %v, hooks %d; want the bead hooked once", err, f.hooks)
			}
		})
	}
}
