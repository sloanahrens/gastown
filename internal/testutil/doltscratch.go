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
// A lease is still one container to one test, so a container's catalog change
// is never made beside a live session. What changed in gt-6u1qd is how many
// containers a process starts: one lease at a time made internal/cmd's eight
// scratch-lease tests queue for ~100s of the package's 170s wall, on a host
// with idle cores. A pool of scratchContainers serves that many leases at
// once. Packages that lease once still start one container and hold it.

// scratchContainers is how many scratch Dolt containers a test process may
// run at once. gt-16rk2 went from one container per test (a start each) to
// one per binary; this is the step back toward concurrency, bounded so the
// shared Docker VM sees a fixed few rather than one per test.
const scratchContainers = 3

// scratchLeaseWait is how long a lease waits for a container. Lessees that
// run alone (the serial-phase tests) wait here; a wait means every container
// is leased by a parallel test.
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

var scratchDolt struct {
	once   sync.Once
	insts  []*scratchInstance
	leases chan *scratchInstance // the containers not currently leased
	err    error                 // a start failed; no lease is granted after
}

// LeaseScratchDoltContainer gives t a scratch Dolt container for the rest of
// the test and returns its port. GT_DOLT_PORT, BEADS_DOLT_PORT and
// BEADS_DOLT_SERVER_PORT point this process and its subprocesses at it, and
// BEADS_TEST_SERVER declares it a test server. The environment it sets is
// process-wide, so this form is for tests that run alone; a parallel test
// takes LeaseScratchDoltContainerEnv and hands the returned environment to
// every subprocess instead.
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

	scratchDolt.once.Do(startScratchDoltContainers)
	var inst *scratchInstance
	select {
	case inst = <-scratchDolt.leases:
	case <-time.After(wait):
		t.Fatalf("no scratch Dolt container free after %s", wait)
	}
	if scratchDolt.err != nil {
		scratchDolt.leases <- inst
		t.Fatalf("scratch Dolt container (%s=1 opted in, so a missing container fails): %v", DockerTestsEnv, scratchDolt.err)
	}
	t.Cleanup(func() {
		defer func() { scratchDolt.leases <- inst }()
		if err := resetScratchCatalog(inst); err != nil {
			scratchDolt.err = fmt.Errorf("resetting the catalog after %s: %w", t.Name(), err)
			t.Errorf("scratch Dolt container: %v", scratchDolt.err)
		}
	})
	return inst
}

// startScratchDoltContainers starts every container concurrently, records the
// catalog each started with, and offers them as leases. All or nothing: a
// process that cannot provide its full pool leaves scratchDolt.err set and
// grants no lease, so a test never runs against a half-started pool.
func startScratchDoltContainers() {
	scratchDolt.leases = make(chan *scratchInstance, scratchContainers)
	ctx := context.Background()
	type started struct {
		inst *scratchInstance
		err  error
	}
	results := make(chan started, scratchContainers)
	for i := 0; i < scratchContainers; i++ {
		go func() {
			inst, err := startScratchDoltContainer(ctx)
			results <- started{inst: inst, err: err}
		}()
	}
	var errs []error
	for i := 0; i < scratchContainers; i++ {
		r := <-results
		if r.err != nil {
			errs = append(errs, r.err)
			continue
		}
		scratchDolt.insts = append(scratchDolt.insts, r.inst)
	}
	if len(errs) > 0 {
		scratchDolt.err = errors.Join(errs...)
		for _, inst := range scratchDolt.insts {
			_ = terminateContainer(inst.ctr)
		}
		scratchDolt.insts = nil
		return
	}
	for _, inst := range scratchDolt.insts {
		scratchDolt.leases <- inst
	}
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
// than a leased catalog may take any of them. Returns nil before the first
// lease started the pool.
func scratchContainerAny() testcontainers.Container {
	if len(scratchDolt.insts) == 0 {
		return nil
	}
	return scratchDolt.insts[0].ctr
}

// terminateScratchDoltContainer stops and removes every scratch container
// this process started.
func terminateScratchDoltContainer() error {
	insts := scratchDolt.insts
	scratchDolt.insts = nil
	var errs []error
	for _, inst := range insts {
		if err := terminateContainer(inst.ctr); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
