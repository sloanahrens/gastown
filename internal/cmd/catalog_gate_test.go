package cmd

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os/exec"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/testutil"
)

// TestCatalogGate_InitsSurviveConcurrentCatalogChurn pins the fix for the
// shared-container schema-migration flake (TestFindActivePatrol* and friends).
//
// Dolt fails a session's information_schema reads (and savepoints) with "could
// not resolve initial root for database X/" while another session creates or
// drops a database. Unguarded, a CREATE/DROP loop beside four parallel isolated
// inits failed 23 of 24 of them, most as "pending schema migrations alter
// pre-existing dirty tables". Through the catalog gate the churn and the inits
// never overlap, so every init and the store reads after it must succeed.
func TestCatalogGate_InitsSurviveConcurrentCatalogChurn(t *testing.T) {
	if _, err := exec.LookPath("bd"); err != nil {
		t.Skip("bd CLI not installed")
	}
	testutil.RequireDoltContainer(t)
	port, err := strconv.Atoi(testutil.DoltContainerPort())
	if err != nil {
		t.Fatalf("container port %q: %v", testutil.DoltContainerPort(), err)
	}

	stop := make(chan struct{})
	churnDone := make(chan error, 1)
	churned := 0
	go func() {
		for {
			select {
			case <-stop:
				churnDone <- nil
				return
			default:
			}
			name := fmt.Sprintf("catalog_churn_%d", time.Now().UnixNano())
			if err := beads.ExecTestCatalogDDL(port, "CREATE DATABASE `"+name+"`", "DROP DATABASE `"+name+"`"); err != nil {
				churnDone <- err
				return
			}
			churned++
			time.Sleep(20 * time.Millisecond)
		}
	}()

	const inits = 4
	errs := make([]error, inits)
	var wg sync.WaitGroup
	for i := range inits {
		dir := t.TempDir()
		wg.Add(1)
		go func() {
			defer wg.Done()
			var buf [4]byte
			_, _ = rand.Read(buf[:])
			b := beads.NewIsolatedWithPort(dir, port)
			if err := b.Init("cg" + hex.EncodeToString(buf[:])); err != nil {
				errs[i] = fmt.Errorf("init: %w", err)
				return
			}
			defer func() {
				if err := beads.ReleaseTestDatabase(port, b.TestDatabaseName()); err != nil {
					t.Logf("drop %s: %v", b.TestDatabaseName(), err)
				}
			}()
			for j := range 3 {
				if _, err := b.Create(beads.CreateOptions{Title: fmt.Sprintf("churn %d", j), Priority: -1}); err != nil {
					errs[i] = fmt.Errorf("create %d: %w", j, err)
					return
				}
			}
			if _, err := b.List(beads.ListOptions{Status: "all", Priority: -1}); err != nil {
				errs[i] = fmt.Errorf("list: %w", err)
			}
		}()
	}
	wg.Wait()
	close(stop)
	if err := <-churnDone; err != nil {
		t.Fatalf("catalog churn: %v", err)
	}
	if churned == 0 {
		t.Fatal("catalog churn never ran; the test proved nothing")
	}
	for i, err := range errs {
		if err != nil {
			t.Errorf("isolated bd %d beside %d catalog changes: %v", i, churned, err)
		}
	}
}
