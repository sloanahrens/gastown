package shebangreadonly

import (
	"bytes"
	"strings"
	"testing"
)

// Reading a shebang is not writing a script.
func TestA(t *testing.T) {
	t.Parallel()
	var b []byte
	if !strings.HasPrefix(string(b), "#!/") || !bytes.HasPrefix(b, []byte("#!")) {
		t.Log("no shebang")
	}
}
