package beads

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

// specDispatchReadyOptions is the query the spec dispatcher's board read makes
// (internal/cmd/spec.go's specReadyOptions): unassigned work beads, the town's
// bookkeeping families and the dispatcher's own exclusions filtered
// server-side, the whole board. The label list is
// constants.NonDispatchableBeadLabels followed by specdispatch.ReadyQueryLabels
// (which is gt:ready-to-land and needs-human today); the type list is
// specdispatch.NonWorkBeadTypes.
func specDispatchReadyOptions() ReadyOptions {
	labels := append([]string(nil), constants.NonDispatchableBeadLabels...)
	labels = append(labels, "gt:ready-to-land", "needs-human")
	return ReadyOptions{
		Unassigned:    true,
		ExcludeLabels: labels,
		ExcludeTypes:  append([]string{constants.EpicBeadType}, constants.NonDispatchableBeadTypes...),
		Limit:         0,
	}
}

// TestReadyFiltered_SendsTheSpecDispatchArgv pins the argv the seam runs for
// the dispatcher's board: the same one internal/cmd/spec.go built by hand
// before the query moved behind beads.Client (gt-p85pw.7), in the same order,
// with each exclusion list joined once.
func TestReadyFiltered_SendsTheSpecDispatchArgv(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: readyBoardEnvelope(2, false)} })
	opts := specDispatchReadyOptions()
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyFiltered(opts)
	if err != nil {
		t.Fatalf("ReadyFiltered: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("ReadyFiltered = %d issues, want 2", len(issues))
	}
	calls := rec.calls()
	if len(calls) != 1 {
		t.Fatalf("calls = %q, want one bd ready", rec.argvs())
	}
	want := []string{
		"ready", "--json",
		"--unassigned",
		"--exclude-label", strings.Join(opts.ExcludeLabels, ","),
		"--exclude-type", strings.Join(opts.ExcludeTypes, ","),
		"--limit", "0",
	}
	if !slices.Equal(calls[0].args, want) {
		t.Errorf("argv = %q, want %q", calls[0].args, want)
	}
}

// TestReadyFiltered_RefusesACutWholeBoard: --limit 0 asks for every ready
// issue, so a page bd reports as cut is a failure, not a smaller board
// (gt-59o9's rule for ReadyAll).
func TestReadyFiltered_RefusesACutWholeBoard(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: readyBoardEnvelope(100, true)} })
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyFiltered(specDispatchReadyOptions())
	if err == nil {
		t.Fatalf("ReadyFiltered accepted a cut board as %d issues", len(issues))
	}
	var truncated *ErrReadyTruncated
	if errors.As(err, &truncated) {
		t.Errorf("err = %v, want a plain failure for a --limit 0 board", err)
	}
}

// TestReadyFiltered_APageRequestCarriesTheTruncation: a positive limit is a
// page request, so the cut page comes back with the sentinel Ready uses.
func TestReadyFiltered_APageRequestCarriesTheTruncation(t *testing.T) {
	t.Parallel()
	rec := newRecorder(func([]string) reply { return reply{stdout: readyBoardEnvelope(5, true)} })
	issues, err := newRecordedBeads(t.TempDir(), rec).ReadyFiltered(ReadyOptions{Limit: 5})
	var truncated *ErrReadyTruncated
	if !errors.As(err, &truncated) {
		t.Fatalf("ReadyFiltered(limit 5) err = %v, want *ErrReadyTruncated", err)
	}
	if truncated.Cap != 5 || truncated.Found != 5 {
		t.Errorf("truncation = %+v, want cap 5 and 5 found", *truncated)
	}
	if len(issues) != 5 {
		t.Errorf("ReadyFiltered(limit 5) = %d issues, want the 5-row page", len(issues))
	}
}
