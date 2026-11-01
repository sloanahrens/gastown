package reaper

import (
	"errors"
	"fmt"
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
	// failDelete names ids whose delete chunk bd refuses. bd deletes a chunk
	// all or nothing, so a chunk holding one deletes nothing.
	failDelete map[string]bool
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
		if w.failDelete[id] {
			return fmt.Errorf("bd delete: %s is still referenced", id)
		}
	}
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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

// chunkWriter records the delete batches it receives and fails the batch that
// holds an id in failOn, as bd refuses a chunk all or nothing.
type chunkWriter struct {
	failOn  map[string]bool
	batches [][]string
}

func (w *chunkWriter) CloseWithReason(string, ...string) error      { return nil }
func (w *chunkWriter) ForceCloseWithReason(string, ...string) error { return nil }

func (w *chunkWriter) DeleteIssues(ids ...string) error {
	w.batches = append(w.batches, append([]string(nil), ids...))
	for _, id := range ids {
		if w.failOn[id] {
			return fmt.Errorf("bd delete: %s refused", id)
		}
	}
	return nil
}

// TestDeleteInChunks: a failed chunk keeps the count of the chunks that did
// delete, names the failed chunk's id range and error, and returns the joined
// error (gt-2wesb).
func TestDeleteInChunks(t *testing.T) {
	t.Parallel()
	ids := make([]string, DefaultBatchSize*2+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("w-%03d", i)
	}
	w := &chunkWriter{failOn: map[string]bool{ids[DefaultBatchSize]: true}}

	deleted, _, failed, err := deleteInChunks(w, ids)
	if err == nil {
		t.Fatal("deleteInChunks with a failed chunk = nil error, want the chunk's failure")
	}
	if want := DefaultBatchSize + 1; deleted != want {
		t.Errorf("deleted = %d, want %d (chunks one and three)", deleted, want)
	}
	if len(w.batches) != 3 ||
		!reflect.DeepEqual(w.batches[0], ids[:DefaultBatchSize]) ||
		!reflect.DeepEqual(w.batches[2], ids[2*DefaultBatchSize:]) {
		t.Errorf("batches = %v, want chunks one and three to be the deleted ones", w.batches)
	}
	if len(failed) != 1 {
		t.Fatalf("failed chunks = %+v, want one", failed)
	}
	failedChunk := failed[0]
	if failedChunk.FirstID != ids[DefaultBatchSize] || failedChunk.LastID != ids[2*DefaultBatchSize-1] {
		t.Errorf("failed chunk = %s..%s, want %s..%s",
			failedChunk.FirstID, failedChunk.LastID, ids[DefaultBatchSize], ids[2*DefaultBatchSize-1])
	}
	if !strings.Contains(failedChunk.Error, "refused") {
		t.Errorf("failed chunk error = %q, want the bd error text", failedChunk.Error)
	}
}

// scriptedWriter answers each DeleteIssues call with the next scripted error,
// then succeeds, and records every batch it was handed.
type scriptedWriter struct {
	errors  []error
	batches [][]string
}

func (w *scriptedWriter) CloseWithReason(string, ...string) error      { return nil }
func (w *scriptedWriter) ForceCloseWithReason(string, ...string) error { return nil }

func (w *scriptedWriter) DeleteIssues(ids ...string) error {
	w.batches = append(w.batches, append([]string(nil), ids...))
	if len(w.errors) == 0 {
		return nil
	}
	err := w.errors[0]
	w.errors = w.errors[1:]
	return err
}

// recordingSleep returns the seam deleteInChunksSleeping takes and the delays
// it recorded, so a test reads the backoff instead of waiting it out.
func recordingSleep() (func(time.Duration), *[]time.Duration) {
	waits := &[]time.Duration{}
	return func(d time.Duration) { *waits = append(*waits, d) }, waits
}

