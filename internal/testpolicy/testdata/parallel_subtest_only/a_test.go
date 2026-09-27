package parallelsubtestonly

import "testing"

func TestTable(t *testing.T) {
	for _, n := range []string{"a", "b"} {
		n := n
		t.Run(n, func(t *testing.T) {
			t.Parallel()
		})
	}
}
