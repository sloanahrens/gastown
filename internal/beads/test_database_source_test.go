package beads

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func TestTestDatabaseForUsesTheSourceForItsPort(t *testing.T) {
	t.Cleanup(func() { SetTestDatabaseSource(nil) })
	SetTestDatabaseSource(func(port int, _ string) (string, error) {
		switch port {
		case 55001:
			return "testdb_pooled", nil
		case 55002:
			return "", errors.New("pool exhausted")
		}
		return "", nil
	})
	if got, err := testDatabaseFor(55001, t.TempDir()); err != nil || got != "testdb_pooled" {
		t.Errorf("testDatabaseFor(pool port) = %q, %v; want the pooled database", got, err)
	}
	if _, err := testDatabaseFor(55002, t.TempDir()); err == nil {
		t.Error("an exhausted pool fell back to minting a database bd would CREATE mid-run")
	}
	if got, err := testDatabaseFor(19999, t.TempDir()); err != nil || !strings.HasPrefix(got, testDatabasePrefix) || got == "testdb_pooled" {
		t.Errorf("testDatabaseFor(other port) = %q, %v; want a freshly minted name", got, err)
	}
	SetTestDatabaseSource(nil)
	if got, _ := testDatabaseFor(55001, t.TempDir()); got == "testdb_pooled" {
		t.Error("source still used after SetTestDatabaseSource(nil)")
	}
}

func TestRemintTestDatabaseTakesFromThePool(t *testing.T) {
	t.Cleanup(func() { SetTestDatabaseSource(nil) })
	var owner string
	SetTestDatabaseSource(func(port int, ownerDir string) (string, error) {
		if port == 55001 {
			owner = ownerDir
			return "testdb_next", nil
		}
		return "", nil
	})
	args := []string{"init", "--prefix", "gt", "--quiet", "--database", "testdb_first", "--server", "--server-port", "55001"}
	dir := t.TempDir()
	got := remintTestDatabase(args, dir)
	if got[5] != "testdb_next" {
		t.Errorf("reminted database = %q, want the next pooled one", got[5])
	}
	if owner != dir {
		t.Errorf("the pool was asked for a database owned by %q, want the init's workDir %q", owner, dir)
	}
	if args[5] != "testdb_first" {
		t.Error("remintTestDatabase modified its input")
	}
}

// recordTestDatabaseSource installs a source that hands out name for port and
// records the owner directory of every request.
func recordTestDatabaseSource(t *testing.T, port int, name string) func() []string {
	t.Helper()
	t.Cleanup(func() { SetTestDatabaseSource(nil) })
	var mu sync.Mutex
	var owners []string
	SetTestDatabaseSource(func(p int, ownerDir string) (string, error) {
		if p != port {
			return "", nil
		}
		mu.Lock()
		defer mu.Unlock()
		owners = append(owners, ownerDir)
		return name, nil
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), owners...)
	}
}

// TestInitLeasesItsDatabaseToItsWorkDir pins the wiring the pool ends a bd
// init lease by: the source is told the directory Init runs in, which is the
// test's, and the pool takes the database back once it is gone.
func TestInitLeasesItsDatabaseToItsWorkDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	useTestContainerInitSlots(t, 1)
	stub := installSucceedingBdStub(t)
	owners := recordTestDatabaseSource(t, 45678, "testdb_leased")
	dir := t.TempDir()
	if err := NewIsolatedWithPort(dir, 45678).Init("gt"); err != nil {
		t.Fatalf("Init: %v", err)
	}
	if got := owners(); len(got) != 1 || got[0] != dir {
		t.Errorf("the pool was asked for databases owned by %q, want one owned by the workDir %q", got, dir)
	}
	if calls := stub.invocations(t); len(calls) != 1 || databaseFlag(t, calls[0]) != "testdb_leased" {
		t.Errorf("bd ran %q, want one init on the leased database", calls)
	}
}

// TestRunTestContainerInitLeasesItsDatabaseToItsDir is the same wiring for a
// test helper's own bd init.
func TestRunTestContainerInitLeasesItsDatabaseToItsDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses Unix shell script mock for bd")
	}
	useTestContainerInitSlots(t, 1)
	installSucceedingBdStub(t)
	owners := recordTestDatabaseSource(t, 45678, "testdb_leased")
	dir := t.TempDir()
	args := []string{"init", "--quiet", "--prefix", "rt", "--server", "--server-port", "45678"}
	if out, err := RunTestContainerInit(context.Background(), dir, args, nil); err != nil {
		t.Fatalf("RunTestContainerInit: %v\n%s", err, out)
	}
	if got := owners(); len(got) != 1 || got[0] != dir {
		t.Errorf("the pool was asked for databases owned by %q, want one owned by %q", got, dir)
	}
}

func TestServerPortAndDatabaseArgs(t *testing.T) {
	if p := serverPortArg([]string{"init", "--server-port", "4242"}); p != 4242 {
		t.Errorf("serverPortArg = %d, want 4242", p)
	}
	if p := serverPortArg([]string{"init", "--server-port=17"}); p != 17 {
		t.Errorf("serverPortArg = %d, want 17", p)
	}
	if p := serverPortArg([]string{"init"}); p != 0 {
		t.Errorf("serverPortArg = %d, want 0", p)
	}
	if !hasDatabaseArg([]string{"init", "--database=x"}) || !hasDatabaseArg([]string{"init", "--database", "x"}) {
		t.Error("hasDatabaseArg missed a --database flag")
	}
	if hasDatabaseArg([]string{"init", "--prefix", "gt"}) {
		t.Error("hasDatabaseArg found a --database flag that is not there")
	}
}