// TestDeleteInChunksRetriesATransientFailure: a chunk bd refused with a
// serialization failure twice and then accepted is deleted once, counted once,
// and the waits between the attempts are 1 s and 2 s (gt-ujklm).
func TestDeleteInChunksRetriesATransientFailure(t *testing.T) {
	t.Parallel()
	ids := []string{"w-a", "w-b", "w-c"}
	w := &scriptedWriter{errors: []error{
		errors.New("bd delete: serialization failure: transaction conflict"),
		errors.New("bd delete: serialization failure: transaction conflict"),
	}}
	sleep, waits := recordingSleep()

	deleted, retried, failed, err := deleteInChunksSleeping(w, ids, sleep)
	if err != nil {
		t.Fatalf("deleteInChunksSleeping = %v, want nil after the retry succeeds", err)
	}
	if deleted != len(ids) || retried != 1 || len(failed) != 0 {
		t.Errorf("deleted=%d retried=%d failed=%+v, want %d, 1, none", deleted, retried, failed, len(ids))
	}
	if len(w.batches) != 3 {
		t.Fatalf("DeleteIssues calls = %d, want 3 (two failures then the success)", len(w.batches))
	}
	if !reflect.DeepEqual(*waits, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("waits = %v, want [1s 2s]", *waits)
	}
}

// TestDeleteInChunksDoesNotRetryAPermanentError: a permanent refusal is
// attempted once, waits for nothing, and is reported as a failed chunk
// (gt-ujklm).
func TestDeleteInChunksDoesNotRetryAPermanentError(t *testing.T) {
	t.Parallel()
	ids := []string{"w-a", "w-b"}
	w := &scriptedWriter{errors: []error{errors.New("bd delete: w-a is still referenced")}}
	sleep, waits := recordingSleep()

	deleted, retried, failed, err := deleteInChunksSleeping(w, ids, sleep)
	if err == nil {
		t.Fatal("deleteInChunksSleeping with a permanent error = nil error, want the refusal")
	}
	if len(w.batches) != 1 {
		t.Errorf("DeleteIssues calls = %d, want 1 (a refusal is not retried)", len(w.batches))
	}
	if len(*waits) != 0 {
		t.Errorf("waits = %v, want none for a permanent error", *waits)
	}
	if deleted != 0 || retried != 0 {
		t.Errorf("deleted=%d retried=%d, want 0 and 0", deleted, retried)
	}
	if len(failed) != 1 || failed[0].FirstID != ids[0] {
		t.Errorf("failed chunks = %+v, want the refused chunk", failed)
	}
}

// TestDeleteInChunksGivesUpAfterThreeAttempts: a chunk that fails transiently
// every time is tried three times and reported failed, while the next chunk
// still deletes (gt-ujklm).
func TestDeleteInChunksGivesUpAfterThreeAttempts(t *testing.T) {
	t.Parallel()
	ids := make([]string, DefaultBatchSize*2)
	for i := range ids {
		ids[i] = fmt.Sprintf("w-%03d", i)
	}
	transient := fmt.Errorf("bd delete: lock wait timeout exceeded")
	w := &scriptedWriter{errors: []error{transient, transient, transient}}
	sleep, waits := recordingSleep()

	deleted, retried, failed, err := deleteInChunksSleeping(w, ids, sleep)
	if err == nil {
		t.Fatal("deleteInChunksSleeping with a chunk that never succeeds = nil error, want the failure")
	}
	first := w.batches[0]
	if len(w.batches) != 4 {
		t.Fatalf("DeleteIssues calls = %d, want 4 (three attempts at the first chunk, one at the second)", len(w.batches))
	}
	if !reflect.DeepEqual(w.batches[1], first) || !reflect.DeepEqual(w.batches[2], first) {
		t.Errorf("attempts 2 and 3 were handed %v and %v, want the same chunk", w.batches[1], w.batches[2])
	}
	if want := len(ids) - DefaultBatchSize; deleted != want {
		t.Errorf("deleted = %d, want %d (the chunk that succeeded)", deleted, want)
	}
	if retried != 0 {
		t.Errorf("retried = %d, want 0 (a chunk given up on did not succeed)", retried)
	}
	if len(failed) != 1 || failed[0].FirstID != ids[0] {
		t.Errorf("failed chunks = %+v, want the chunk that never succeeded", failed)
	}
	if !reflect.DeepEqual(*waits, []time.Duration{time.Second, 2 * time.Second}) {
		t.Errorf("waits = %v, want [1s 2s] before giving up", *waits)
	}
}

