package registry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

// recordingRegistry serves DELETE requests and records each ref it received, so
// a test can see exactly which deletes a run sent, in what order, and can make
// chosen ones fail.
type recordingRegistry struct {
	mu      sync.Mutex
	deletes []string
	status  map[string]int // "repo@digest" -> the status to answer with
}

func newRecordingRegistry(status map[string]int) *recordingRegistry {
	return &recordingRegistry{status: status}
}

func (r *recordingRegistry) handler(w http.ResponseWriter, req *http.Request) {
	repo, digest, ok := splitManifestPath(req.URL.Path)
	if req.Method != http.MethodDelete || !ok {
		http.NotFound(w, req)
		return
	}
	ref := repo + "@" + digest
	r.mu.Lock()
	r.deletes = append(r.deletes, ref)
	code := r.status[ref]
	r.mu.Unlock()
	if code != 0 {
		w.WriteHeader(code)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// deleted returns the refs the run deleted, in the order it sent them.
func (r *recordingRegistry) deleted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.deletes)
}

// stepLog records the order one run made its calls in. The registry handler
// runs on the server's goroutine, so every append takes the same mutex.
type stepLog struct {
	mu    sync.Mutex
	steps []string
}

func (l *stepLog) add(step string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.steps = append(l.steps, step)
}

func (l *stepLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.steps)
}

// applyConfig wires a target to reg with a ledger at ledgerPath.
func applyConfig(t *testing.T, reg *recordingRegistry, ledgerPath string) Config {
	t.Helper()
	return Config{
		Base:   "http://registry.test",
		HTTP:   &http.Client{Transport: roundTrip(reg.handler)},
		Ledger: diskmaint.NewLedger(ledgerPath, 1<<20),
	}
}

// deletePlan is a plan that deletes refs; the nth action holds n×100 bytes.
func deletePlan(refs ...string) diskmaint.Plan {
	plan := diskmaint.Plan{Target: planTarget}
	for i, ref := range refs {
		plan.Actions = append(plan.Actions, diskmaint.Action{Kind: actionDelete, Ref: ref, Bytes: int64(100 * (i + 1))})
	}
	return plan
}

// ledgerPath returns a path for a test's ledger inside its temp directory.
func ledgerPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "ledger.jsonl")
}

// readLedger returns the ledger's entries, failing the test unless the apply
// wrote exactly one line.
func readLedger(t *testing.T, path string) []diskmaint.LedgerEntry {
	t.Helper()
	entries, err := diskmaint.NewLedger(path, 1<<20).Tail(10)
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("ledger holds %d entries, want one line per apply: %+v", len(entries), entries)
	}
	return entries
}

