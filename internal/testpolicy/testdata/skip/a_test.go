package skip

import "testing"

func TestA(t *testing.T) {
	t.Parallel()
	t.Skip("skip a")
}

func TestB(t *testing.T) {
	t.Parallel()
	t.Skipf("skip %s", "b")
}

func TestC(t *testing.T) {
	t.Parallel()
	t.SkipNow()
}
