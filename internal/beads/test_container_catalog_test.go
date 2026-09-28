package beads

import (
	"testing"
	"time"
)

// TestChangeTestCatalogWaitsForSharedHolders pins the gate's one promise: a
// catalog change never runs while a bd subprocess holds the shared side, and a
// bd subprocess never starts while a catalog change runs. Dolt fails the
// information_schema reads of any session that overlaps a CREATE or DROP
// DATABASE, which is what broke concurrent test inits on the shared container.
func TestChangeTestCatalogWaitsForSharedHolders(t *testing.T) {
	prevActive, prevPort := testContainerActive.Load(), testContainerPort.Load()
	t.Cleanup(func() {
		testContainerActive.Store(prevActive)
		testContainerPort.Store(prevPort)
	})
	testContainerActive.Store(true)

	release := shareTestCatalog()
	changed := make(chan struct{})
	go func() {
		_ = ChangeTestCatalog(func() error { close(changed); return nil })
	}()
	select {
	case <-changed:
		t.Fatal("catalog change ran while a bd subprocess held the shared side")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-changed:
	case <-time.After(10 * time.Second):
		t.Fatal("catalog change never ran after the shared side was released")
	}

	inChange := make(chan struct{})
	finishChange := make(chan struct{})
	go func() {
		_ = ChangeTestCatalog(func() error { close(inChange); <-finishChange; return nil })
	}()
	<-inChange
	shared := make(chan struct{})
	go func() {
		r := shareTestCatalog()
		close(shared)
		r()
	}()
	select {
	case <-shared:
		t.Fatal("bd subprocess started while a catalog change ran")
	case <-time.After(100 * time.Millisecond):
	}
	close(finishChange)
	select {
	case <-shared:
	case <-time.After(10 * time.Second):
		t.Fatal("bd subprocess never started after the catalog change finished")
	}
}

// TestShareTestCatalogIsFreeWithoutASharedContainer: a process that never
// started the shared container takes no lock, so a catalog change is never
// blocked by, and never blocks, production-shaped bd calls.
func TestShareTestCatalogIsFreeWithoutASharedContainer(t *testing.T) {
	prev := testContainerActive.Load()
	t.Cleanup(func() { testContainerActive.Store(prev) })
	testContainerActive.Store(false)

	release := shareTestCatalog()
	defer release()
	done := make(chan struct{})
	go func() {
		_ = ChangeTestCatalog(func() error { close(done); return nil })
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("catalog change blocked by a bd call in a process with no shared container")
	}
}

func TestIsSharedTestContainerPort(t *testing.T) {
	prevActive, prevPort := testContainerActive.Load(), testContainerPort.Load()
	t.Cleanup(func() {
		testContainerActive.Store(prevActive)
		testContainerPort.Store(prevPort)
	})
	testContainerActive.Store(true)
	testContainerPort.Store(55001)
	if !isSharedTestContainerPort(55001) {
		t.Error("the recorded container port is not recognized")
	}
	for _, p := range []int{0, 19999} {
		if isSharedTestContainerPort(p) {
			t.Errorf("port %d treated as the shared container", p)
		}
	}
	testContainerActive.Store(false)
	if isSharedTestContainerPort(55001) {
		t.Error("an inactive container is still treated as shared")
	}
}

func TestInitTestDatabaseArg(t *testing.T) {
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"init", "--prefix", "gt", "--database", "testdb_ab", "--server"}, "testdb_ab"},
		{[]string{"init", "--database=testdb_cd"}, "testdb_cd"},
		{[]string{"init", "--database", "hq"}, ""},
		{[]string{"create", "--database", "testdb_ab"}, ""},
		{[]string{"init", "--prefix", "gt"}, ""},
	}
	for _, c := range cases {
		if got := initTestDatabaseArg(c.args); got != c.want {
			t.Errorf("initTestDatabaseArg(%q) = %q, want %q", c.args, got, c.want)
		}
	}
}

func TestWithInitDatabaseArg(t *testing.T) {
	got := withInitDatabaseArg([]string{"init", "--database", "testdb_a", "--server"}, "testdb_b")
	if got[2] != "testdb_b" {
		t.Errorf("--database value = %q, want testdb_b", got[2])
	}
	got = withInitDatabaseArg([]string{"init", "--database=testdb_a"}, "testdb_b")
	if got[1] != "--database=testdb_b" {
		t.Errorf("--database= form = %q, want --database=testdb_b", got[1])
	}
	orig := []string{"init", "--database", "testdb_a"}
	_ = withInitDatabaseArg(orig, "testdb_b")
	if orig[2] != "testdb_a" {
		t.Error("withInitDatabaseArg modified its input")
	}
}
