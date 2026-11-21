package daemon

import (
	"errors"
	"io"
	"log"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
)

// TestCacheTrimParkRaisesAndParks is the bead's box: a failed trim's claim is
// parked, the store holds one item under the failure key with the cause's first
// line and PARKED and the failure's kind and severity, the log names the failed
// stage, and cacheTrimParked reports the failed claim parked and another claim,
// and another namespace, not.
func TestCacheTrimParkRaisesAndParks(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	d := &Daemon{logger: log.New(&logs, "", 0)}
	cause := errors.New("applying the trim Job gt-shared-cache-trim: the server refused\n  status 403\n  more text")

	if d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Fatal("the claim is parked before any failure")
	}

	d.cacheTrimPark(cacheTrimNamespace, cacheTrimClaimName, cause)

	if !d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Error("the failed claim is not parked")
	}
	if d.cacheTrimParked(cacheTrimNamespace, "gt-other-cache") {
		t.Error("a claim whose trim did not fail is parked")
	}
	if d.cacheTrimParked("other-namespace", cacheTrimClaimName) {
		t.Error("the claim is parked in another namespace")
	}

	items := d.cacheTrimNoOp.observed()
	if len(items) != 1 {
		t.Fatalf("the store holds %d items, want the failure: %+v", len(items), items)
	}
	it := items[0]
	if it.Kind != attention.KindCacheTrimNoOp {
		t.Errorf("item kind = %q, want %q: the kind is what serves it to the queue", it.Kind, attention.KindCacheTrimNoOp)
	}
	if it.Severity != attention.SeverityMedium {
		t.Errorf("item severity = %q, want %q: the severity decides whether the operator is woken", it.Severity, attention.SeverityMedium)
	}
	if want := "cache-trim-failed:" + cacheTrimNamespace + "/" + cacheTrimClaimName; it.Key != want {
		t.Errorf("item key = %q, want %q", it.Key, want)
	}
	for _, want := range []string{
		"applying the trim Job gt-shared-cache-trim: the server refused",
		"PARKED until the daemon restarts",
	} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("item summary %q does not name %q", it.Summary, want)
		}
	}
	if strings.Contains(it.Summary, "status 403") {
		t.Errorf("item summary %q carries the cause past its first line", it.Summary)
	}
	if got := logs.String(); !strings.Contains(got, "applying the trim Job") || !strings.Contains(got, "PARKED until the daemon restarts") {
		t.Errorf("the log %q does not name the failed stage and the park", got)
	}
}

// TestCacheTrimParkWithoutACauseStillParks pins that a nil cause parks the
// claim and is named as unknown rather than panicking or reading as a clean
// run: the failure still fails loud.
func TestCacheTrimParkWithoutACauseStillParks(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	d := &Daemon{logger: log.New(&logs, "", 0)}

	d.cacheTrimPark(cacheTrimNamespace, cacheTrimClaimName, nil)

	if !d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Error("the claim is not parked when the cause is nil")
	}
	items := d.cacheTrimNoOp.observed()
	if len(items) != 1 {
		t.Fatalf("the store holds %d items, want the failure: %+v", len(items), items)
	}
	if !strings.Contains(items[0].Summary, "the failure named no cause") {
		t.Errorf("item summary %q does not name the missing cause", items[0].Summary)
	}
}

// TestCacheTrimParkRaces pins that the park map is safe to write and read from
// two goroutines: eight goroutines park and read the same claim, which -race
// reports as a data race when the lock around the map is removed.
func TestCacheTrimParkRaces(t *testing.T) {
	t.Parallel()
	d := &Daemon{logger: log.New(io.Discard, "", 0)}
	cause := errors.New("applying the trim Job gt-shared-cache-trim: the server refused")
	const goroutines = 8
	const rounds = 16
	var wg sync.WaitGroup
	start := make(chan struct{})
	for range goroutines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range rounds {
				d.cacheTrimPark(cacheTrimNamespace, cacheTrimClaimName, cause)
				if !d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
					t.Error("the claim is not parked after cacheTrimPark")
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
}
