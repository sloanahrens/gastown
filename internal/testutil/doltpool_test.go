package testutil

import "testing"

func TestTestCountFromArgs(t *testing.T) {
	cases := []struct {
		args []string
		want int
	}{
		{nil, 1},
		{[]string{"-test.v=true", "-test.count=3"}, 3},
		{[]string{"-test.count", "5"}, 5},
		{[]string{"--test.count=2"}, 2},
		{[]string{"-test.count=0"}, 1},
		{[]string{"-test.count=x"}, 1},
		{[]string{"-test.run", "TestX"}, 1},
	}
	for _, c := range cases {
		if got := testCountFromArgs(c.args); got != c.want {
			t.Errorf("testCountFromArgs(%q) = %d, want %d", c.args, got, c.want)
		}
	}
}

func TestTakeDoltPoolPathExhausts(t *testing.T) {
	doltPool.Lock()
	saved := doltPool.entries
	savedNext := doltPool.next
	doltPool.entries = []string{t.TempDir() + "/a/.beads/dolt"}
	doltPool.next = 0
	doltPool.Unlock()
	t.Cleanup(func() {
		doltPool.Lock()
		doltPool.entries, doltPool.next = saved, savedNext
		doltPool.Unlock()
	})
	if _, err := takeDoltPoolPath(); err != nil {
		t.Fatalf("first take: %v", err)
	}
	if _, err := takeDoltPoolPath(); err == nil {
		t.Fatal("second take from a one-entry pool succeeded; an exhausted pool must fail, never create a database mid-run")
	}
}
