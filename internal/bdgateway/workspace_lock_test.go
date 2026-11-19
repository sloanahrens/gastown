package bdgateway

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// workspaceTestYields bounds every barrier and channel receive in these tests:
// a lock bug fails the test instead of hanging it. It counts scheduler yields
// rather than reading a clock or a timer, so the bound is the same on every
// machine and no test waits on the wall clock.
const workspaceTestYields = 10_000_000

// recvBounded receives from ch, yielding to the scheduler between attempts, or
// fails the test once the yield budget is spent.
func recvBounded[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	for i := 0; i < workspaceTestYields; i++ {
		select {
		case v := <-ch:
			return v
		default:
			runtime.Gosched()
		}
	}
	t.Fatalf("no value after %d yields: a barrier or the lock is stuck", workspaceTestYields)
	var zero T
	return zero
}

// fakeLock is the in-process lock seam: one holder per path, shared by every
// store built over the same workspaceFS. A busy attempt reports busy and never
// blocks, so a test drives every wait through the fake clock.
type fakeLock struct {
	mu      sync.Mutex
	holders map[string]bool
}

func (l *fakeLock) tryLock(path string) (func() error, bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.holders[path] {
		return nil, true, nil
	}
	l.holders[path] = true
	return func() error {
		l.mu.Lock()
		defer l.mu.Unlock()
		delete(l.holders, path)
		return nil
	}, false, nil
}

// fakeClock is the wait's clock. pause advances it and yields, so a timeout is
// reached in fake time and a holder on another goroutine gets to run.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// pause advances the clock by d and yields.
func (c *fakeClock) pause(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
	runtime.Gosched()
}

// jump moves the clock forward by d without a wait.
func (c *fakeClock) jump(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// yieldWhileHeld is the pause for a test whose lock holder is another goroutine:
// a waiter yields and leaves the clock alone, so the wait ends when the holder
// releases, however long its disk work takes. Past the failsafe in real time it
// jumps the clock to the deadline, so a holder that never releases fails the
// test instead of hanging it.
func yieldWhileHeld(clock *fakeClock) func(time.Duration) {
	var yields atomic.Int64
	return func(time.Duration) {
		if yields.Add(1) > workspaceTestYields {
			clock.jump(workspaceLockWait)
			return
		}
		runtime.Gosched()
	}
}

// testLockedFS is the production seam with the lock and the clock replaced by
// in-process fakes. Stores built over one value share the lock, which is how a
// test stands in for a second gateway process: that process is a call to the
// same tryLock.
func testLockedFS() workspaceFS {
	fsys, _ := newTestLockedFS()
	return fsys
}

// newTestLockedFS is testLockedFS that also returns the fake clock, for a test
// that sets how the wait passes.
func newTestLockedFS() (workspaceFS, *fakeClock) {
	lock := &fakeLock{holders: map[string]bool{}}
	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	fsys := osWorkspaceFS()
	fsys.tryLock = lock.tryLock
	fsys.now = clock.Now
	fsys.pause = clock.pause
	return fsys, clock
}

// testLockPath is the lock file beside the root, as the store names it.
func testLockPath(root string) string {
	return filepath.Join(filepath.Dir(root), "."+filepath.Base(root)+".lock")
}

// writeFilesUnder writes files, keyed by slash path, beneath dir: a peer's temp
// tree, staged the way a push would stage it.
func writeFilesUnder(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, content := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatalf("staging %q: %v", name, err)
		}
		if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
			t.Fatalf("staging %q: %v", name, err)
		}
	}
}

// TestWorkspaceTwoStoresPushConcurrently is the measured failure: two gateway
// processes on one claim push at once. Both pushes end in success and the tree
// holds one whole document. Without the lock the swaps interleave and a push
// fails; the rounds make that interleaving likely.
func TestWorkspaceTwoStoresPushConcurrently(t *testing.T) {
	t.Parallel()
	type pushed struct {
		manifest string
		err      error
	}
	const rounds = 25
	for round := 0; round < rounds; round++ {
		fsys, clock := newTestLockedFS()
		fsys.pause = yieldWhileHeld(clock)
		root := filepath.Join(t.TempDir(), "ws")
		first, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening the first store: %v", err)
		}
		second, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening the second store: %v", err)
		}
		filesA := testWorkspaceFiles("a")
		filesB := testWorkspaceFiles("b")
		start := make(chan struct{})
		results := make(chan pushed, 2)
		for _, push := range []struct {
			store *WorkspaceStore
			files map[string]string
		}{{first, filesA}, {second, filesB}} {
			go func() {
				// The test closes start right after launching both goroutines, so this
				// receive cannot outlast the test's own flow.
				<-start
				manifest, err := push.store.Put(WorkspaceDocument{ManifestSHA: WorkspaceManifest(push.files), Files: push.files})
				results <- pushed{manifest, err}
			}()
		}
		close(start)
		for i := 0; i < 2; i++ {
			if got := recvBounded(t, results); got.err != nil {
				t.Fatalf("round %d: a concurrent push failed: %v", round, got.err)
			}
		}
		live := readWorkspace(t, root)
		if !reflect.DeepEqual(live, filesA) && !reflect.DeepEqual(live, filesB) {
			t.Fatalf("round %d: the tree is neither push whole: %v", round, live)
		}
	}
}

