package testutil

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql" // driver for creating the pool

	"github.com/steveyegge/gastown/internal/beads"
)

// The shared test Dolt container's database pool.
//
// A Dolt sql-server (dolthub/dolt-sql-server:2.0.7) fails a session's
// information_schema reads and savepoints with "could not resolve initial root
// for database X/" or "no root value found in session" while another session
// runs CREATE or DROP DATABASE. bd issues exactly those statements on every
// store open and all through a schema migration, so one test's bd init (a
// CREATE) or cleanup (a DROP) broke the migration of the test beside it; the
// migration left its working set dirty, bd retried its own open, and the retry
// refused with "pending schema migrations alter pre-existing dirty tables"
// (gastownhall/beads#4566) — the top flake family of the gate.
//
// So the catalog never changes while tests run. Every database a test process
// needs is created when its container starts, before any test can reach the
// container, and nothing is dropped: the container, and every database in it,
// goes at teardown. Isolated beads Inits (beads.SetTestDatabaseSource) and
// in-process test stores (OpenTestStore) take their database from the pool.
//
// Each entry is a store path whose BEADS_TEST_MODE database name
// ("testdb_" + FNV-64a of the path, beads internal/storage/dolt
// applyConfigDefaults) is what the pool created, so the same entry serves a
// bd init (the name) and a beadsdk.Open (the path).

// doltPoolPerRun is how many databases the pool holds per -count iteration.
// Measured per iteration on 2026-09-28: daemon 35, convoy 32, refinery 19,
// cmd 12, mail 1 — so a package can nearly quadruple its store tests before it
// runs out, and teardown warns once a run uses three quarters. Creating the
// 128 empty databases takes about 0.4s.
const doltPoolPerRun = 128

// doltPoolDDLTimeout bounds each CREATE DATABASE of the pool.
const doltPoolDDLTimeout = 2 * time.Minute

var doltPool struct {
	sync.Mutex
	port    int
	base    string
	entries []string // store paths, in creation order
	next    int      // index of the next entry to hand out
	names   map[string]bool
}

// testCount is this test binary's -test.count. The shared container can start
// from TestMain, before the testing package parses its flags, so os.Args is
// read when the flag is still at its default.
func testCount() int {
	if f := flag.Lookup("test.count"); f != nil && flag.Parsed() {
		if n, err := strconv.Atoi(f.Value.String()); err == nil && n > 0 {
			return n
		}
	}
	return testCountFromArgs(os.Args[1:])
}

func testCountFromArgs(args []string) int {
	for i, a := range args {
		for _, name := range []string{"-test.count", "--test.count"} {
			v := ""
			switch {
			case a == name && i+1 < len(args):
				v = args[i+1]
			case strings.HasPrefix(a, name+"="):
				v = a[len(name)+1:]
			default:
				continue
			}
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				return n
			}
		}
	}
	return 1
}

// beadsTestModeDatabase is the database name beadsdk.Open picks for dbPath
// under BEADS_TEST_MODE=1.
func beadsTestModeDatabase(dbPath string) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(dbPath))
	return fmt.Sprintf("testdb_%x", h.Sum64())
}

// createDoltPool creates the pool on the container at port. It runs inside the
// shared container's sync.Once, so no test can reach the container until it
// returns. It installs the pool as the source of isolated beads Inits.
func createDoltPool(port int) error {
	base, err := os.MkdirTemp("", "gt-doltpool-")
	if err != nil {
		return fmt.Errorf("dolt test pool dir: %w", err)
	}
	size := doltPoolPerRun * testCount()
	entries := make([]string, size)
	names := make(map[string]bool, size)
	for i := range entries {
		entries[i] = filepath.Join(base, fmt.Sprintf("s%04d", i), ".beads", "dolt")
		names[beadsTestModeDatabase(entries[i])] = true
	}

	db, err := sql.Open("mysql", fmt.Sprintf("root:@tcp(127.0.0.1:%d)/?timeout=30s", port))
	if err != nil {
		return fmt.Errorf("dolt test pool: %w", err)
	}
	defer db.Close()
	for _, p := range entries {
		ctx, cancel := context.WithTimeout(context.Background(), doltPoolDDLTimeout)
		_, err := db.ExecContext(ctx, "CREATE DATABASE `"+beadsTestModeDatabase(p)+"`")
		cancel()
		if err != nil {
			return fmt.Errorf("dolt test pool: create database %d of %d: %w", len(names), size, err)
		}
	}

	doltPool.Lock()
	doltPool.port, doltPool.base, doltPool.entries, doltPool.next, doltPool.names = port, base, entries, 0, names
	doltPool.Unlock()
	beads.SetTestDatabaseSource(func(p int) (string, error) {
		if p != port {
			return "", nil
		}
		path, err := takeDoltPoolPath()
		if err != nil {
			return "", err
		}
		return beadsTestModeDatabase(path), nil
	})
	return nil
}

// takeDoltPoolPath hands out the next pool entry's store path, created on disk.
func takeDoltPoolPath() (string, error) {
	doltPool.Lock()
	defer doltPool.Unlock()
	if doltPool.next >= len(doltPool.entries) {
		return "", fmt.Errorf("the shared test Dolt container's database pool is exhausted (%d databases, %d per -count iteration): "+
			"raise doltPoolPerRun in internal/testutil/doltpool.go — creating a database while tests run would reopen the catalog race it prevents",
			len(doltPool.entries), doltPoolPerRun)
	}
	p := doltPool.entries[doltPool.next]
	doltPool.next++
	if err := os.MkdirAll(p, 0o755); err != nil {
		return "", fmt.Errorf("dolt test pool store dir: %w", err)
	}
	return p, nil
}

// IsDoltPoolDatabase reports whether name is one of the databases the shared
// test container's pool created before tests ran.
func IsDoltPoolDatabase(name string) bool {
	doltPool.Lock()
	defer doltPool.Unlock()
	return doltPool.names[name]
}

// DoltPoolUsage reports how many pool databases have been handed out, and the
// pool's size.
func DoltPoolUsage() (used, size int) {
	doltPool.Lock()
	defer doltPool.Unlock()
	return doltPool.next, len(doltPool.entries)
}

// releaseDoltPool forgets the pool when its container goes, and removes the
// store directories it handed out.
func releaseDoltPool() {
	doltPool.Lock()
	base := doltPool.base
	used, size := doltPool.next, len(doltPool.entries)
	doltPool.port, doltPool.base, doltPool.entries, doltPool.next, doltPool.names = 0, "", nil, 0, nil
	doltPool.Unlock()
	beads.SetTestDatabaseSource(nil)
	if size > 0 && used*4 >= size*3 {
		fmt.Fprintf(os.Stderr, "testutil: shared Dolt container used %d of its %d pooled databases; raise doltPoolPerRun before it runs out\n", used, size)
	}
	if base != "" {
		_ = os.RemoveAll(base)
	}
}
