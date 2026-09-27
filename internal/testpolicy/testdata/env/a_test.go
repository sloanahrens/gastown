package env

import (
	"os"
	"testing"
)

func TestA(t *testing.T) {
	t.Parallel()
	os.Setenv("A", "b")
	t.Setenv("C", "d")
}
