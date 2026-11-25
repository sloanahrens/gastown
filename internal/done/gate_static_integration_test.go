//go:build integration

package done

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// listerDeadline bounds the one wait in this file: the real go list. The production seam
// (execGateListCommand) takes no context, so the deadline is the test's, set around the
// call; it is not a context passed to the child.
const listerDeadline = 2 * time.Minute

// listBounded runs lister on gr in a goroutine and returns its answer, or an error naming
// the Gate line when ctx ends first, so a hung go list cannot block the test unbounded.
func listBounded(ctx context.Context, lister gateTestLister, dir string, gr GateRun) ([]string, error) {
	type answer struct {
		got []string
		err error
	}
	done := make(chan answer, 1)
	go func() {
		got, err := lister(dir, gr)
		done <- answer{got, err}
	}()
	select {
	case a := <-done:
		return a.got, a.err
	case <-ctx.Done():
		return nil, fmt.Errorf("Gate line %q: the lister did not return: %w", gr.Pattern, ctx.Err())
	}
}

// TestIntegrationListBoundedEndsOnAHungLister: a lister that never returns ends the wait with
// an error naming the Gate line. The context is already done, so the row starts no process
// and waits on no clock.
func TestIntegrationListBoundedEndsOnAHungLister(t *testing.T) {
	t.Parallel()
	bound, stop := context.WithTimeout(t.Context(), listerDeadline)
	defer stop()
	hung := func(string, GateRun) ([]string, error) {
		select {
		case <-t.Context().Done(): // the test's end releases the goroutine
		case <-bound.Done():
		}
		return nil, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	gr := GateRun{Pattern: "TestHung"}
	got, err := listBounded(ctx, hung, ".", gr)
	if err == nil || got != nil {
		t.Fatalf("listBounded on a hung lister = %q, %v; want no answer and an error", got, err)
	}
	if want := `Gate line "TestHung"`; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to name %s", err, want)
	}
}

// TestIntegrationStaticGateListerAppliesTheBuildTags: the same Gate line without its -tags lists
// nothing on the real go, because the test file sits behind the integration build constraint,
// which go list applies and a text scan of the directory could not (gt-ssmxte).
func TestIntegrationStaticGateListerAppliesTheBuildTags(t *testing.T) {
	const line = "go test ./internal/cmd -run 'TestIntegrationAcceptanceTickRetriesAGuardMissOnRealBd' -count=1"
	runs := gateRunLines(line)
	if len(runs) != 1 {
		t.Fatalf("gateRunLines(%q) = %d lines, want 1", line, len(runs))
	}
	gr := runs[0]
	lister := newStaticGateLister(execGateListCommand, os.Environ, os.DirFS)
	ctx, cancel := context.WithTimeout(t.Context(), listerDeadline)
	defer cancel()
	got, err := listBounded(ctx, lister, "../..", gr)
	if err != nil {
		t.Fatalf("lister(%q) error = %v", gr.Pattern, err)
	}
	if len(got) != 0 {
		t.Errorf("lister(%q) = %q; want no name: the test file is behind the integration build constraint", gr.Pattern, got)
	}
}

// TestIntegrationStaticGateListerListsAPackageWhoseTestMainStartsAContainer: the real go list and
// the parser list the one test of internal/cmd's integration tier, under its tag. No test binary
// starts, so the TestMain that starts a container never runs (gt-8uqh1g).
func TestIntegrationStaticGateListerListsAPackageWhoseTestMainStartsAContainer(t *testing.T) {
	gr := GateRun{
		Pattern:  "TestIntegrationAcceptanceTickRetriesAGuardMissOnRealBd",
		Tags:     "integration",
		Packages: []string{"./internal/cmd"},
	}
	lister := newStaticGateLister(execGateListCommand, os.Environ, os.DirFS)
	ctx, cancel := context.WithTimeout(t.Context(), listerDeadline)
	defer cancel()
	got, err := listBounded(ctx, lister, "../..", gr)
	if err != nil {
		t.Fatalf("lister(%q) error = %v", gr.Pattern, err)
	}
	want := []string{gr.Pattern}
	if !slices.Equal(got, want) {
		t.Errorf("lister(%q) = %q, want %q", gr.Pattern, got, want)
	}
}
