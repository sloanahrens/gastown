package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/procutil"
)

// The bead-detail read starts a bd child on an HTTP goroutine. A restart that
// execs with that child running strands it as defunct (gt-fekq1), so the
// detail read has to join the hub's quiesce. These tests drive a hub under
// procutil.Stoppable, the way gt dashboard does, with a stubbed detail reader
// that blocks until the test lets it go.

const detailNever = 24 * time.Hour

// blockingDetail is a detail reader that parks each call until release.
type blockingDetail struct {
	calls   atomic.Int32
	started chan string // the id of each read as it starts
	release chan struct{}
}

func newBlockingDetail() *blockingDetail {
	return &blockingDetail{started: make(chan string, 8), release: make(chan struct{})}
}

func (b *blockingDetail) read(rig, id string) (*BeadDetail, error) {
	b.calls.Add(1)
	b.started <- id
	<-b.release
	return &BeadDetail{ID: id, Rig: rig, Title: "t"}, nil
}

func getBead(h http.Handler, id string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", "/api/bead?rig=gastown&id="+id, nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// waitFor polls cond until it holds; the conditions here are set by goroutines
// the test started, so there is nothing to select on.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

func inFlightDetail(h *Hub) int {
	h.detailGate.mu.Lock()
	defer h.detailGate.mu.Unlock()
	return h.detailGate.inFlight
}

// Quiesce waits for a detail read in flight, refuses a new one with a retryable
// error that starts no child, and resume serves reads again.
func TestQuiesceWaitsForADetailReadAndRefusesNewOnes(t *testing.T) {
	t.Parallel()
	stub := newBlockingDetail()
	h := NewHub(Config{Bead: stub.read})
	handler := h.Handler()
	run := procutil.StartStoppable(context.Background(), h.Run)

	var first *httptest.ResponseRecorder
	var reads sync.WaitGroup
	reads.Add(1)
	go func() {
		defer reads.Done()
		first = getBead(handler, "gt-1")
	}()
	<-stub.started

	quiesced := make(chan func(), 1)
	go func() {
		quiesced <- run.Quiesce(procutil.QuiesceBounds{Drain: detailNever, Kill: detailNever}, func() int { return 0 }, t.Logf)
	}()

	// Once the gate has closed, a new read is refused without reaching the reader.
	// A probe sent before then would be admitted and queue behind the first read,
	// so the test waits for the close before sending one.
	waitUntil(t, "the gate to close", h.detailGate.isClosed)
	refused := getBead(handler, "gt-2")
	if refused.Code != http.StatusServiceUnavailable {
		t.Errorf("a read during quiesce got %d, want 503", refused.Code)
	}
	if refused.Header().Get("Retry-After") == "" {
		t.Errorf("a refused read must say when to retry: %v", refused.Header())
	}
	if n := stub.calls.Load(); n != 1 {
		t.Errorf("%d reader calls, want 1: a refused read started a child", n)
	}
	select {
	case <-quiesced:
		t.Fatal("Quiesce returned while a detail read was in flight")
	default:
	}

	close(stub.release)
	resume := <-quiesced
	reads.Wait()
	if first.Code != http.StatusOK {
		t.Errorf("the read in flight got %d, want it to finish", first.Code)
	}

	// A failed exec resumes the hub, and detail reads work again.
	// The resumed Run opens the gate on its own goroutine; a read before it does
	// gets the same retryable error, so the test waits for the open.
	resume()
	waitUntil(t, "the resumed hub to open the gate", func() bool { return !h.detailGate.isClosed() })
	if rec := getBead(handler, "gt-3"); rec.Code != http.StatusOK {
		t.Errorf("after resume a detail read got %d, want 200", rec.Code)
	}
	run.Quiesce(procutil.QuiesceBounds{Drain: detailNever, Kill: detailNever}, func() int { return 0 }, t.Logf)
}

// A read still in flight after the drain bound is a straggler: the kill ends
// its child, the read returns, and Quiesce with it.
func TestQuiesceKillsADetailReadThatOutlastsTheDrainBound(t *testing.T) {
	t.Parallel()
	stub := newBlockingDetail()
	h := NewHub(Config{Bead: stub.read})
	handler := h.Handler()
	run := procutil.StartStoppable(context.Background(), h.Run)

	done := make(chan struct{})
	go func() {
		defer close(done)
		getBead(handler, "gt-1")
	}()
	<-stub.started

	var killed atomic.Bool
	run.Quiesce(procutil.QuiesceBounds{Drain: time.Millisecond, Kill: detailNever}, func() int {
		if killed.CompareAndSwap(false, true) {
			close(stub.release) // the kill: the bd child dies and the read returns
		}
		return 1
	}, t.Logf)
	<-done
	if !killed.Load() {
		t.Error("a detail read outlasted the drain bound and was not killed")
	}
}

// A read queued behind another was admitted before the restart began; it must
// not start a child of its own once the restart has.
func TestQuiesceDetailReadQueuedBehindAnotherStartsNoChild(t *testing.T) {
	t.Parallel()
	stub := newBlockingDetail()
	h := NewHub(Config{Bead: stub.read})
	handler := h.Handler()
	run := procutil.StartStoppable(context.Background(), h.Run)

	var reads sync.WaitGroup
	var second *httptest.ResponseRecorder
	reads.Add(2)
	go func() {
		defer reads.Done()
		getBead(handler, "gt-1")
	}()
	<-stub.started
	go func() {
		defer reads.Done()
		second = getBead(handler, "gt-2")
	}()
	waitUntil(t, "the second read to queue behind the first", func() bool { return inFlightDetail(h) == 2 })

	quiesced := make(chan struct{})
	go func() {
		defer close(quiesced)
		run.Quiesce(procutil.QuiesceBounds{Drain: detailNever, Kill: detailNever}, func() int { return 0 }, t.Logf)
	}()
	waitUntil(t, "the gate to close", h.detailGate.isClosed)
	close(stub.release)
	<-quiesced
	reads.Wait()

	if n := stub.calls.Load(); n != 1 {
		t.Errorf("%d reader calls, want 1: the queued read started a child during the restart", n)
	}
	if second.Code != http.StatusServiceUnavailable {
		t.Errorf("the queued read got %d, want 503", second.Code)
	}
}