// mentions reports whether one of lines contains substr.
func mentions(lines []string, substr string) bool {
	for _, line := range lines {
		if strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func TestApplyDeletesEachManifest(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	first, second := RepoMain+"@"+dg("a1"), "seedtest/polecat@"+dg("b2")

	result, err := NewTarget(applyConfig(t, reg, ledgerPath(t))).Apply(context.Background(), deletePlan(first, second))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, want := reg.deleted(), []string{first, second}; !slices.Equal(got, want) {
		t.Fatalf("deletes = %v, want %v", got, want)
	}
	if len(result.Done) != 2 || len(result.Failed) != 0 {
		t.Fatalf("done = %d, failed = %d; want 2 and 0", len(result.Done), len(result.Failed))
	}
	if result.BytesFreed != 300 {
		t.Fatalf("bytes freed = %d, want 300 (successful actions only)", result.BytesFreed)
	}
}

func TestApplyContinuesAfterOneDeleteFails(t *testing.T) {
	t.Parallel()
	first, failing, last := RepoMain+"@"+dg("a1"), RepoMain+"@"+dg("a2"), RepoMain+"@"+dg("a3")
	reg := newRecordingRegistry(map[string]int{failing: http.StatusInternalServerError})
	path := ledgerPath(t)

	result, err := NewTarget(applyConfig(t, reg, path)).Apply(context.Background(), deletePlan(first, failing, last))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got, want := reg.deleted(), []string{first, failing, last}; !slices.Equal(got, want) {
		t.Fatalf("deletes = %v, want all three attempted in order %v", got, want)
	}
	if len(result.Done) != 2 || result.Done[1].Ref != last {
		t.Fatalf("done = %+v, want the first and last action", result.Done)
	}
	if len(result.Failed) != 1 || result.Failed[0].Action.Ref != failing {
		t.Fatalf("failed = %+v, want the middle action", result.Failed)
	}
	if result.BytesFreed != 400 {
		t.Fatalf("bytes freed = %d, want 400 (100+300, not the failure)", result.BytesFreed)
	}
	if entry := readLedger(t, path)[0]; entry.Done != 2 || !mentions(entry.Errors, failing) {
		t.Fatalf("ledger = %+v, want done 2 and the failure in errors", entry)
	}
}

func TestApplyDeletesDisabledStopsAtFirstDelete(t *testing.T) {
	t.Parallel()
	disabled := RepoMain + "@" + dg("a1")
	reg := newRecordingRegistry(map[string]int{disabled: http.StatusMethodNotAllowed})
	path := ledgerPath(t)

	result, err := NewTarget(applyConfig(t, reg, path)).Apply(context.Background(), deletePlan(disabled, RepoMain+"@"+dg("a2"), RepoMain+"@"+dg("a3")))
	if !errors.Is(err, ErrDeletesDisabled) {
		t.Fatalf("error = %v, want ErrDeletesDisabled", err)
	}
	if got, want := reg.deleted(), []string{disabled}; !slices.Equal(got, want) {
		t.Fatalf("deletes = %v, want only the first %v", got, want)
	}
	if len(result.Done) != 0 || len(result.Failed) != 1 {
		t.Fatalf("done = %d, failed = %d; want 0 and 1", len(result.Done), len(result.Failed))
	}
	entry := readLedger(t, path)[0]
	if entry.Planned != 3 || entry.Done != 0 {
		t.Fatalf("ledger planned/done = %d/%d, want 3/0", entry.Planned, entry.Done)
	}
	if !mentions(entry.Errors, "2 of 3 actions not attempted") {
		t.Fatalf("ledger errors = %v, want the count of actions not attempted", entry.Errors)
	}
}

func TestApplyRunsInsideTheLock(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	steps := &stepLog{}
	cfg := applyConfig(t, reg, ledgerPath(t))
	cfg.HTTP = &http.Client{Transport: roundTrip(func(w http.ResponseWriter, r *http.Request) {
		steps.add("delete")
		reg.handler(w, r)
	})}
	cfg.Lock = func(ctx context.Context, fn func(context.Context) error) error {
		steps.add("lock acquire")
		defer steps.add("lock release")
		return fn(ctx)
	}

	if _, err := NewTarget(cfg).Apply(context.Background(), deletePlan(RepoMain+"@"+dg("a1"), RepoMain+"@"+dg("a2"))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	want := []string{"lock acquire", "delete", "delete", "lock release"}
	if got := steps.all(); !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want the deletes between acquire and release %v", got, want)
	}
}

func TestApplyDefersWhenLockHeld(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)
	cfg := applyConfig(t, reg, path)
	cfg.Lock = func(context.Context, func(context.Context) error) error {
		return fmt.Errorf("polecat-image lock: %w", ErrDeferred)
	}

	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(RepoMain+"@"+dg("a1")))
	if err != nil {
		t.Fatalf("apply: %v; a held lock defers and is not a failure", err)
	}
	if result.Deferred == "" {
		t.Fatal("Result.Deferred is empty; want the reason the apply deferred")
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none", got)
	}
	entry := readLedger(t, path)[0]
	if entry.Deferred == "" || entry.Done != 0 || entry.Planned != 1 {
		t.Fatalf("ledger = %+v, want planned 1, done 0 and the deferral", entry)
	}
}

func TestApplyWithNilLockRunsDirectly(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	cfg := applyConfig(t, reg, ledgerPath(t))

	ref := RepoMain + "@" + dg("a1")
	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(ref))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := reg.deleted(); !slices.Equal(got, []string{ref}) {
		t.Fatalf("deletes = %v, want %v", got, ref)
	}
	if len(result.Done) != 1 {
		t.Fatalf("done = %+v, want the one action", result.Done)
	}
}

