package cmd

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/update"
)

const (
	rollbackOldCommit = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	rollbackNewCommit = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// rollbackTestSwap is the daemon a rollback moves. It records each pair it is
// put on, and reports the commit of the last one as the build it runs.
type rollbackTestSwap struct {
	running string
	applied []update.ImagePair
}

func (s *rollbackTestSwap) Apply(_ context.Context, pair update.ImagePair) error {
	s.applied = append(s.applied, pair)
	s.running = pair.Commit
	return nil
}

func (s *rollbackTestSwap) GTVersion(context.Context) (string, error)  { return "", nil }
func (s *rollbackTestSwap) ImageLabel(context.Context) (string, error) { return "", nil }
func (s *rollbackTestSwap) Health(context.Context) (string, error)     { return "", nil }

// DaemonVersion is the commit the daemon reports, the probe the wait reads.
func (s *rollbackTestSwap) DaemonVersion(context.Context) (string, error) { return s.running, nil }

// rollbackTestClock is a clock the fake sleep advances, so no test waits.
type rollbackTestClock struct{ now time.Time }

func (c *rollbackTestClock) Now() time.Time { return c.now }

func (c *rollbackTestClock) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.now = c.now.Add(d)
	return nil
}

// rollbackRegistry serves the manifests it holds, keyed by image name and
// digest, and answers 404 for any other digest.
type rollbackRegistry struct {
	mu   sync.Mutex
	held map[string][]byte
}

func (r *rollbackRegistry) hold(image, digest string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.held[image+"@"+digest] = body
}

func (r *rollbackRegistry) serveHTTP(w http.ResponseWriter, req *http.Request) {
	name, endpoint, ref, ok := splitRegistryPath(req.URL.Path)
	r.mu.Lock()
	body, found := r.held[name+"@"+ref]
	r.mu.Unlock()
	if !ok || endpoint != "manifests" || !found {
		http.NotFound(w, req)
		return
	}
	w.Header().Set("Docker-Content-Digest", ref)
	w.Header().Set("Content-Type", "application/vnd.oci.image.manifest.v1+json")
	_, _ = w.Write(body)
}

// rollbackManifest is the manifest body a fixture image serves. Its digest is
// the hash of these bytes, as a registry's is.
func rollbackManifest(image string) []byte {
	return []byte(`{"schemaVersion":2,"image":"` + image + `"}`)
}

// imageRollbackFixture is one town with a previous pair the registry may or may not
// hold, and a running pair the registry never holds. The daemon is a fake that
// reports the commit it was last put on.
type imageRollbackFixture struct {
	townRoot string
	swap     *rollbackTestSwap
	notes    *notifyfake.Recorder
	registry imagevalues.Registry
	server   *httptest.Server
	// previous is the pair a rollback restores; running is the pair the daemon
	// runs. Neither is recorded until the test calls record.
	previous, running update.ImagePair
}

// newImageRollbackFixture makes the town and its registry. servePrevious decides
// whether the registry holds the previous pair's digests.
func newImageRollbackFixture(t *testing.T, servePrevious bool) *imageRollbackFixture {
	t.Helper()
	registry := &rollbackRegistry{held: map[string][]byte{}}
	//testpolicy:allow no-network — the rollback's registry check reads digests from an httptest server: it answers in-process on loopback, and the command's only client is the one this test wires to it
	server := httptest.NewServer(http.HandlerFunc(registry.serveHTTP))
	t.Cleanup(server.Close)
	address := testRegistryFor(t, server)

	prevDaemon := testDigestOf(rollbackManifest(imagevalues.DaemonImage))
	prevPolecat := testDigestOf(rollbackManifest(imagevalues.PolecatImage))
	if servePrevious {
		registry.hold(imagevalues.DaemonImage, prevDaemon, rollbackManifest(imagevalues.DaemonImage))
		registry.hold(imagevalues.PolecatImage, prevPolecat, rollbackManifest(imagevalues.PolecatImage))
	}
	return &imageRollbackFixture{
		townRoot: t.TempDir(),
		swap:     &rollbackTestSwap{running: rollbackNewCommit},
		notes:    notifyfake.New(),
		registry: address,
		server:   server,
		previous: update.ImagePair{
			Daemon:  address.Ref(imagevalues.DaemonImage, prevDaemon),
			Polecat: address.Ref(imagevalues.PolecatImage, prevPolecat),
			Commit:  rollbackOldCommit,
		},
		running: update.ImagePair{
			Daemon:  address.Ref(imagevalues.DaemonImage, testDigestOf([]byte("running daemon"))),
			Polecat: address.Ref(imagevalues.PolecatImage, testDigestOf([]byte("running polecat"))),
			Commit:  rollbackNewCommit,
		},
	}
}

// record writes rec as the town's ledger.
func (f *imageRollbackFixture) record(t *testing.T, rec update.ImageRecord) {
	t.Helper()
	if err := (update.FileImageLedger{Dir: update.Dir(f.townRoot)}).Write(rec); err != nil {
		t.Fatalf("record the pairs: %v", err)
	}
}

// ledgerNow reads the recorded pairs back from the town.
func (f *imageRollbackFixture) ledgerNow(t *testing.T) update.ImageRecord {
	t.Helper()
	rec, err := update.FileImageLedger{Dir: update.Dir(f.townRoot)}.Read()
	if err != nil {
		t.Fatalf("read the ledger: %v", err)
	}
	return rec
}

