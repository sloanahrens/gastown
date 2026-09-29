//go:build integration

package cmd

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestIntegrationDoltPool_IsolatedInitsCreateNoDatabases pins the fix for the
// shared-container schema-migration flake (TestFindActivePatrol* and friends).
//
// Dolt fails a session's information_schema reads and savepoints ("could not
// resolve initial root for database X/") while another session creates or
// drops a database, and bd init used to CREATE its database while other tests
// migrated theirs on the same server: 23 of 24 parallel inits failed beside a
// DROP loop, most with "pending schema migrations alter pre-existing dirty
// tables". Now every database a test process uses exists before its first test
// runs. Parallel isolated inits must therefore land in pool databases and leave
// the server's catalog exactly as they found it.
func TestIntegrationDoltPool_IsolatedInitsCreateNoDatabases(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Fatalf("bd CLI not on PATH: %v", err)
	}
	testutil.RequireDoltContainer(t)
	port := testutil.DoltContainerPort()
	before := showDatabases(t, port)

	const inits = 4
	dirs := make([]string, inits)
	errs := make([]error, inits)
	var wg sync.WaitGroup
	for i := range inits {
		dirs[i] = t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf [4]byte
			_, _ = rand.Read(buf[:])
			var p int
			_, _ = fmt.Sscan(port, &p)
			b := beads.NewIsolatedWithPort(dirs[i], p)
			if err := b.Init("dp" + hex.EncodeToString(buf[:])); err != nil {
				errs[i] = fmt.Errorf("init: %w", err)
				return
			}
			if _, err := b.Create(beads.CreateOptions{Title: "pooled", Priority: -1}); err != nil {
				errs[i] = fmt.Errorf("create: %w", err)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("isolated init %d: %v", i, err)
		}
	}

	for i, dir := range dirs {
		name := initDatabase(t, dir)
		if !testutil.IsDoltPoolDatabase(name) {
			t.Errorf("init %d used database %q, which the pool did not create before the tests ran", i, name)
		}
	}
	after := showDatabases(t, port)
	for name := range after {
		if !before[name] {
			t.Errorf("database %q appeared while tests ran; nothing may change the shared container's catalog mid-run", name)
		}
	}
}

func showDatabases(t *testing.T, port string) map[string]bool {
	t.Helper()
	db, err := sql.Open("mysql", "root:@tcp(127.0.0.1:"+port+")/")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query("SHOW DATABASES")
	if err != nil {
		t.Fatalf("SHOW DATABASES: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		out[n] = true
	}
	return out
}

// initDatabase reads the database bd init recorded in dir's workspace.
func initDatabase(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, ".beads", "metadata.json"))
	if err != nil {
		t.Fatalf("read metadata.json: %v", err)
	}
	var m struct {
		DoltDatabase string `json:"dolt_database"`
	}
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("parse metadata.json: %v", err)
	}
	return m.DoltDatabase
}