func TestApplyWritesALedgerLine(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)

	if _, err := NewTarget(applyConfig(t, reg, path)).Apply(context.Background(), deletePlan(RepoMain+"@"+dg("a1"))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	entry := readLedger(t, path)[0]
	if entry.Target != planTarget || entry.Mode != "apply" {
		t.Fatalf("ledger target/mode = %q/%q, want %q/apply", entry.Target, entry.Mode, planTarget)
	}
	if entry.TS.IsZero() {
		t.Fatal("ledger ts is zero")
	}
	if entry.Planned != 1 || entry.Done != 1 || entry.BytesFreed != 100 {
		t.Fatalf("ledger = %+v, want planned 1, done 1, 100 bytes", entry)
	}
}

func TestApplyOfRefusedPlanDoesNothing(t *testing.T) {
	t.Parallel()
	// The second plan also carries an action: a refusal is a decision not to
	// act, so nothing in such a plan is applied.
	cases := []struct {
		name    string
		actions []diskmaint.Action
	}{
		{"empty", nil},
		{"with an action", deletePlan(RepoMain + "@" + dg("a1")).Actions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newRecordingRegistry(nil)
			path := ledgerPath(t)
			cfg := applyConfig(t, reg, path)
			cfg.Lock = func(context.Context, func(context.Context) error) error {
				t.Error("a refused plan took the lock; it has nothing to protect")
				return nil
			}
			plan := diskmaint.Plan{Target: planTarget, Actions: tc.actions, Refused: "read failed: catalog: connection refused"}

			result, err := NewTarget(cfg).Apply(context.Background(), plan)
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got := reg.deleted(); len(got) != 0 {
				t.Fatalf("deletes = %v, want none", got)
			}
			if len(result.Done) != 0 || result.Deferred != "" {
				t.Fatalf("result = %+v, want an untouched result", result)
			}
			entry := readLedger(t, path)[0]
			if entry.Refused != plan.Refused || entry.Planned != len(tc.actions) || entry.Done != 0 {
				t.Fatalf("ledger = %+v, want the refusal recorded with nothing done", entry)
			}
		})
	}
}

func TestApplyOfEmptyPlanRecordsIt(t *testing.T) {
	t.Parallel()
	path := ledgerPath(t)
	reg := newRecordingRegistry(nil)

	if _, err := NewTarget(applyConfig(t, reg, path)).Apply(context.Background(), diskmaint.Plan{Target: planTarget}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none", got)
	}
	if entry := readLedger(t, path)[0]; entry.Planned != 0 || entry.Done != 0 {
		t.Fatalf("ledger = %+v, want a line for the run with nothing planned", entry)
	}
}

func TestApplyRejectsAnActionRefWithABadName(t *testing.T) {
	t.Parallel()
	good := RepoMain + "@" + dg("a1")
	cases := []struct{ name, ref string }{
		{"bad repo", "../x@" + dg("a2")},
		{"bad digest", RepoMain + "@sha256:abc"},
		{"no separator", RepoMain + dg("a3")},
		{"empty ref", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newRecordingRegistry(nil)
			path := ledgerPath(t)

			result, err := NewTarget(applyConfig(t, reg, path)).Apply(context.Background(), deletePlan(good, tc.ref))
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if got, want := reg.deleted(), []string{good}; !slices.Equal(got, want) {
				t.Fatalf("deletes = %v, want only the valid action %v", got, want)
			}
			if len(result.Failed) != 1 || result.Failed[0].Action.Ref != tc.ref {
				t.Fatalf("failed = %+v, want the bad ref", result.Failed)
			}
			failure := result.Failed[0].Err
			if !mentions([]string{failure}, "failed validation") {
				t.Fatalf("failure = %q, want a validation error", failure)
			}
			// The split validation names the whole ref, so a ref the client
			// itself refused is distinguishable from one this target rejected
			// before building a request.
			if tc.ref != "" && !mentions([]string{failure}, tc.ref) {
				t.Fatalf("failure = %q, want it to name the whole rejected ref %q", failure, tc.ref)
			}
			entry := readLedger(t, path)[0]
			if len(entry.Errors) == 0 || (tc.ref != "" && !mentions(entry.Errors, tc.ref)) {
				t.Fatalf("ledger errors = %v, want the bad ref recorded", entry.Errors)
			}
		})
	}
}

