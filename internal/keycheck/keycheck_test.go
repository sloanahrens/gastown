package keycheck

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/keystore"
)

// keyValue is the key every test stores. It is distinctive, so a leak shows up
// as a substring match.
const keyValue = "sk-test-KEYVALUE-0001"

// storeWith returns a Fake holding keyValue as ws's model provider key.
func storeWith(t *testing.T, ws string) *keystore.Fake {
	t.Helper()
	store := keystore.NewFake()
	if _, err := store.Put(context.Background(), ws, KeyName, []byte(keyValue)); err != nil {
		t.Fatalf("Put(%q): %v", ws, err)
	}
	return store
}

// fakeProvider answers every Verify with err and records the keys it saw.
type fakeProvider struct {
	err error

	mu   sync.Mutex
	seen []string
}

func providerAnswering(err error) *fakeProvider { return &fakeProvider{err: err} }

func (f *fakeProvider) Verify(_ context.Context, key keystore.Secret) error {
	f.mu.Lock()
	f.seen = append(f.seen, string(key.Bytes()))
	f.mu.Unlock()
	return f.err
}

// calls returns how many Verifies ran and the keys they were shown.
func (f *fakeProvider) calls() (int, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.seen), append([]string(nil), f.seen...)
}

func TestRefusedKeyPausesWithReason(t *testing.T) {
	t.Parallel()
	prov := providerAnswering(fmt.Errorf("%w: revoked", ErrRefused))

	d, err := Check(context.Background(), storeWith(t, "ws-a"), prov, "ws-a", NewTick())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if d.Dispatch {
		t.Error("a refused key dispatched")
	}
	if d.Reason != ReasonPausedKey {
		t.Errorf("Reason = %q, want %q", d.Reason, ReasonPausedKey)
	}
	if d.Workspace != "ws-a" {
		t.Errorf("Workspace = %q, want the paused workspace %q", d.Workspace, "ws-a")
	}
	if d.Warning != "" {
		t.Errorf("Warning = %q, want none on a refusal", d.Warning)
	}
	if n, _ := prov.calls(); n != 1 {
		t.Errorf("Verify ran %d times, want 1", n)
	}
}

func TestTransientProviderErrorDoesNotPause(t *testing.T) {
	t.Parallel()
	prov := providerAnswering(errors.New("dial tcp 10.0.0.1:443: i/o timeout"))

	d, err := Check(context.Background(), storeWith(t, "ws-a"), prov, "ws-a", NewTick())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !d.Dispatch {
		t.Error("a provider outage paused the workspace; it must only warn")
	}
	if d.Warning != WarningUnverified {
		t.Errorf("Warning = %q, want %q", d.Warning, WarningUnverified)
	}
	if d.Reason != "" || d.Workspace != "" {
		t.Errorf("Reason = %q, Workspace = %q; a transient error names neither", d.Reason, d.Workspace)
	}
}

func TestAcceptedKeyDispatchesAndVerifySeesStoredKey(t *testing.T) {
	t.Parallel()
	prov := providerAnswering(nil)

	d, err := Check(context.Background(), storeWith(t, "ws-a"), prov, "ws-a", NewTick())
	if err != nil {
		t.Fatalf("Check: %v", err)
	}
	if !d.Dispatch || d.Warning != "" || d.Reason != "" {
		t.Errorf("Decision = %+v, want a clean dispatch", d)
	}
	n, seen := prov.calls()
	if n != 1 || len(seen) != 1 || seen[0] != keyValue {
		t.Fatalf("Verify saw %q in %d calls, want the stored key once", seen, n)
	}
}

func TestOneCheckPerTick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := keystore.NewFake()
	for _, ws := range []string{"ws-a", "ws-b"} {
		if _, err := store.Put(ctx, ws, KeyName, []byte(keyValue)); err != nil {
			t.Fatalf("Put(%q): %v", ws, err)
		}
	}
	prov := providerAnswering(fmt.Errorf("%w: revoked", ErrRefused))
	tick := NewTick()

	first, err := Check(ctx, store, prov, "ws-a", tick)
	if err != nil {
		t.Fatalf("first Check: %v", err)
	}
	second, err := Check(ctx, store, prov, "ws-a", tick)
	if err != nil {
		t.Fatalf("second Check: %v", err)
	}
	if second != first {
		t.Errorf("second Check = %+v, want the first result %+v", second, first)
	}
	if n, _ := prov.calls(); n != 1 {
		t.Errorf("two Checks for one workspace in one tick ran Verify %d times, want 1", n)
	}

	if _, err := Check(ctx, store, prov, "ws-b", tick); err != nil {
		t.Fatalf("Check for a second workspace: %v", err)
	}
	if n, _ := prov.calls(); n != 2 {
		t.Errorf("Verify ran %d times after two workspaces, want 2", n)
	}

	if _, err := Check(ctx, store, prov, "ws-a", NewTick()); err != nil {
		t.Fatalf("Check in a new tick: %v", err)
	}
	if n, _ := prov.calls(); n != 3 {
		t.Errorf("Verify ran %d times after a new tick, want the key checked again (3)", n)
	}
}

