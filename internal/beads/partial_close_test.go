package beads

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// showStatuses answers "show --json <ids>" with status for each id, taken
// from statuses, leaving out ids it does not name.
func showStatuses(args []string, statuses map[string]string) reply {
	var out []string
	for _, a := range args {
		if st, ok := statuses[a]; ok {
			out = append(out, `{"id":"`+a+`","title":"t","status":"`+st+`"}`)
		}
	}
	return reply{stdout: "[" + strings.Join(out, ",") + "]"}
}

func cmdOf(args []string) string {
	for _, a := range args {
		if !strings.HasPrefix(a, "--") {
			return a
		}
	}
	return ""
}

// TestBatchCloseReportsRefusedIssues: bd 1.2 skips a refused issue in a
// multi-issue close and exits 0. Close must re-read the batch and report the
// issues still open, not return nil as if all closed.
func TestBatchCloseReportsRefusedIssues(t *testing.T) {
	t.Parallel()
	statuses := map[string]string{"gt-a": "closed", "gt-b": "open", "gt-c": "closed"}
	r := newRecorder(func(args []string) reply {
		if cmdOf(args) == "show" {
			return showStatuses(args, statuses)
		}
		return reply{}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec

	err := b.CloseWithReason("done", "gt-a", "gt-b", "gt-c")
	var pe *PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("CloseWithReason(a, b, c) = %v, want a *PartialCloseError", err)
	}
	if !errors.Is(err, ErrCloseRefused) {
		t.Errorf("error %v does not wrap ErrCloseRefused", err)
	}
	if !reflect.DeepEqual(pe.Closed, []string{"gt-a", "gt-c"}) || !reflect.DeepEqual(pe.NotClosed, []string{"gt-b"}) {
		t.Errorf("Closed %v NotClosed %v, want [gt-a gt-c] and [gt-b]", pe.Closed, pe.NotClosed)
	}
	if got := ClosedIDs([]string{"gt-a", "gt-b", "gt-c"}, err); !reflect.DeepEqual(got, []string{"gt-a", "gt-c"}) {
		t.Errorf("ClosedIDs = %v", got)
	}

	// A batch that all closed, a single issue, and a forced batch are not
	// re-read beyond what they need.
	before := len(r.calls())
	if err := b.Close("gt-a", "gt-c"); err != nil {
		t.Errorf("Close(all closed) = %v, want nil", err)
	}
	if err := b.Close("gt-b"); err != nil {
		t.Errorf("Close(one, bd exit 0) = %v, want nil", err)
	}
	if err := b.ForceCloseWithReason("forced", "gt-a", "gt-b"); err != nil {
		t.Errorf("ForceCloseWithReason = %v, want nil", err)
	}
	var shows int
	for _, c := range r.calls()[before:] {
		if cmdOf(c.args) == "show" {
			shows++
		}
	}
	if shows != 1 {
		t.Errorf("%d show calls after the first batch, want 1 (the unforced multi-issue close only)", shows)
	}

	// A re-read that fails is reported, not taken as success.
	failing := newRecorder(func(args []string) reply {
		if cmdOf(args) == "show" {
			return reply{stderr: "Error: database locked", err: exitError{1}}
		}
		return reply{}
	})
	b.exec = failing.exec
	if err := b.Close("gt-a", "gt-b"); err == nil {
		t.Error("Close with a failed re-read = nil, want an error")
	}
}

// routedTown builds a town whose hq- issues live in the town database and
// pt- issues in a rig's, and returns a *Beads in the town database.
func routedTown(t *testing.T) (b *Beads, townBeads, rigBeads string) {
	t.Helper()
	townRoot, _ := filepath.EvalSymlinks(t.TempDir())
	rigDir := filepath.Join(townRoot, "prig", "mayor", "rig")
	townBeads = filepath.Join(townRoot, ".beads")
	rigBeads = filepath.Join(rigDir, ".beads")
	for _, dir := range []string{filepath.Join(townRoot, "mayor"), townBeads, rigBeads} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"test"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	routes := "{\"prefix\":\"hq-\",\"path\":\".\"}\n{\"prefix\":\"pt-\",\"path\":\"prig/mayor/rig\"}\n"
	if err := os.WriteFile(filepath.Join(townBeads, "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewWithBeadsDir(townRoot, townBeads), townBeads, rigBeads
}

// TestRoutedBatchCloseMergesPartialResults: a batch spanning two databases
// reports every refused issue, from whichever database refused it.
func TestRoutedBatchCloseMergesPartialResults(t *testing.T) {
	t.Parallel()
	b, _, _ := routedTown(t)
	statuses := map[string]string{"hq-a": "closed", "hq-b": "open", "pt-c": "closed", "pt-d": "closed"}
	r := newRecorder(func(args []string) reply {
		if cmdOf(args) == "show" {
			return showStatuses(args, statuses)
		}
		return reply{}
	})
	b.exec = r.exec

	err := b.Close("hq-a", "hq-b", "pt-c", "pt-d")
	var pe *PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("Close across two databases = %v, want a *PartialCloseError", err)
	}
	closed := append([]string(nil), pe.Closed...)
	sort.Strings(closed)
	if !reflect.DeepEqual(closed, []string{"hq-a", "pt-c", "pt-d"}) || !reflect.DeepEqual(pe.NotClosed, []string{"hq-b"}) {
		t.Errorf("Closed %v NotClosed %v, want [hq-a pt-c pt-d] and [hq-b]", closed, pe.NotClosed)
	}

	statuses["hq-b"] = "closed"
	if err := b.Close("hq-a", "hq-b", "pt-c", "pt-d"); err != nil {
		t.Errorf("Close across two databases, all closed = %v, want nil", err)
	}
}

// TestClearMailCountsOnlyClosedMessages: when bd refuses to close some of
// the messages (one assigned to another agent, say), ClearMail counts only
// the ones that closed, still clears the pinned ones, and reports the
// refusal. It used to count every message it asked bd to close.
func TestClearMailCountsOnlyClosedMessages(t *testing.T) {
	t.Parallel()
	statuses := map[string]string{"gt-m1": "closed", "gt-m2": "open", "gt-m3": "closed"}
	r := newRecorder(func(args []string) reply {
		switch cmdOf(args) {
		case "list":
			return reply{stdout: `[{"id":"gt-m1","title":"a","status":"open","labels":["gt:message"]},` +
				`{"id":"gt-m2","title":"b","status":"open","labels":["gt:message"]},` +
				`{"id":"gt-m3","title":"c","status":"open","labels":["gt:message"]},` +
				`{"id":"gt-p1","title":"pinned","status":"pinned","labels":["gt:message"]}]`}
		case "show":
			return showStatuses(args, statuses)
		}
		return reply{}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec

	res, err := ClearMail(b, "clear")
	if !errors.Is(err, ErrCloseRefused) {
		t.Errorf("ClearMail error = %v, want it to report the refused message", err)
	}
	if res == nil {
		t.Fatal("ClearMail returned no result")
	}
	if res.Closed != 2 || res.Cleared != 1 {
		t.Errorf("ClearMail = closed %d cleared %d, want 2 and 1", res.Closed, res.Cleared)
	}
}

// machinePartial is bd da4983e's machine-mode answer to a batch close where
// some ids failed: exit 22, error.kind "partial", the failures in error.ids.
func machinePartial(kind string, failed ...string) reply {
	var ids []string
	for _, id := range failed {
		ids = append(ids, `{"id":"`+id+`","kind":"`+kind+`","message":"cannot close `+id+`"}`)
	}
	return reply{
		stdout: `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"partial","message":"closed some","ids":[` + strings.Join(ids, ",") + `]}}`,
		stderr: "Error: cannot close " + strings.Join(failed, ", "),
		err:    exitError{22},
	}
}

// TestBatchCloseRunsInMachineModeAndReadsThePartialSplit: bd exits non-zero
// on a partial batch close (22 in machine mode). Close must not return that
// as a plain error: callers need which ids closed.
func TestBatchCloseRunsInMachineModeAndReadsThePartialSplit(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if cmdOf(args) == "close" {
			return machinePartial("refused", "gt-b")
		}
		return reply{stdout: "[]"}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec

	err := b.CloseWithReason("done", "gt-a", "gt-b", "gt-c")
	var pe *PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("CloseWithReason = %T %v, want a *PartialCloseError", err, err)
	}
	if !reflect.DeepEqual(pe.Closed, []string{"gt-a", "gt-c"}) || !reflect.DeepEqual(pe.NotClosed, []string{"gt-b"}) {
		t.Errorf("Closed %v NotClosed %v, want [gt-a gt-c] and [gt-b]", pe.Closed, pe.NotClosed)
	}
	if !errors.Is(err, ErrCloseRefused) {
		t.Errorf("refused failures must wrap ErrCloseRefused: %v", err)
	}
	for _, c := range r.calls() {
		if cmdOf(c.args) == "show" {
			t.Errorf("the envelope named the failures; no re-read needed, got %q", c.args)
		}
		if cmdOf(c.args) == "close" {
			if v, ok := lastEnvValue(c.env, "BD_MACHINE"); !ok || v != "1" {
				t.Errorf("close ran without BD_MACHINE=1: env %v", c.env)
			}
		}
	}
}

// TestBatchCloseWithAMissingIDClosesTheRest: bd since be-sut resolves each
// argument of a batch close on its own, closes the ones it can, and reports
// the ones it cannot as not_found beside them (exit 22, error.kind
// "partial"), instead of aborting the batch. Close must keep the work the
// batch did and still answer not-found for the absent id.
func TestBatchCloseWithAMissingIDClosesTheRest(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if cmdOf(args) == "close" {
			return machinePartial("not_found", "gt-nosuch")
		}
		return reply{stdout: "[]"}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec

	err := b.Close("gt-a", "gt-nosuch")
	var pe *PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("Close(a, nosuch) = %T %v, want a *PartialCloseError", err, err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Close(a, nosuch) = %v, want it to wrap ErrNotFound", err)
	}
	if !reflect.DeepEqual(pe.Closed, []string{"gt-a"}) || !reflect.DeepEqual(pe.NotClosed, []string{"gt-nosuch"}) {
		t.Errorf("Closed %v NotClosed %v, want [gt-a] and [gt-nosuch]", pe.Closed, pe.NotClosed)
	}
}

// TestForcedBatchCloseWithAMissingIDClosesTheRest: --force bypasses bd's
// policy refusals, not its resolution of each id, so a forced batch naming an
// id bd cannot resolve closes the rest and still says which closed (be-sut);
// the caller must not count the batch as a whole failure.
func TestForcedBatchCloseWithAMissingIDClosesTheRest(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if cmdOf(args) == "close" {
			return machinePartial("not_found", "gt-nosuch")
		}
		return reply{stdout: "[]"}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec

	err := b.ForceCloseWithReason("forced", "gt-a", "gt-nosuch")
	var pe *PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("ForceCloseWithReason(a, nosuch) = %T %v, want a *PartialCloseError", err, err)
	}
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("ForceCloseWithReason(a, nosuch) = %v, want it to wrap ErrNotFound", err)
	}
	if !reflect.DeepEqual(pe.Closed, []string{"gt-a"}) || !reflect.DeepEqual(pe.NotClosed, []string{"gt-nosuch"}) {
		t.Errorf("Closed %v NotClosed %v, want [gt-a] and [gt-nosuch]", pe.Closed, pe.NotClosed)
	}
}

// TestBatchCloseLegacyNonZeroExitReReads: a bd without machine mode that
// exits 1 on a partial batch leaves no envelope; Close re-reads the batch.
func TestBatchCloseLegacyNonZeroExitReReads(t *testing.T) {
	t.Parallel()
	statuses := map[string]string{"gt-a": "closed", "gt-b": "open"}
	r := newRecorder(func(args []string) reply {
		switch cmdOf(args) {
		case "close":
			return reply{stderr: "Error: cannot close gt-b: blocked by open dependency", err: exitError{1}}
		case "show":
			return showStatuses(args, statuses)
		}
		return reply{}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec
	err := b.CloseWithReason("done", "gt-a", "gt-b")
	var pe *PartialCloseError
	if !errors.As(err, &pe) {
		t.Fatalf("CloseWithReason = %T %v, want a *PartialCloseError", err, err)
	}
	if !reflect.DeepEqual(pe.Closed, []string{"gt-a"}) || !reflect.DeepEqual(pe.NotClosed, []string{"gt-b"}) {
		t.Errorf("Closed %v NotClosed %v", pe.Closed, pe.NotClosed)
	}
	if !strings.Contains(err.Error(), "blocked by open dependency") {
		t.Errorf("bd's reason lost: %v", err)
	}
}

// TestBatchCloseAllRefused: machine exit 21 closes nothing.
func TestBatchCloseAllRefused(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		if cmdOf(args) == "close" {
			return reply{
				stdout: `{"schema_version":1,"contract_version":1,"data":null,"pagination":null,"error":{"kind":"refused","message":"refused"}}`,
				err:    exitError{21},
			}
		}
		return reply{stdout: "[]"}
	})
	b := NewIsolated(t.TempDir())
	b.exec = r.exec
	err := b.CloseWithReason("done", "gt-a", "gt-b")
	var pe *PartialCloseError
	if !errors.As(err, &pe) || len(pe.Closed) != 0 || !reflect.DeepEqual(pe.NotClosed, []string{"gt-a", "gt-b"}) || !errors.Is(err, ErrCloseRefused) {
		t.Fatalf("CloseWithReason = %v, want nothing closed, both refused", err)
	}
}
