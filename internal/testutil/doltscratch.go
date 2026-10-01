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

// The scratch Dolt container serves tests whose code under test creates the
// databases it names — gt install's hq, gt rig add's rig database. The shared
// container cannot serve them: its catalog must not change while tests run
// (doltpool.go), and the next test would meet the last one's databases.
//
// Those tests used to start a container each (StartIsolatedDoltContainer).
// One scratch container per test binary serves them all instead (gt-16rk2): a
// test leases it exclusively, and when the lease ends every database the test
// created is dropped, so each lessee starts from the catalog the container
// started with. Nothing else runs on the container during a lease, so the
// drops are not the catalog change beside a live session that the pool
// exists to prevent.

// scratchLeaseWait is how long a lease waits for the previous lessee. The
// lessees are sequential tests, so a wait means one is running in parallel.
const scratchLeaseWait = 5 * time.Minute

// scratchParallelLeaseWait is how long a parallel test's lease waits. The
// parallel lessees queue for the container behind each other, so the wait is
// the sum of their runs, not a sign of misuse.
const scratchParallelLeaseWait = 10 * time.Minute

// scratchResetTimeout bounds the catalog reset at the end of a lease.
const scratchResetTimeout = 2 * time.Minute

var scratchDolt struct {
	once     sync.Once
	ctr      testcontainers.Container
	port     string
	baseline []string // the databases the container started with
	err      error    // the start failed, or a reset did; no lease is granted after
	lease    chan struct{}
}

// LeaseScratchDoltContainer gives t the scratch Dolt container for the rest
// of the test and returns its port. GT_DOLT_PORT, BEADS_DOLT_PORT and
// BEADS_DOLT_SERVER_PORT point this process and its subprocesses at it, and
// BEADS_TEST_SERVER declares it a test server. Not for parallel tests: it sets
// the environment, and the lease is exclusive.
func LeaseScratchDoltContainer(t *testing.T) string {
	t.Helper()
	leaseScratch(t, scratchLeaseWait)
	for _, kv := range scratchEnv(scratchDolt.port) {
		k, v, _ := strings.Cut(kv, "=")
		t.Setenv(k, v)
	}
	return scratchDolt.port
}

// LeaseScratchDoltContainerEnv is LeaseScratchDoltContainer for a parallel
// test. It takes the same exclusive lease, waiting for the other parallel
// lessees for up to scratchParallelLeaseWait, and sets nothing: it returns
// the port and os.Environ() with the variables LeaseScratchDoltContainer
// would set, which the test must give every bd and gt it runs. A subprocess
// that inherits the process environment instead reaches the package's shared
// container, whose catalog must not change.
func LeaseScratchDoltContainerEnv(t *testing.T) (port string, env []string) {
	t.Helper()
	leaseScratch(t, scratchParallelLeaseWait)
	env = os.Environ()
	for _, kv := range scratchEnv(scratchDolt.port) {
		k, _, _ := strings.Cut(kv, "=")
		env = beads.StripEnvKey(env, k)
	}
	return scratchDolt.port, append(env, scratchEnv(scratchDolt.port)...)
}

// scratchEnv is the environment that points bd and gt at the scratch
// container on port.
func scratchEnv(port string) []string {
	return []string{
		"GT_DOLT_PORT=" + port,
		"BEADS_DOLT_PORT=" + port,
		"BEADS_DOLT_SERVER_PORT=" + port,
		"BEADS_TEST_SERVER=1",
	}
}

// leaseScratch takes the scratch container for the rest of t, waiting at
// most wait for the previous lessee, and drops what t created when t ends.
func leaseScratch(t *testing.T, wait time.Duration) {
	t.Helper()
	if !DockerTestsEnabled() {
		t.Skip(dockerTestsSkipMsg)
	}
	if !isDockerAvailable() {
		t.Fatal(dockerMissingMsg)
	}

	scratchDolt.once.Do(startScratchDoltContainer)
	select {
	case scratchDolt.lease <- struct{}{}:
	case <-time.After(wait):
		t.Fatalf("scratch Dolt container still leased after %s", wait)
	}
	if scratchDolt.err != nil {
		<-scratchDolt.lease
		t.Fatalf("scratch Dolt container (%s=1 opted in, so a missing container fails): %v", DockerTestsEnv, scratchDolt.err)
	}
	t.Cleanup(func() {
		defer func() { <-scratchDolt.lease }()
		if err := resetScratchCatalog(); err != nil {
			scratchDolt.err = fmt.Errorf("resetting the catalog after %s: %w", t.Name(), err)
			t.Errorf("scratch Dolt container: %v", scratchDolt.err)
		}
	})
}

// startScratchDoltContainer starts the scratch container and records its
// starting catalog.
func startScratchDoltContainer() {
	scratchDolt.lease = make(chan struct{}, 1)
	ctx := context.Background()
	ctr, err := runDoltContainerWithRetry(ctx)
	if err != nil {
		scratchDolt.err = fmt.Errorf("starting Dolt container: %w", err)
		return
	}
	port, err := waitForMappedPort(ctx, doltPortLookup(ctr))
	if err == nil {
		scratchDolt.port = port
		scratchDolt.baseline, err = scratchDatabases(ctx)
	}
	if err != nil {
		scratchDolt.err = containerStartError(ctx, ctr, err)
		_ = terminateContainer(ctr)
		return
	}
	scratchDolt.ctr = ctr
	n, _ := strconv.Atoi(port)
	beads.RegisterTestServerPort(n)
}

// openScratch opens a session pool on the scratch container.
func openScratch() (*sql.DB, error) {
	return sql.Open("mysql", "root:@tcp(127.0.0.1:"+scratchDolt.port+")/?timeout=10s&readTimeout=60s&writeTimeout=60s")
}

// scratchDatabases lists the scratch container's databases.
func scratchDatabases(ctx context.Context) ([]string, error) {
	db, err := openScratch()
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

// resetScratchCatalog drops every database the container did not start with.
func resetScratchCatalog() error {
	ctx, cancel := context.WithTimeout(context.Background(), scratchResetTimeout)
	defer cancel()
	now, err := scratchDatabases(ctx)
	if err != nil {
		return err
	}
	extra := databasesAdded(scratchDolt.baseline, now)
	db, err := openScratch()
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

// terminateScratchDoltContainer stops and removes the scratch container, if
// this process started one.
func terminateScratchDoltContainer() error {
	if scratchDolt.ctr == nil {
		return nil
	}
	err := terminateContainer(scratchDolt.ctr)
	scratchDolt.ctr = nil
	return err
}