// samePairs reports whether two records name the same current and previous
// pairs. The ledger's version is the file's own and is not compared.
func samePairs(a, b update.ImageRecord) bool {
	return a.Current == b.Current && a.Previous == b.Previous
}

// runRollback runs gt update rollback over the fixture's seams.
func (f *imageRollbackFixture) runRollback(t *testing.T) (string, error) {
	t.Helper()
	return f.runRollbackWith(t, nil)
}

// runRollbackWith runs gt update rollback with mutate applied to the seams, so a
// test can unwire or break one of them.
func (f *imageRollbackFixture) runRollbackWith(t *testing.T, mutate func(*updateSeams)) (string, error) {
	t.Helper()
	clock := &rollbackTestClock{now: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}
	seams := updateSeams{
		FindTownRoot: func() (string, error) { return f.townRoot, nil },
		DepsFactory: func(string) (update.Deps, error) {
			return update.Deps{Clock: clock, Sleep: clock.sleep, Verify: f.swap}, nil
		},
		Images: updateImagesSeams{
			Registry:    func(string) (imagevalues.Registry, error) { return f.registry, nil },
			Credentials: func() (string, string, error) { return testUpdateImagesUser, testUpdateImagesToken, nil },
			HTTPClient:  f.server.Client(),
		},
		ImageSwap: func(string) (update.ImageSwap, error) { return f.swap, nil },
		Notifier:  func(string) notify.Notifier { return f.notes },
	}
	if mutate != nil {
		mutate(&seams)
	}
	c := newUpdateCmdWith(seams)
	var out, errOut bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs([]string{"rollback"})
	err := c.Execute()
	return out.String(), err
}

func TestUpdateRollbackRestoresThePair(t *testing.T) {
	t.Parallel()

	t.Run("both recorded digests are put back and recorded as running", func(t *testing.T) {
		t.Parallel()
		f := newImageRollbackFixture(t, true)
		f.record(t, update.ImageRecord{Current: f.running, Previous: f.previous})
		out, err := f.runRollback(t)
		if err != nil {
			t.Fatalf("gt update rollback: %v", err)
		}
		if len(f.swap.applied) != 1 || f.swap.applied[0] != f.previous {
			t.Fatalf("the daemon was put on %+v, want exactly the recorded previous pair %+v", f.swap.applied, f.previous)
		}
		if got := f.ledgerNow(t); !samePairs(got, update.ImageRecord{Current: f.previous, Previous: f.previous}) {
			t.Errorf("ledger after the rollback = %+v, want the restored pair as both current and previous", got)
		}
		if !strings.Contains(out, f.previous.Daemon) || !strings.Contains(out, f.previous.Polecat) {
			t.Errorf("output %q does not name both restored images", out)
		}
	})

	t.Run("nothing recorded is refused with nothing changed", func(t *testing.T) {
		t.Parallel()
		f := newImageRollbackFixture(t, true)
		f.record(t, update.ImageRecord{})
		_, err := f.runRollback(t)
		if !errors.Is(err, update.ErrNothingRecorded) {
			t.Fatalf("gt update rollback error = %v, want ErrNothingRecorded", err)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("the daemon was put on %+v, want no swap", f.swap.applied)
		}
	})

	t.Run("a recorded digest the daemon already runs is refused with nothing changed", func(t *testing.T) {
		t.Parallel()
		f := newImageRollbackFixture(t, true)
		f.record(t, update.ImageRecord{Current: f.previous, Previous: f.previous})
		_, err := f.runRollback(t)
		if !errors.Is(err, update.ErrAlreadyRunning) {
			t.Fatalf("gt update rollback error = %v, want ErrAlreadyRunning", err)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("the daemon was put on %+v, want no swap", f.swap.applied)
		}
	})

	t.Run("a registry that does not hold the recorded digest is refused with nothing changed", func(t *testing.T) {
		t.Parallel()
		f := newImageRollbackFixture(t, false)
		rec := update.ImageRecord{Current: f.running, Previous: f.previous}
		f.record(t, rec)
		_, err := f.runRollback(t)
		if err == nil || !strings.Contains(err.Error(), "has no digest") {
			t.Fatalf("gt update rollback error = %v, want the registry refusal", err)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("the daemon was put on %+v, want no swap", f.swap.applied)
		}
		if !samePairs(f.ledgerNow(t), rec) {
			t.Errorf("ledger changed to %+v, want %+v", f.ledgerNow(t), rec)
		}
	})

	t.Run("a recorded ref that is not the town's image is refused with nothing changed", func(t *testing.T) {
		t.Parallel()
		f := newImageRollbackFixture(t, true)
		foreign := f.previous
		foreign.Daemon = "other.example.com/someone/gastown-daemon@" + testDigestOf([]byte("elsewhere"))
		f.record(t, update.ImageRecord{Current: f.running, Previous: foreign})
		_, err := f.runRollback(t)
		if err == nil || !strings.Contains(err.Error(), "is not a digest of the town's gastown-daemon image") {
			t.Fatalf("gt update rollback error = %v, want the foreign-image refusal", err)
		}
		if len(f.swap.applied) != 0 {
			t.Errorf("the daemon was put on %+v, want no swap", f.swap.applied)
		}
	})
}
