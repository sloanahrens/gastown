package reaper

import (
	"errors"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// writerCall is one bd verb a fake Writer received.
type writerCall struct {
	verb   string // "close", "force-close" or "delete"
	reason string
	ids    []string
}

// fakeReaperWriter applies the reaper's bd writes to the fake state, the way
// bd would to the database the fake driver reads, and records each call.
type fakeReaperWriter struct {
	state *fakeReaperState
	calls []writerCall
	// refuse names ids bd refuses to close; a batch containing one reports a
	// *beads.PartialCloseError, as bd's partial exit does.
	refuse map[string]bool
}

func (s *fakeReaperState) writer() *fakeReaperWriter {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.w == nil {
		s.w = &fakeReaperWriter{state: s}
	}
	return s.w
}

func (w *fakeReaperWriter) close(verb, reason string, ids []string) error {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	w.calls = append(w.calls, writerCall{verb: verb, reason: reason, ids: append([]string(nil), ids...)})
	pe := &beads.PartialCloseError{}
	for _, id := range ids {
		if w.refuse[id] {
			pe.NotClosed = append(pe.NotClosed, id)
			continue
		}
		if wisp := w.state.wisps[id]; wisp != nil {
			wisp.status = "closed"
		}
		if issue := w.state.staleIssues[id]; issue != nil {
			issue.status = "closed"
		}
		pe.Closed = append(pe.Closed, id)
	}
	if len(pe.NotClosed) > 0 {
		return pe
	}
	return nil
}

func (w *fakeReaperWriter) CloseWithReason(reason string, ids ...string) error {
	return w.close("close", reason, ids)
}

func (w *fakeReaperWriter) ForceCloseWithReason(reason string, ids ...string) error {
	return w.close("force-close", reason, ids)
}

func (w *fakeReaperWriter) DeleteIssues(ids ...string) error {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	w.calls = append(w.calls, writerCall{verb: "delete", ids: append([]string(nil), ids...)})
	for _, id := range ids {
		delete(w.state.wisps, id)
	}
	return nil
}

func (w *fakeReaperWriter) callsOf(verb string) []writerCall {
	w.state.mu.Lock()
	defer w.state.mu.Unlock()
	var out []writerCall
	for _, c := range w.calls {
		if c.verb == verb {
			out = append(out, c)
		}
	}
	return out
}

func idsOf(calls []writerCall) []string {
	var ids []string
	for _, c := range calls {
		ids = append(ids, c.ids...)
	}
	sort.Strings(ids)
	return ids
}

// TestReaperSourceIssuesNoWrites is the source half of gt-fcxe9.12: reaper.go
// holds no DML, DDL or Dolt commit. The fake driver (which fails any Exec)
// is the behavioral half.
func TestReaperSourceIssuesNoWrites(t *testing.T) {
	data, err := os.ReadFile("reaper.go")
	if err != nil {
		t.Fatal(err)
	}
	write := regexp.MustCompile(`(?i)\b(UPDATE\s+\S+\s+SET|DELETE\s+FROM|INSERT\s+(IGNORE\s+)?INTO|REPLACE\s+INTO|CREATE\s+TABLE|DROP\s+TABLE|DOLT_COMMIT|DOLT_ADD|ExecContext)\b`)
	for i, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "//") {
			continue
		}
		if m := write.FindString(line); m != "" {
			t.Errorf("reaper.go:%d writes through SQL (%s): %s", i+1, m, strings.TrimSpace(line))
		}
	}
}

// TestLiveReaperRunsRefuseWithoutAWriter: with no bd writer a live run must
// refuse before touching anything, never fall back to SQL.
func TestLiveReaperRunsRefuseWithoutAWriter(t *testing.T) {
	state := newStaleIssueState("hq-a")
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })

	if _, err := Reap(db, nil, "hq", time.Hour, false); !errors.Is(err, ErrNoWriter) {
		t.Errorf("Reap live with nil writer = %v, want ErrNoWriter", err)
	}
	if _, err := Purge(db, nil, "hq", time.Hour, time.Hour, false); !errors.Is(err, ErrNoWriter) {
		t.Errorf("Purge live with nil writer = %v, want ErrNoWriter", err)
	}
	if _, err := AutoClose(db, nil, "hq", AutoCloseOptions{StaleAge: MinStaleIssueAge, Force: true}); !errors.Is(err, ErrNoWriter) {
		t.Errorf("AutoClose live with nil writer = %v, want ErrNoWriter", err)
	}
	for id, status := range state.staleIssueStatuses() {
		if status != "open" {
			t.Errorf("%s = %q after a refused run, want open", id, status)
		}
	}
	// A dry run needs no writer.
	if _, err := Reap(db, nil, "hq", time.Hour, true); err != nil {
		t.Errorf("dry-run Reap with nil writer: %v", err)
	}
}

