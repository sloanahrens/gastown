package execfile

import (
	"os"
	"testing"
)

func TestA(t *testing.T) {
	t.Parallel()
	p := "script.sh"
	b := []byte("echo hi\n")
	os.WriteFile(p, b, 0o755)
}
