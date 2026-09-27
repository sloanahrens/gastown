package chdir

import (
	"os"
	"testing"
)

func TestA(t *testing.T) {
	t.Parallel()
	os.Chdir("/tmp")
	t.Chdir("/tmp")
}
