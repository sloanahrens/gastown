//go:build !windows

package testutil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/testcontainers/testcontainers-go"
)

// The scratch Dolt containers serve tests whose code under test creates the
// databases it names — gt install's hq, gt rig add's rig database. The shared
// container cannot serve them: its catalog must not change while tests run
// (doltpool.go), and the next test would meet the last one's databases.
//
// A lease is one container to one test, so a container's catalog change is
// never made beside a live session, and the lease drops what its test created
// before the next lessee starts. One container per test binary served those
// lessees (gt-16rk2) until internal/cmd measured the cost: eight tests queued
// for ~100s of the package's 170s wall on 24 idle cores (gt-6u1qd). The pool
// starts another container when a lease finds the free list empty, up to
// scratchContainers, so overlapping lessees run at once while a package whose
// tests never overlap still starts exactly one container.

// scratchContainers is the most scratch Dolt containers one test process runs
// at once, and so the most leases that can overlap. Bounded so the shared
// Docker VM sees a fixed few rather than one container per test.
const scratchContainers = 3

// scratchLeaseWait is how long a serial test's lease waits for a container. A
// wait means scratchContainers tests are already running in parallel.
const scratchLeaseWait = 5 * time.Minute

// scratchParallelLeaseWait is how long a parallel test's lease waits. The
// parallel lessees queue for the containers behind each other, so the wait is
// the sum of their runs, not a sign of misuse.
const scratchParallelLeaseWait = 10 * time.Minute

// scratchResetTimeout bounds the catalog reset at the end of a lease.
const scratchResetTimeout = 2 * time.Minute

// scratchInstance is one container and the catalog it started with.
type scratchInstance struct {
	ctr      testcontainers.Container
	port     string
	baseline []string // the databases the container started with
}

// scratchPool is this process's scratch containers: the ones started, the ones
// no lease holds, and the start failure that stops it growing.
type scratchPool struct {
	mu       sync.Mutex
	insts    []*scratchInstance    // started and alive; at most scratchContainers
	starting int                   // starts in flight, counted against that cap
	free     chan *scratchInstance // the containers no lease holds
	broken   chan struct{}         // closed when a start fails
	err      error                 // the failure that stopped the pool
	start    func(context.Context) (*scratchInstance, error)
}

// newScratchPool returns an empty pool that starts its containers with start.
func newScratchPool(start func(context.Context) (*scratchInstance, error)) *scratchPool {
	return &scratchPool{
		free:   make(chan *scratchInstance, scratchContainers),
		broken: make(chan struct{}),
		start:  start,
	}
}

var scratch = newScratchPool(startScratchDoltContainer)

// LeaseScratchDoltContainer gives t a scratch Dolt container for the rest of
// the test and returns its port, pointing GT_DOLT_PORT, BEADS_DOLT_PORT,
// BEADS_DOLT_SERVER_PORT and BEADS_TEST_SERVER at it process-wide. Not for
// parallel tests: use LeaseScratchDoltContainerEnv, which returns the same
// environment for the test to hand to each subprocess.
func LeaseScratchDoltContainer(t *testing.T) string {
	t.Helper()
	inst := leaseScratch(t, scratchLeaseWait)
	for _, kv := range scratchEnv(inst.port) {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	return inst.port
}

// LeaseScratchDoltContainerEnv is LeaseScratchDoltContainer for a parallel
// test. It returns the port of a leased container and os.Environ() with the
// variables LeaseScratchDoltContainer would set, which the test must give
// every bd and gt it runs. A subprocess that inherits the process environment
// instead reaches the package's shared container, whose catalog must not
// change.
func LeaseScratchDoltContainerEnv(t *testing.T) (port string, env []string) {
	t.Helper()
	inst := leaseScratch(t, scratchParallelLeaseWait)
	env = os.Environ()
	for _, kv := range scratchEnv(inst.port) {
		k, _, _ := strings.Cut(kv, "=")
		env = beads.StripEnvKey(env, k)
	}
	return inst.port, append(env, scratchEnv(inst.port)...)
}

// scratchEnv is the environment that points bd and gt at the container on
// port.
func scratchEnv(port string) []string {
	return []string{
		"GT_DOLT_PORT=" + port,
		"BEADS_DOLT_PORT=" + port,
		"BEADS_DOLT_SERVER_PORT=" + port,
		"BEADS_TEST_SERVER=1",
	}
}

// leaseScratch takes a container for the rest of t, waiting at most wait for
// one to come free, and drops what t created when t ends.
func leaseScratch(t *testing.T, wait time.Duration) *scratchInstance {
	t.Helper()
	if !DockerTestsEnabled() {
		t.Skip(dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		t.Fatal(dockerMissingMsg)
	}

	deadline := time.Now().Add(wait)
	for {
		inst, err := scratch.acquire()
		if err != nil {
			t.Fatalf("scratch Dolt container (%s=1 opted in, so a missing container fails): %v", DockerTestsEnv, err)
		}
		if inst != nil {
			return holdScratch(t, inst)
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no scratch Dolt container free after %s", wait)
		}
		select {
		case inst := <-scratch.free:
			return holdScratch(t, inst)
		case <-scratch.broken:
			t.Fatalf("scratch Dolt container (%s=1 opted in, so a missing container fails): %v", DockerTestsEnv, scratch.startErr())
		case <-time.After(remaining):
		}
	}
}

// holdScratch gives t the lease on inst, and when t ends resets the catalog it
// created on and returns it to the pool.
func holdScratch(t *testing.T, inst *scratchInstance) *scratchInstance {
	t.Helper()
	t.Cleanup(func() {
		defer scratch.release(inst)
		if err := resetScratchCatalog(inst); err != nil {
			err = fmt.Errorf("resetting the catalog after %s: %w", t.Name(), err)
			scratch.fail(err)
			t.Errorf("scratch Dolt container: %v", err)
		}
	})
	return inst
}

// acquire returns a free container or starts one while the pool is under
// scratchContainers. A nil container with a nil error means every slot is
// leased or starting, so the caller waits; a start that fails stops the pool
// and every lease reports it from here on.
func (p *scratchPool) acquire() (*scratchInstance, error) {
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return nil, err
	}
	select {
	case inst := <-p.free:
		p.mu.Unlock()
		return inst, nil
	default:
	}
	if p.starting+len(p.insts) >= scratchContainers {
		p.mu.Unlock()
		return nil, nil
	}
	// Hold the slot across the start, so concurrent lessees cannot between
	// them start more than scratchContainers.
	p.starting++
	p.mu.Unlock()

	inst, err := p.start(context.Background())

	p.mu.Lock()
	p.starting--
	if err != nil {
		p.mu.Unlock()
		p.fail(err)
		return nil, err
	}
	p.insts = append(p.insts, inst)
	p.mu.Unlock()
	return inst, nil
}

// release returns inst to the pool for the next lessee.
func (p *scratchPool) release(inst *scratchInstance) {
	p.free <- inst
}

// fail records the first failure that stopped the pool and wakes every lessee
// waiting for a container.
func (p *scratchPool) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err == nil {
		p.err = err
		close(p.broken)
	}
}

