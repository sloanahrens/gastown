package daemon

import (
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/testutil"
)

// newCacheTrimParkDaemon is the daemon the park tests use: a private temp town
// (the release request lives under its .runtime) and a logger the test reads.
func newCacheTrimParkDaemon(t *testing.T, logs io.Writer) *Daemon {
	t.Helper()
	return &Daemon{config: &Config{TownRoot: testutil.PrivateTempDir(t)}, logger: log.New(logs, "", 0)}
}

// writeCacheTrimUnparkRequest writes the release request a person's command
// would leave for the claim.
func writeCacheTrimUnparkRequest(t *testing.T, d *Daemon) string {
	t.Helper()
	path := cacheTrimUnparkPath(d.config.TownRoot, cacheTrimNamespace, cacheTrimClaimName)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("create the request directory: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatalf("write the request %s: %v", path, err)
	}
	return path
}

// TestCacheTrimParkRaisesAndParks is the bead's box: a failed trim's claim is
// parked, the store holds one item under the failure key with the cause's first
// line and PARKED and the failure's kind and severity, the log names the failed
// stage, and cacheTrimParked reports the failed claim parked and another claim,
// and another namespace, not.
func TestCacheTrimParkRaisesAndParks(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	d := newCacheTrimParkDaemon(t, &logs)
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
	d := newCacheTrimParkDaemon(t, &logs)

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
	d := newCacheTrimParkDaemon(t, io.Discard)
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

// TestCacheTrimUnparkReleasesAParkedClaim is the bead's box: a parked claim
// with a request is released once. The file is gone, the failure item cleared,
// one line names the release, and the claim reports not parked, so the pass
// goes on to trim; the next read stays not parked.
func TestCacheTrimUnparkReleasesAParkedClaim(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	d := newCacheTrimParkDaemon(t, &logs)
	d.cacheTrimPark(cacheTrimNamespace, cacheTrimClaimName, errors.New("applying the trim Job: the server refused"))
	path := writeCacheTrimUnparkRequest(t, d)

	if d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Fatal("the claim is still parked after its release request")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the request %s was not removed: err = %v", path, err)
	}
	if items := d.cacheTrimNoOp.observed(); len(items) != 0 {
		t.Errorf("the store still holds the failure item: %+v", items)
	}
	if got := logs.String(); !strings.Contains(got, "released") {
		t.Errorf("the log %q does not name the release", got)
	}
	if d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Error("the claim is parked again on the next read")
	}
}

// TestCacheTrimUnparkKeepsTheParkWhenTheRequestSurvives pins the refusals: no
// request, one that cannot be removed (a non-empty directory at its path) and
// one that cannot be read (a regular file where its directory belongs, so stat
// fails with ENOTDIR) each leave the claim parked with its failure item, and
// the last two log why.
func TestCacheTrimUnparkKeepsTheParkWhenTheRequestSurvives(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		prepare func(t *testing.T, path string)
		wantLog string
	}{
		{name: "no request"},
		{name: "cannot be removed", wantLog: "could not be removed", prepare: func(t *testing.T, path string) {
			if err := os.MkdirAll(filepath.Join(path, "held"), 0o755); err != nil {
				t.Fatalf("make the request path a non-empty directory: %v", err)
			}
		}},
		{name: "cannot be read", wantLog: "could not be read", prepare: func(t *testing.T, path string) {
			dir := filepath.Dir(path)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatalf("create the request directory: %v", err)
			}
			if err := os.Remove(dir); err != nil {
				t.Fatalf("remove the request directory: %v", err)
			}
			if err := os.WriteFile(dir, nil, 0o644); err != nil {
				t.Fatalf("put a file where the request directory belongs: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var logs strings.Builder
			d := newCacheTrimParkDaemon(t, &logs)
			d.cacheTrimPark(cacheTrimNamespace, cacheTrimClaimName, errors.New("applying the trim Job: the server refused"))
			path := cacheTrimUnparkPath(d.config.TownRoot, cacheTrimNamespace, cacheTrimClaimName)
			if tc.prepare != nil {
				tc.prepare(t, path)
			}
			if !d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
				t.Fatalf("the claim was released by %s", tc.name)
			}
			if items := d.cacheTrimNoOp.observed(); len(items) != 1 {
				t.Errorf("the store holds %d items, want the failure: %+v", len(items), items)
			}
			if tc.wantLog != "" && !strings.Contains(logs.String(), tc.wantLog) {
				t.Errorf("the log %q does not say %q", logs.String(), tc.wantLog)
			}
		})
	}
}

// TestCacheTrimUnparkRequestForAnUnparkedClaimIsRemoved pins that a request for
// a claim that is not parked is removed and logged, so it cannot release a park
// raised after it: the later failure still reports parked.
func TestCacheTrimUnparkRequestForAnUnparkedClaimIsRemoved(t *testing.T) {
	t.Parallel()
	var logs strings.Builder
	d := newCacheTrimParkDaemon(t, &logs)
	path := writeCacheTrimUnparkRequest(t, d)

	if d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Fatal("a claim that never failed is reported parked")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the stray request %s was not removed: err = %v", path, err)
	}
	if got := logs.String(); !strings.Contains(got, "not parked") {
		t.Errorf("the log %q does not say the request named a claim that is not parked", got)
	}
	d.cacheTrimPark(cacheTrimNamespace, cacheTrimClaimName, errors.New("applying the trim Job: the server refused"))
	if !d.cacheTrimParked(cacheTrimNamespace, cacheTrimClaimName) {
		t.Error("the failure after the stray request is not parked")
	}
}