// TestWorkspaceOpenWaitsForASwapInFlight is a peer that has moved the live tree
// aside and staged its own, holding the lock, before it publishes. An open that
// finds the lock held waits for the peer to finish, reads the published tree and
// restores nothing; with no swap in flight, an open recovers the tree a crashed
// swap moved aside.
func TestWorkspaceOpenWaitsForASwapInFlight(t *testing.T) {
	t.Parallel()
	t.Run("the open waits, reads the finished tree and restores nothing", func(t *testing.T) {
		fsys := testLockedFS()
		root := filepath.Join(t.TempDir(), "ws")
		seed, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}
		pushWorkspace(t, seed, testWorkspaceFiles("old"))

		// The peer's swap is in flight: the old tree is moved aside and the new one is
		// staged, but the peer has not published it and holds the lock.
		old := filepath.Join(filepath.Dir(root), "ws.old")
		if err := os.Rename(root, old); err != nil {
			t.Fatalf("moving the live tree aside: %v", err)
		}
		staged := filepath.Join(filepath.Dir(root), "ws.new.peer")
		writeFilesUnder(t, staged, testWorkspaceFiles("peer"))
		peerRelease, busy, err := fsys.tryLock(testLockPath(root))
		if err != nil || busy {
			t.Fatalf("the peer taking the lock: busy=%v err=%v", busy, err)
		}

		// The peer finishes on the open's first wait: it publishes its tree and releases.
		published := false
		inner := fsys.pause
		fsys.pause = func(d time.Duration) {
			if !published {
				published = true
				if err := os.Rename(staged, root); err != nil {
					t.Errorf("the peer publishing its tree: %v", err)
				}
				if err := peerRelease(); err != nil {
					t.Errorf("the peer releasing the lock: %v", err)
				}
			}
			inner(d)
		}

		opened, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening during the swap: %v", err)
		}
		if !published {
			t.Fatal("the open did not wait for the lock: the peer's swap never ran")
		}
		if want := WorkspaceManifest(testWorkspaceFiles("peer")); opened.Manifest() != want {
			t.Fatalf("the open reports manifest %s, want the peer's %s", opened.Manifest(), want)
		}
		if _, err := os.Stat(old); err != nil {
			t.Fatalf("the moved-aside tree was restored over the peer's: %v", err)
		}
		if got := readWorkspace(t, root); !reflect.DeepEqual(got, testWorkspaceFiles("peer")) {
			t.Fatalf("the live tree is not the peer's: %v", got)
		}
	})

	t.Run("with no swap in flight an open recovers a half-finished swap under the lock", func(t *testing.T) {
		fsys := testLockedFS()
		root := filepath.Join(t.TempDir(), "ws")
		seed, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}
		pushWorkspace(t, seed, testWorkspaceFiles("kept"))
		old := filepath.Join(filepath.Dir(root), "ws.old")
		if err := os.Rename(root, old); err != nil {
			t.Fatalf("moving the live tree aside: %v", err)
		}

		recovered, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening after a crashed swap: %v", err)
		}
		if want := WorkspaceManifest(testWorkspaceFiles("kept")); recovered.Manifest() != want {
			t.Fatalf("the recovered manifest is %s, want %s", recovered.Manifest(), want)
		}
		if got := readWorkspace(t, root); !reflect.DeepEqual(got, testWorkspaceFiles("kept")) {
			t.Fatalf("the restored tree is not the one the crashed swap moved aside: %v", got)
		}
	})
}