// TestReapForceClosesThroughBdWithAReason: stale wisps close with bd close
// --force (the reaper always closed regardless of children) and a reason, so
// bd records who closed them and why.
func TestReapForceClosesThroughBdWithAReason(t *testing.T) {
	old := time.Now().UTC().Add(-48 * time.Hour)
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"stale-a": {id: "stale-a", status: "open", issueType: "task", createdAt: old},
			"stale-b": {id: "stale-b", status: "hooked", issueType: "task", createdAt: old},
			"fresh":   {id: "fresh", status: "open", issueType: "task", createdAt: time.Now().UTC()},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()

	res, err := Reap(db, w, "testdb", 24*time.Hour, false)
	if err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if res.Reaped != 2 {
		t.Errorf("Reaped = %d, want 2", res.Reaped)
	}
	forced := w.callsOf("force-close")
	if got := idsOf(forced); !reflect.DeepEqual(got, []string{"stale-a", "stale-b"}) {
		t.Errorf("force-closed %v, want [stale-a stale-b]", got)
	}
	for _, c := range forced {
		if !strings.HasPrefix(c.reason, "reaper: ") {
			t.Errorf("close reason %q does not name the reaper", c.reason)
		}
	}
	if n := len(w.callsOf("close")) + len(w.callsOf("delete")); n != 0 {
		t.Errorf("Reap made %d non-force writes, want 0", n)
	}
}

// TestReapCountsOnlyWhatBdClosed: a partial close counts the ids bd closed
// and returns bd's refusal instead of reporting the whole batch closed.
func TestReapCountsOnlyWhatBdClosed(t *testing.T) {
	old := time.Now().UTC().Add(-48 * time.Hour)
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"stale-a": {id: "stale-a", status: "open", issueType: "task", createdAt: old},
			"stale-b": {id: "stale-b", status: "open", issueType: "task", createdAt: old},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()
	w.refuse = map[string]bool{"stale-b": true}

	res, err := Reap(db, w, "testdb", 24*time.Hour, false)
	if !errors.Is(err, beads.ErrCloseRefused) {
		t.Errorf("Reap error = %v, want bd's refusal", err)
	}
	if res == nil || res.Reaped != 1 {
		t.Fatalf("Reap result = %+v, want Reaped 1", res)
	}
}

// TestPurgeDeletesThroughBd: closed wisps past purge age are deleted with
// bd delete, and a wisp a live agent still references is not in the batch.
func TestPurgeDeletesThroughBd(t *testing.T) {
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"done-a":     {id: "done-a", status: "closed", issueType: "task", createdAt: old, closedAt: old},
			"done-b":     {id: "done-b", status: "closed", issueType: "task", createdAt: old, closedAt: old},
			"referenced": {id: "referenced", status: "closed", issueType: "task", createdAt: old, closedAt: old},
			"live-agent": {id: "live-agent", status: "open", issueType: "agent", createdAt: time.Now().UTC(), description: "agent_state: working\nhook_bead: referenced\n"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()

	res, err := Purge(db, w, "testdb", 7*24*time.Hour, 7*24*time.Hour, false)
	if err != nil {
		t.Fatalf("Purge: %v", err)
	}
	if res.WispsPurged != 2 {
		t.Errorf("WispsPurged = %d, want 2", res.WispsPurged)
	}
	if got := idsOf(w.callsOf("delete")); !reflect.DeepEqual(got, []string{"done-a", "done-b"}) {
		t.Errorf("deleted %v, want [done-a done-b]", got)
	}
}

// TestAutoCloseClosesThroughBdWithoutForce: durable issues close with plain
// bd close and the stale reason; bd's fences are not overridden.
func TestAutoCloseClosesThroughBdWithoutForce(t *testing.T) {
	state := newStaleIssueState("hq-a", "hq-b")
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()

	res, err := AutoClose(db, w, "hq", AutoCloseOptions{StaleAge: MinStaleIssueAge, Force: true})
	if err != nil {
		t.Fatalf("AutoClose: %v", err)
	}
	if res.Closed != 2 {
		t.Errorf("Closed = %d, want 2", res.Closed)
	}
	closes := w.callsOf("close")
	if got := idsOf(closes); !reflect.DeepEqual(got, []string{"hq-a", "hq-b"}) {
		t.Errorf("closed %v, want [hq-a hq-b]", got)
	}
	for _, c := range closes {
		if c.reason != StaleAutoCloseReason {
			t.Errorf("reason %q, want %q", c.reason, StaleAutoCloseReason)
		}
	}
	if n := len(w.callsOf("force-close")); n != 0 {
		t.Errorf("AutoClose force-closed %d batches, want 0", n)
	}
}

// TestCloseInChunksSplitsBatches: a large candidate set reaches bd in
// DefaultBatchSize batches, so one bd argv never carries thousands of ids.
func TestCloseInChunksSplitsBatches(t *testing.T) {
	ids := make([]string, DefaultBatchSize*2+1)
	for i := range ids {
		ids[i] = "w-" + strings.Repeat("x", i%3) + string(rune('a'+i%26))
	}
	var sizes []int
	n, err := closeInChunks(ids, func(chunk ...string) error {
		sizes = append(sizes, len(chunk))
		return nil
	})
	if err != nil || n != len(ids) {
		t.Fatalf("closeInChunks = %d, %v", n, err)
	}
	if !reflect.DeepEqual(sizes, []int{DefaultBatchSize, DefaultBatchSize, 1}) {
		t.Errorf("chunk sizes %v", sizes)
	}
}