func TestDecisionNeverContainsKey(t *testing.T) {
	t.Parallel()
	cases := map[string]error{
		"refusal":   fmt.Errorf("%w: rejected %s", ErrRefused, keyValue),
		"transient": fmt.Errorf("verifying %s: timeout", keyValue),
	}
	for name, provErr := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			d, err := Check(context.Background(), storeWith(t, "ws-a"), providerAnswering(provErr), "ws-a", NewTick())
			if err != nil {
				t.Fatalf("Check: %v", err)
			}
			if got := strings.Join([]string{d.Reason, d.Workspace, d.Warning}, "\n"); strings.Contains(got, keyValue) {
				t.Errorf("Decision %q contains the key", got)
			}
		})
	}
}

func TestCheckRefusesUnwiredInputs(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storeWith(t, "ws-a")
	prov := providerAnswering(nil)
	tick := NewTick()

	cases := []struct {
		name string
		call func() (Decision, error)
		want error
	}{
		{"no tick", func() (Decision, error) { return Check(ctx, store, prov, "ws-a", nil) }, ErrNoTick},
		{"no store", func() (Decision, error) { return Check(ctx, nil, prov, "ws-a", tick) }, ErrNoStore},
		{"no provider", func() (Decision, error) { return Check(ctx, store, nil, "ws-a", tick) }, ErrNoProvider},
		{"no workspace", func() (Decision, error) { return Check(ctx, store, prov, "", tick) }, ErrNoWorkspace},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := tc.call()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Check error = %v, want %v", err, tc.want)
			}
			if d != (Decision{}) {
				t.Errorf("Decision = %+v, want the zero Decision on a refused input", d)
			}
			if n, _ := prov.calls(); n != 0 {
				t.Errorf("a refused input ran Verify %d times", n)
			}
		})
	}
}

func TestCanceledContextRefused(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	d, err := Check(ctx, storeWith(t, "ws-a"), providerAnswering(nil), "ws-a", NewTick())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Check error = %v, want context.Canceled", err)
	}
	if d != (Decision{}) {
		t.Errorf("Decision = %+v, want the zero Decision", d)
	}
}

// cancelingProvider cancels the check's context as it answers, which is what a
// provider watching its context does when the caller gives up mid-call.
type cancelingProvider struct{ cancel context.CancelFunc }

func (p cancelingProvider) Verify(ctx context.Context, _ keystore.Secret) error {
	p.cancel()
	return ctx.Err()
}

func TestProviderCancellationIsNotDispatch(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	d, err := Check(ctx, storeWith(t, "ws-a"), cancelingProvider{cancel: cancel}, "ws-a", NewTick())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Check error = %v, want context.Canceled rather than a dispatch", err)
	}
	if d != (Decision{}) {
		t.Errorf("Decision = %+v, want the zero Decision", d)
	}
}

// countingStore counts Get calls so a test can see a tick reuse its result.
type countingStore struct {
	keystore.Store
	mu sync.Mutex
	n  int
}

func (s *countingStore) Get(ctx context.Context, workspace, name string) (keystore.Secret, error) {
	s.mu.Lock()
	s.n++
	s.mu.Unlock()
	return s.Store.Get(ctx, workspace, name)
}

func (s *countingStore) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

func TestStoreErrorReusedWithinTick(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := &countingStore{Store: keystore.NewFake()} // no key stored
	prov := providerAnswering(nil)
	tick := NewTick()

	for i := 0; i < 2; i++ {
		d, err := Check(ctx, store, prov, "ws-a", tick)
		if !errors.Is(err, keystore.ErrNotFound) {
			t.Fatalf("Check %d error = %v, want the store's ErrNotFound", i+1, err)
		}
		if !strings.Contains(err.Error(), "ws-a") {
			t.Errorf("Check %d error %q does not name the workspace", i+1, err)
		}
		if d != (Decision{}) {
			t.Errorf("Check %d Decision = %+v, want the zero Decision", i+1, d)
		}
	}
	if n := store.count(); n != 1 {
		t.Errorf("Get ran %d times in one tick, want 1", n)
	}
	if n, _ := prov.calls(); n != 0 {
		t.Errorf("a store error ran Verify %d times, want 0", n)
	}
}