// TestWorkspacePushLockTimeout is a lock held past the deadline. The push returns
// ErrWorkspaceLockTimeout, the HTTP push answers 503 workspace_lock_timeout, and
// nothing on disk changes.
func TestWorkspacePushLockTimeout(t *testing.T) {
	t.Parallel()
	fsys := testLockedFS()
	root := filepath.Join(t.TempDir(), "ws")
	s, err := openWorkspaceStore(root, fsys)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	pushWorkspace(t, s, testWorkspaceFiles("one"))
	before := readWorkspace(t, root)
	beforeSiblings := siblingNames(t, root)

	// A peer holds the lock and never lets go.
	if _, busy, err := fsys.tryLock(testLockPath(root)); err != nil || busy {
		t.Fatalf("the peer taking the lock: busy=%v err=%v", busy, err)
	}

	doc := WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("two")), Files: testWorkspaceFiles("two")}
	if _, err := s.Put(doc); !errors.Is(err, ErrWorkspaceLockTimeout) {
		t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceLockTimeout)
	}
	if got := readWorkspace(t, root); !reflect.DeepEqual(got, before) {
		t.Fatalf("a push that never took the lock changed the tree: %v", got)
	}
	if got := siblingNames(t, root); !reflect.DeepEqual(got, beforeSiblings) {
		t.Fatalf("a push that never took the lock left %v beside the root, want %v", got, beforeSiblings)
	}

	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("encoding the document: %v", err)
	}
	rec := httptest.NewRecorder()
	s.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, "/v1/workspace", bytes.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	var payload ErrorPayload
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decoding the refusal: %v", err)
	}
	if payload.Code != "workspace_lock_timeout" {
		t.Fatalf("code = %q, want workspace_lock_timeout", payload.Code)
	}
}

// TestWorkspacePeerSwapIsReported is a store that never saw its peer's push. Its
// next Put, here refused before any write, reports the manifest of the tree the
// peer published, read from the tree under the lock.
func TestWorkspacePeerSwapIsReported(t *testing.T) {
	t.Parallel()
	fsys := testLockedFS()
	root := filepath.Join(t.TempDir(), "ws")
	stale, err := openWorkspaceStore(root, fsys)
	if err != nil {
		t.Fatalf("opening the stale store: %v", err)
	}
	peer, err := openWorkspaceStore(root, fsys)
	if err != nil {
		t.Fatalf("opening the peer store: %v", err)
	}
	peerManifest := pushWorkspace(t, peer, testWorkspaceFiles("peer"))
	if stale.Manifest() == peerManifest {
		t.Fatal("the stale store already reports the peer's manifest; the test does not hold the stale view")
	}

	refused := WorkspaceDocument{ManifestSHA: "not-the-hash", Files: testWorkspaceFiles("mine")}
	if _, err := stale.Put(refused); !errors.Is(err, ErrWorkspaceManifestMismatch) {
		t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceManifestMismatch)
	}
	if got := stale.Manifest(); got != peerManifest {
		t.Fatalf("the stale store reports %s after its Put, want the peer's %s", got, peerManifest)
	}
}

// TestWorkspaceLockFailures are the lock's refusal branches. Each one leaves the
// tree as it was and says what failed.
func TestWorkspaceLockFailures(t *testing.T) {
	t.Parallel()
	t.Run("an open whose lock file cannot be locked is refused", func(t *testing.T) {
		fsys := testLockedFS()
		fsys.tryLock = func(string) (func() error, bool, error) { return nil, false, errors.New("permission denied") }
		root := filepath.Join(t.TempDir(), "ws")
		if _, err := openWorkspaceStore(root, fsys); !errors.Is(err, ErrWorkspaceLock) {
			t.Fatalf("open error = %v, want %v", err, ErrWorkspaceLock)
		}
	})

	t.Run("a push whose lock file cannot be locked changes nothing", func(t *testing.T) {
		fsys := testLockedFS()
		root := filepath.Join(t.TempDir(), "ws")
		s, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}
		pushWorkspace(t, s, testWorkspaceFiles("one"))
		before := readWorkspace(t, root)
		fsys.tryLock = func(string) (func() error, bool, error) { return nil, false, errors.New("permission denied") }
		s.fs = fsys
		doc := WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("two")), Files: testWorkspaceFiles("two")}
		if _, err := s.Put(doc); !errors.Is(err, ErrWorkspaceLock) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceLock)
		}
		if got := readWorkspace(t, root); !reflect.DeepEqual(got, before) {
			t.Fatalf("a push that could not lock changed the tree: %v", got)
		}
	})

	t.Run("a push whose lock cannot be released reports it", func(t *testing.T) {
		fsys := testLockedFS()
		root := filepath.Join(t.TempDir(), "ws")
		s, err := openWorkspaceStore(root, fsys)
		if err != nil {
			t.Fatalf("opening the store: %v", err)
		}
		inner := fsys.tryLock
		fsys.tryLock = func(path string) (func() error, bool, error) {
			release, busy, err := inner(path)
			if err != nil || busy {
				return release, busy, err
			}
			return func() error {
				_ = release()
				return errors.New("unlock refused")
			}, false, nil
		}
		s.fs = fsys
		doc := WorkspaceDocument{ManifestSHA: WorkspaceManifest(testWorkspaceFiles("one")), Files: testWorkspaceFiles("one")}
		if _, err := s.Put(doc); !errors.Is(err, ErrWorkspaceLock) {
			t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceLock)
		}
	})
}

