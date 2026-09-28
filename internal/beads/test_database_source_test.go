package beads

import (
	"errors"
	"strings"
	"testing"
)

func TestTestDatabaseForUsesTheSourceForItsPort(t *testing.T) {
	t.Cleanup(func() { SetTestDatabaseSource(nil) })
	SetTestDatabaseSource(func(port int) (string, error) {
		switch port {
		case 55001:
			return "testdb_pooled", nil
		case 55002:
			return "", errors.New("pool exhausted")
		}
		return "", nil
	})
	if got, err := testDatabaseFor(55001); err != nil || got != "testdb_pooled" {
		t.Errorf("testDatabaseFor(pool port) = %q, %v; want the pooled database", got, err)
	}
	if _, err := testDatabaseFor(55002); err == nil {
		t.Error("an exhausted pool fell back to minting a database bd would CREATE mid-run")
	}
	if got, err := testDatabaseFor(19999); err != nil || !strings.HasPrefix(got, testDatabasePrefix) || got == "testdb_pooled" {
		t.Errorf("testDatabaseFor(other port) = %q, %v; want a freshly minted name", got, err)
	}
	SetTestDatabaseSource(nil)
	if got, _ := testDatabaseFor(55001); got == "testdb_pooled" {
		t.Error("source still used after SetTestDatabaseSource(nil)")
	}
}

func TestRemintTestDatabaseTakesFromThePool(t *testing.T) {
	t.Cleanup(func() { SetTestDatabaseSource(nil) })
	SetTestDatabaseSource(func(port int) (string, error) {
		if port == 55001 {
			return "testdb_next", nil
		}
		return "", nil
	})
	args := []string{"init", "--prefix", "gt", "--quiet", "--database", "testdb_first", "--server", "--server-port", "55001"}
	got := remintTestDatabase(args)
	if got[5] != "testdb_next" {
		t.Errorf("reminted database = %q, want the next pooled one", got[5])
	}
	if args[5] != "testdb_first" {
		t.Error("remintTestDatabase modified its input")
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