// waitBound is how long a test waits for a goroutine-shaped event before it
// calls the code broken. It is a failure bound, not a timeout to tune.
const waitBound = 5 * time.Second

// await fails the test if ch has not been closed within waitBound.
func await(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), waitBound)
	defer cancel()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatalf("waited %s for %s; it did not arrive", waitBound, what)
	}
}

// blockingProvider holds every Verify until release is closed, so a test can
// keep one check in flight.
type blockingProvider struct {
	entered   chan struct{}
	release   chan struct{}
	closeIn   sync.Once
	releaseIn sync.Once
	mu        sync.Mutex
	n         int
}

func newBlockingProvider() *blockingProvider {
	return &blockingProvider{entered: make(chan struct{}), release: make(chan struct{})}
}

func (p *blockingProvider) Verify(_ context.Context, _ keystore.Secret) error {
	p.mu.Lock()
	p.n++
	p.mu.Unlock()
	p.closeIn.Do(func() { close(p.entered) })
	<-p.release
	return fmt.Errorf("%w: revoked", ErrRefused)
}

// releaseAll lets every Verify return. It runs once, so a test can call it on
// the way out after calling it itself.
func (p *blockingProvider) releaseAll() {
	p.releaseIn.Do(func() { close(p.release) })
}

func (p *blockingProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func TestConcurrentChecksShareOneVerify(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := storeWith(t, "ws-a")
	prov := newBlockingProvider()
	defer prov.releaseAll() // unblock Verify whatever way the test leaves
	tick := NewTick()

	type outcome struct {
		d   Decision
		err error
	}
	results := make(chan outcome, 2)
	var wg sync.WaitGroup
	start := func() {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, err := Check(ctx, store, prov, "ws-a", tick)
			results <- outcome{d, err}
		}()
	}

	start()
	await(t, prov.entered, "the first check to reach Verify")
	start()
	prov.releaseAll()

	both := make(chan struct{})
	go func() {
		wg.Wait()
		close(both)
	}()
	await(t, both, "both Checks to return")

	close(results)
	for o := range results {
		if o.err != nil {
			t.Fatalf("Check: %v", o.err)
		}
		if o.d.Dispatch || o.d.Reason != ReasonPausedKey {
			t.Errorf("Decision = %+v, want the refusal both callers see", o.d)
		}
	}
	if n := prov.count(); n != 1 {
		t.Errorf("Verify ran %d times for one workspace in one tick, want 1", n)
	}
}

// cancelThenAnswerProvider cancels the checking caller's context on its first
// Verify and answers nil on every later one, so a test can tell a reused
// cancellation from a new check.
type cancelThenAnswerProvider struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	n      int
}

func (p *cancelThenAnswerProvider) Verify(ctx context.Context, _ keystore.Secret) error {
	p.mu.Lock()
	p.n++
	first := p.n == 1
	p.mu.Unlock()
	if first {
		p.cancel()
		return ctx.Err()
	}
	return nil
}

func (p *cancelThenAnswerProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.n
}

func TestCanceledCallerDoesNotPoisonTheTick(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := storeWith(t, "ws-a")
	prov := &cancelThenAnswerProvider{cancel: cancel}
	tick := NewTick()

	d, err := Check(ctx, store, prov, "ws-a", tick)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Check error = %v, want context.Canceled", err)
	}
	if d != (Decision{}) {
		t.Errorf("canceled Check Decision = %+v, want the zero Decision", d)
	}

	// A second caller in the same tick has a live context of its own; the first
	// caller giving up is no answer about the key, so it must be checked again.
	live, err := Check(context.Background(), store, prov, "ws-a", tick)
	if err != nil {
		t.Fatalf("live Check reused the cancellation: %v", err)
	}
	if !live.Dispatch || live.Warning != "" || live.Reason != "" {
		t.Errorf("live Check Decision = %+v, want a clean dispatch", live)
	}
	if n := prov.count(); n != 2 {
		t.Errorf("Verify ran %d times, want the live caller's check to run (2)", n)
	}
}
