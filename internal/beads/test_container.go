package beads

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Test Dolt container init capacity (gt-elvf4).
//
// Each container-backed test package runs one shared Dolt container per test
// process (internal/testutil/doltserver.go), and every isolated Init against
// it is a CREATE DATABASE plus a full migration pass, one DOLT_COMMIT per
// step. internal/refinery runs about eighteen of those at once against its one
// server. Under gate load that burst produced both failure faces of gt-elvf4:
// Dolt's catalog-snapshot race ("could not resolve initial root") and bd
// refusing to resume a migration it had itself been interrupted in
// ("refusing to auto-apply"). The pool below caps concurrent inits per
// process, which is the scope the contention lives in; the env var lets bd
// resume a half-migrated throwaway database instead of refusing it.

// testDoltInitConcurrencyEnv overrides the pool size.
const testDoltInitConcurrencyEnv = "GT_TEST_DOLT_INIT_CONCURRENCY"

// defaultTestDoltInitConcurrency is the pool size when the override is unset
// or unusable.
const defaultTestDoltInitConcurrency = 4

// allowRemoteMigrateEnv is bd's documented escape hatch for its
// remote-migrate gate (beads internal/storage/schema/remote_migrate_gate.go).
const allowRemoteMigrateEnv = "BD_ALLOW_REMOTE_MIGRATE"

// initSlots is a counting semaphore whose channel holds the free tokens: it
// is filled when built, acquire receives a token and release sends it back.
// Both failed gt-elvf4 patches declared such a channel and never filled it, so
// the first receive blocked forever; newInitSlots is the only constructor, so
// a pool cannot exist unfilled.
type initSlots struct {
	tokens chan struct{}
}

func newInitSlots(n int) *initSlots {
	s := &initSlots{tokens: make(chan struct{}, n)}
	for i := 0; i < n; i++ {
		s.tokens <- struct{}{}
	}
	return s
}

// acquire takes a token, waiting until one is free or ctx is done. The
// returned release gives the token back to this pool; only its first call
// does anything, so a deferred release beside an explicit one cannot grow the
// pool.
func (s *initSlots) acquire(ctx context.Context) (release func(), err error) {
	// Checked first: a select with both cases ready picks at random, and a
	// caller whose context is already done must not take a token.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("test Dolt init slot: %w", ctxErr)
	}
	select {
	case <-s.tokens:
	case <-ctx.Done():
		return nil, fmt.Errorf("test Dolt init slot: %w", ctx.Err())
	}
	var once sync.Once
	return func() { once.Do(func() { s.tokens <- struct{}{} }) }, nil
}

// testContainerInitSlots is this process's pool. Tests swap it whole for a
// small one (never resize it), hence the atomic pointer.
var testContainerInitSlots atomic.Pointer[initSlots]

// The size is read at package initialization on purpose: testutil.HermeticMain
// unsets every GT_* variable before a test package's tests run, so a lazy read
// would never see the override. Package init runs before TestMain.
func init() {
	n, warning := parseTestDoltInitConcurrency(os.Getenv(testDoltInitConcurrencyEnv))
	if warning != "" {
		fmt.Fprintln(os.Stderr, warning)
	}
	testContainerInitSlots.Store(newInitSlots(n))
}

// parseTestDoltInitConcurrency returns the pool size for raw, the value of
// GT_TEST_DOLT_INIT_CONCURRENCY, and a one-line warning when raw is set but
// unusable (not a whole number, or below 1). There is no upper clamp.
func parseTestDoltInitConcurrency(raw string) (n int, warning string) {
	if raw == "" {
		return defaultTestDoltInitConcurrency, ""
	}
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || n < 1 {
		return defaultTestDoltInitConcurrency, fmt.Sprintf("beads: ignoring %s=%q (want a whole number >= 1); using %d",
			testDoltInitConcurrencyEnv, raw, defaultTestDoltInitConcurrency)
	}
	return n, ""
}

// AcquireTestContainerInitSlot takes one of this process's test Dolt init
// slots, waiting until one is free or ctx is done; a done ctx returns
// "test Dolt init slot: <ctx error>" wrapping ctx.Err(). Call release exactly
// when the init — every retry of it included — is over; it is idempotent.
// Only Init and RunTestContainerInit acquire, and neither calls the other, so
// no caller ever holds two slots.
func AcquireTestContainerInitSlot(ctx context.Context) (release func(), err error) {
	return testContainerInitSlots.Load().acquire(ctx)
}

// testContainerEnv is what a bd call against testutil's Dolt container needs
// on top of its port wiring. An init interrupted partway through its
// migrations leaves its own database committed at some middle version, and
// bd's remote-migrate gate refuses to resume that in server mode; on a
// throwaway test database resuming is the right answer.
func testContainerEnv() []string {
	return []string{allowRemoteMigrateEnv + "=1"}
}