// TestWorkspaceFlockAttempt drives each branch of one lock attempt with a fake
// flock: an interrupted call is retried, a held lock is reported busy, and any
// other failure is returned.
func TestWorkspaceFlockAttempt(t *testing.T) {
	t.Parallel()
	ioErr := errors.New("input/output error")
	cases := []struct {
		name     string
		answers  []error
		wantErr  error
		wantBusy bool
		wantCall int
	}{
		{name: "an interrupted call is retried until it answers", answers: []error{syscall.EINTR, nil}, wantCall: 2},
		{name: "a held lock is reported busy", answers: []error{syscall.EWOULDBLOCK}, wantBusy: true, wantCall: 1},
		{name: "any other failure is returned and holds nothing", answers: []error{ioErr}, wantErr: ioErr, wantCall: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			calls := 0
			flock := func(int, int) error {
				calls++
				if calls <= len(tc.answers) {
					return tc.answers[calls-1]
				}
				return nil // the unlock after a held lock
			}
			path := filepath.Join(t.TempDir(), ".ws.lock")
			release, busy, err := flockTryLock(path, flock)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("error = %v, want none", err)
			}
			if busy != tc.wantBusy {
				t.Fatalf("busy = %v, want %v", busy, tc.wantBusy)
			}
			if calls != tc.wantCall {
				t.Fatalf("flock called %d times, want %d", calls, tc.wantCall)
			}
			if release != nil {
				if err := release(); err != nil {
					t.Fatalf("releasing: %v", err)
				}
			}
		})
	}
}

// TestWorkspaceFlockExcludes is the production lock: a real flock on a file under
// the test's temp dir. Two goroutines cannot hold it at once, the lock file is
// created 0600 beside the root, and Put releases the lock when it returns an
// error.
func TestWorkspaceFlockExcludes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	root := filepath.Join(dir, "ws")
	lockFile := testLockPath(root)

	if _, _, err := osTryLock(filepath.Join(dir, "missing", ".ws.lock")); err == nil {
		t.Fatal("a lock file in a directory that does not exist was created")
	}

	holder, busy, err := osTryLock(lockFile)
	if err != nil || busy {
		t.Fatalf("taking the lock: busy=%v err=%v", busy, err)
	}
	info, err := os.Stat(lockFile)
	if err != nil {
		t.Fatalf("the lock file is not beside the root: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("the lock file mode is %o, want 600", mode)
	}

	attempt := func() <-chan bool {
		busyCh := make(chan bool, 1)
		go func() {
			release, busy, err := osTryLock(lockFile)
			if err != nil {
				busyCh <- false
				return
			}
			if !busy {
				_ = release()
			}
			busyCh <- busy
		}()
		return busyCh
	}
	if got := recvBounded(t, attempt()); !got {
		t.Fatal("a second goroutine took the lock while the first held it")
	}
	if err := holder(); err != nil {
		t.Fatalf("releasing the lock: %v", err)
	}
	if got := recvBounded(t, attempt()); got {
		t.Fatal("the lock stayed held after its holder released it")
	}

	// A Put that fails leaves the lock free for the next holder.
	s, err := OpenWorkspaceStore(root)
	if err != nil {
		t.Fatalf("opening the store: %v", err)
	}
	refused := WorkspaceDocument{ManifestSHA: "not-the-hash", Files: testWorkspaceFiles("one")}
	if _, err := s.Put(refused); !errors.Is(err, ErrWorkspaceManifestMismatch) {
		t.Fatalf("Put error = %v, want %v", err, ErrWorkspaceManifestMismatch)
	}
	if got := recvBounded(t, attempt()); got {
		t.Fatal("Put returned an error and kept the workspace lock")
	}
}
