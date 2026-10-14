//go:build !windows

package procutil

import "testing"

func TestChildrenOfReadsTheChildrenOfOnePid(t *testing.T) {
	t.Parallel()
	table := "    1     0\n  100     1\n  200   100\n  201   100\n  202   100\n  300   200\nbad line\n"
	got := childrenOf(table, 100, 202)
	if len(got) != 2 || got[0] != 200 || got[1] != 201 {
		t.Fatalf("childrenOf = %v, want [200 201]: the children of 100 less the skipped ps, no grandchild", got)
	}
}
