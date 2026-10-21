//go:build integration

package notes

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationExecHelpRunsTheInstalledBinary: execHelp runs the binary on the
// PATH with the subcommand path and --help, and keeps what a binary prints to
// stderr while it exits non-zero, as hm does. It sets PATH, so it does not run
// in parallel.
func TestIntegrationExecHelpRunsTheInstalledBinary(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"args: $*\" >&2\nexit 2\n"
	if err := os.WriteFile(filepath.Join(bin, "fakebin"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	got, err := execHelp{}.Help(context.Background(), "fakebin", "sub", "leaf")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(got) != "args: sub leaf --help" {
		t.Errorf("Help = %q, want the args it was run with", got)
	}
	if _, err := (execHelp{}).Help(context.Background(), "no-such-binary-xyz"); !errors.Is(err, ErrNoBinary) {
		t.Errorf("Help for a missing binary = %v, want ErrNoBinary", err)
	}
}
