package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestRunRefusesAMissingRevOrOut checks the command refuses to run without the
// commit the image is built at or the directory the context goes in, before it
// reads any file or runs any command.
func TestRunRefusesAMissingRevOrOut(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		args []string
	}{
		{"no flags", nil},
		{"no -out", []string{"-rev", "0000000000000000000000000000000000000001"}},
		{"no -rev", []string{"-out", "/tmp/x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(tc.args, &stdout, &stderr); code != 2 {
				t.Fatalf("run exit = %d, want 2", code)
			}
			if !strings.Contains(stderr.String(), "-rev and -out are required") {
				t.Errorf("stderr = %q, want the required-flags refusal", stderr.String())
			}
			if stdout.Len() != 0 {
				t.Errorf("a refused run printed to stdout: %q", stdout.String())
			}
		})
	}
}