// startErr returns the failure that stopped the pool, if any.
func (p *scratchPool) startErr() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// startScratchDoltContainer starts one container and records its starting
// catalog.
func startScratchDoltContainer(ctx context.Context) (*scratchInstance, error) {
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		return nil, fmt.Errorf("starting Dolt container: %w", err)
	}
	port, err := waitForMappedPort(ctx, doltPortLookup(ctr))
	if err == nil {
		inst := &scratchInstance{ctr: ctr, port: port}
		if inst.baseline, err = scratchDatabases(ctx, port); err == nil {
			n, _ := strconv.Atoi(port)
			beads.RegisterTestServerPort(n)
			return inst, nil
		}
	}
	startErr := containerStartError(ctx, ctr, err)
	_ = terminateContainer(ctr)
	return nil, startErr
}

// openScratch opens a session pool on the scratch container on port.
func openScratch(port string) (*sql.DB, error) {
	return sql.Open("mysql", "root:@tcp(127.0.0.1:"+port+")/?timeout=10s&readTimeout=60s&writeTimeout=60s")
}

// scratchDatabases lists the databases on the scratch container on port.
func scratchDatabases(ctx context.Context, port string) ([]string, error) {
	db, err := openScratch(port)
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	rows, err := db.QueryContext(ctx, "SHOW DATABASES")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		names = append(names, name)
	}
	return names, rows.Err()
}

// resetScratchCatalog drops every database the container did not start with,
// so the next lease of it starts from the catalog it began with.
func resetScratchCatalog(inst *scratchInstance) error {
	ctx, cancel := context.WithTimeout(context.Background(), scratchResetTimeout)
	defer cancel()
	now, err := scratchDatabases(ctx, inst.port)
	if err != nil {
		return err
	}
	extra := databasesAdded(inst.baseline, now)
	db, err := openScratch(inst.port)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close() }()
	var errs []error
	for _, name := range extra {
		if _, err := db.ExecContext(ctx, "DROP DATABASE `"+name+"`"); err != nil {
			errs = append(errs, fmt.Errorf("drop %s: %w", name, err))
		}
	}
	// Dolt keeps a dropped database for dolt_undrop; the next lessee creates
	// the same names, so let none linger.
	if _, err := db.ExecContext(ctx, "CALL dolt_purge_dropped_databases()"); err != nil {
		errs = append(errs, fmt.Errorf("purge dropped databases: %w", err))
	}
	return errors.Join(errs...)
}

// databasesAdded returns the names in now that are not in baseline, in now's
// order.
func databasesAdded(baseline, now []string) []string {
	had := make(map[string]bool, len(baseline))
	for _, name := range baseline {
		had[name] = true
	}
	var added []string
	for _, name := range now {
		if !had[name] {
			added = append(added, name)
		}
	}
	return added
}

// scratchContainerAny returns a started scratch container. Every instance
// starts with the same options, so a test that inspects the options rather
// than a leased catalog may take any of them. Returns nil before the pool has
// started one.
func scratchContainerAny() testcontainers.Container {
	scratch.mu.Lock()
	defer scratch.mu.Unlock()
	if len(scratch.insts) == 0 {
		return nil
	}
	return scratch.insts[0].ctr
}

// terminateScratchDoltContainer stops and removes every scratch container
// this process started.
func terminateScratchDoltContainer() error {
	scratch.mu.Lock()
	insts := scratch.insts
	scratch.insts = nil
	scratch.mu.Unlock()
	var errs []error
	for _, inst := range insts {
		if err := terminateContainer(inst.ctr); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