// TestIsTransientDeleteError is the classification on its own: a serialization
// failure, a lock wait timeout and a connection timeout may be retried, while
// a refusal, a bad id and a nil error may not (gt-ujklm).
func TestIsTransientDeleteError(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "serialization failure", err: errors.New("Error 1213: Deadlock found"), want: true},
		{name: "lock wait timeout", err: errors.New("Error 1205: Lock wait timeout exceeded"), want: true},
		{name: "connection timeout", err: errors.New("read tcp: i/o timeout"), want: true},
		{name: "dropped connection", err: errors.New("invalid connection"), want: true},
		{name: "refused chunk", err: errors.New("bd delete: w-a is still referenced"), want: false},
		{name: "bad id", err: errors.New("no such issue: w-a"), want: false},
		{name: "nil", err: nil, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isTransientDeleteError(tt.err); got != tt.want {
				t.Errorf("isTransientDeleteError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

// TestPurgeKeepsPartialCountOnFailure: a purge whose second delete chunk fails
// returns the count the first chunk deleted together with the error, so the
// caller reports a partial purge rather than nothing (gt-2wesb).
func TestPurgeKeepsPartialCountOnFailure(t *testing.T) {
	t.Parallel()
	old := time.Now().UTC().Add(-30 * 24 * time.Hour)
	ids := make([]string, DefaultBatchSize+1)
	wisps := make(map[string]*fakeWisp, len(ids))
	for i := range ids {
		ids[i] = fmt.Sprintf("done-%03d", i)
		wisps[ids[i]] = &fakeWisp{id: ids[i], status: "closed", issueType: "task", createdAt: old, closedAt: old}
	}
	state := &fakeReaperState{wisps: wisps, ops: map[int][]string{}}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()
	w.failDelete = map[string]bool{ids[DefaultBatchSize]: true}

	res, err := Purge(db, w, "testdb", 7*24*time.Hour, 7*24*time.Hour, false)
	if err == nil {
		t.Fatal("Purge with a failed chunk = nil error, want the failure")
	}
	if res == nil {
		t.Fatal("Purge with a failed chunk returned no result, want what it did delete")
	}
	if res.WispsPurged != DefaultBatchSize {
		t.Errorf("WispsPurged = %d, want %d", res.WispsPurged, DefaultBatchSize)
	}
	if len(res.FailedChunks) != 1 || res.FailedChunks[0].FirstID != ids[DefaultBatchSize] {
		t.Errorf("FailedChunks = %+v, want the chunk starting at %s", res.FailedChunks, ids[DefaultBatchSize])
	}
}

// TestAutoCloseClosesThroughBdWithoutForce: durable issues close with plain
// bd close and the stale reason; bd's fences are not overridden.
func TestAutoCloseClosesThroughBdWithoutForce(t *testing.T) {
	t.Parallel()
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
	t.Parallel()
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

// TestReapStopsAtTheFirstFailedPhase: a refused molecule-step close stops
// the run before the absent-parent and stale phases select, so no id is
// re-selected under another reason and the counts cover only what closed.
func TestReapStopsAtTheFirstFailedPhase(t *testing.T) {
	t.Parallel()
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"mol-closed":  {id: "mol-closed", status: "closed", issueType: "molecule", createdAt: now},
			"step-a":      {id: "step-a", status: "open", issueType: "task", createdAt: old},
			"step-b":      {id: "step-b", status: "open", issueType: "task", createdAt: old},
			"stale-other": {id: "stale-other", status: "open", issueType: "task", createdAt: old},
		},
		deps: []fakeDep{
			{issueID: "step-a", dependsOnID: "mol-closed", depType: "parent-child"},
			{issueID: "step-b", dependsOnID: "mol-closed", depType: "parent-child"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()
	w.refuse = map[string]bool{"step-b": true}

	res, err := Reap(db, w, "testdb", 24*time.Hour, false)
	if !errors.Is(err, beads.ErrCloseRefused) {
		t.Fatalf("Reap error = %v, want the step refusal", err)
	}
	if res.MoleculeStepsClosed != 1 || res.Reaped != 0 {
		t.Errorf("MoleculeStepsClosed=%d Reaped=%d, want 1 and 0", res.MoleculeStepsClosed, res.Reaped)
	}
	if got := idsOf(w.callsOf("force-close")); !reflect.DeepEqual(got, []string{"step-a", "step-b"}) {
		t.Errorf("force-close batches named %v, want only the step phase's [step-a step-b]", got)
	}
	if got := state.status("stale-other"); got != "open" {
		t.Errorf("stale-other = %q after a failed first phase, want open (later phases must not run)", got)
	}
}

// TestReapNeverClosesAnAgentWispInAnyPhase: every candidate query excludes
// issue_type='agent', including the absent-parent one, so bd close --force
// is never handed an agent bead.
func TestReapNeverClosesAnAgentWispInAnyPhase(t *testing.T) {
	t.Parallel()
	old := time.Now().UTC().Add(-48 * time.Hour)
	state := &fakeReaperState{
		wisps: map[string]*fakeWisp{
			"agent-orphan": {id: "agent-orphan", status: "open", issueType: "agent", createdAt: old},
			"task-orphan":  {id: "task-orphan", status: "open", issueType: "task", createdAt: old},
		},
		deps: []fakeDep{
			{issueID: "agent-orphan", dependsOnID: "purged-mol", depType: "parent-child"},
			{issueID: "task-orphan", dependsOnID: "purged-mol", depType: "parent-child"},
		},
		ops: map[int][]string{},
	}
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()

	if _, err := Reap(db, w, "testdb", 24*time.Hour, false); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	for _, id := range idsOf(w.callsOf("force-close")) {
		if id == "agent-orphan" {
			t.Fatal("Reap force-closed an agent wisp")
		}
	}
	if got := state.status("task-orphan"); got != "closed" {
		t.Errorf("task-orphan = %q, want closed (absent parent)", got)
	}
}

// TestAutoCloseReportsBdRefusalWithAnAccurateCount: plain bd close may refuse
// an issue the old UPDATE would have closed. The refusal is returned, and
// Closed counts only what bd closed.
func TestAutoCloseReportsBdRefusalWithAnAccurateCount(t *testing.T) {
	t.Parallel()
	state := newStaleIssueState("hq-a", "hq-b", "hq-c")
	db := openFakeReaperDB(t, state)
	t.Cleanup(func() { _ = db.Close() })
	w := state.writer()
	w.refuse = map[string]bool{"hq-b": true}

	res, err := AutoClose(db, w, "hq", AutoCloseOptions{StaleAge: MinStaleIssueAge, Force: true})
	if !errors.Is(err, beads.ErrCloseRefused) {
		t.Fatalf("AutoClose error = %v, want bd's refusal", err)
	}
	if res == nil || res.Closed != 2 {
		t.Fatalf("AutoClose result = %+v, want Closed 2", res)
	}
	statuses := state.staleIssueStatuses()
	if statuses["hq-b"] != "open" || statuses["hq-a"] != "closed" || statuses["hq-c"] != "closed" {
		t.Errorf("statuses = %v", statuses)
	}
}