func TestApplyRejectsAnUnknownActionKind(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	plan := diskmaint.Plan{Target: planTarget, Actions: []diskmaint.Action{
		{Kind: "prune-tag", Ref: RepoMain + "@" + dg("a1"), Bytes: 100},
	}}

	result, err := NewTarget(applyConfig(t, reg, ledgerPath(t))).Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none for a kind this target does not run", got)
	}
	if len(result.Failed) != 1 || !mentions([]string{result.Failed[0].Err}, "prune-tag") {
		t.Fatalf("failed = %+v, want the unknown kind recorded", result.Failed)
	}
}

func TestApplyRecordsTheDeletesWhenTheLockFailsAfterTheBody(t *testing.T) {
	t.Parallel()
	// A lock whose error comes after the body ran; the second also wraps the
	// deferral sentinel, which a body that ran contradicts, so the deletes
	// stand and the run is not called deferred.
	cases := []struct {
		name    string
		lockErr error
	}{
		{"release failure", errors.New("release the plugin lock: busy")},
		{"deferral after the body", fmt.Errorf("release the plugin lock: %w", ErrDeferred)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newRecordingRegistry(map[string]int{RepoMain + "@" + dg("a2"): http.StatusInternalServerError})
			path := ledgerPath(t)
			cfg := applyConfig(t, reg, path)
			cfg.Lock = func(ctx context.Context, fn func(context.Context) error) error {
				if err := fn(ctx); err != nil {
					return err
				}
				return tc.lockErr
			}

			result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(RepoMain+"@"+dg("a1"), RepoMain+"@"+dg("a2")))
			if err == nil {
				t.Fatal("apply returned no error; the lock failed")
			}
			if len(result.Done) != 1 || len(result.Failed) != 1 {
				t.Fatalf("done = %d, failed = %d; want the run's outcome kept", len(result.Done), len(result.Failed))
			}
			if result.Deferred != "" {
				t.Fatalf("result deferred = %q; the body ran, so the run was not deferred", result.Deferred)
			}
			entry := readLedger(t, path)[0]
			if entry.Done != 1 {
				t.Fatalf("ledger done = %d, want 1: the delete ran before the lock failed", entry.Done)
			}
			if !mentions(entry.Errors, RepoMain+"@"+dg("a2")) {
				t.Fatalf("ledger errors = %v, want the failed delete kept beside the lock error", entry.Errors)
			}
			if !mentions(entry.Errors, "lock: ") {
				t.Fatalf("ledger errors = %v, want the lock error recorded too", entry.Errors)
			}
		})
	}
}

func TestApplyReportsALockThatNeverRunsTheBody(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	path := ledgerPath(t)
	cfg := applyConfig(t, reg, path)
	cfg.Lock = func(context.Context, func(context.Context) error) error { return nil }

	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(RepoMain+"@"+dg("a1")))
	if !errors.Is(err, errNotRun) {
		t.Fatalf("error = %v, want errNotRun: the run deleted nothing", err)
	}
	if len(result.Done) != 0 {
		t.Fatalf("done = %+v, want nothing done", result.Done)
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none", got)
	}
	if entry := readLedger(t, path)[0]; !mentions(entry.Errors, "without running the deletes") {
		t.Fatalf("ledger errors = %v, want the run that never ran recorded", entry.Errors)
	}
}

func TestApplyRefusesWithoutALedger(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	cfg := applyConfig(t, reg, ledgerPath(t))
	cfg.Ledger = nil

	if _, err := NewTarget(cfg).Apply(context.Background(), deletePlan(RepoMain+"@"+dg("a1"))); !errors.Is(err, errNoLedger) {
		t.Fatalf("error = %v, want errNoLedger", err)
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v; an apply that cannot be recorded must not start", got)
	}
}

func TestApplyReportsALedgerThatCannotBeWritten(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	// A directory where the ledger file goes fails the append however the
	// ledger opens its file, so the test does not depend on that detail.
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := NewTarget(applyConfig(t, reg, path)).Apply(context.Background(), deletePlan(RepoMain+"@"+dg("a1")))
	if err == nil {
		t.Fatal("apply returned no error; the ledger line could not be written")
	}
	if !mentions([]string{err.Error()}, "record the apply") {
		t.Fatalf("error = %v, want it to name the ledger it could not write", err)
	}
	if len(result.Done) != 1 {
		t.Fatalf("done = %+v, want the delete the run made", result.Done)
	}
}
