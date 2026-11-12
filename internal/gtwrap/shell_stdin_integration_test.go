//go:build integration

package gtwrap

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationOSShellAttachStdinFeedsTheReader: a real process started by
// OSShell.AttachStdin reads the given reader as its stdin, and its exit status
// comes back. The process writes what it read to a temp file, since AttachStdin
// leaves its stdout attached to the wrapper's own.
func TestIntegrationOSShellAttachStdinFeedsTheReader(t *testing.T) {
	out := filepath.Join(t.TempDir(), "stdin.txt")
	body := "the bead text\nsecond line\n"
	exit, err := OSShell{}.AttachStdin([]string{"sh", "-c", `cat > "$1"; exit 3`, "sh", out}, nil, strings.NewReader(body))
	if err != nil || exit != 3 {
		t.Fatalf("AttachStdin = %d, %v; want exit 3 and no error", exit, err)
	}
	got, err := os.ReadFile(out)
	if err != nil || string(got) != body {
		t.Errorf("the process read %q (%v); want %q", got, err, body)
	}
}
