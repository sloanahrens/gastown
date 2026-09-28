package parallelmissing

import (
	"os"
	"testing"
)

func TestA(t *testing.T) {
	_ = t
}

func TestMain(m *testing.M) {
	os.Exit(m.Run())
}
