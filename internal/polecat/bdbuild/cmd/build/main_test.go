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

func TestRunRequiresTheCheckoutAndTheOutput(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		nil,
		{"-src", "/beads"},
		{"-out", "/out/bd"},
	} {
		var stdout, stderr bytes.Buffer
		if code := run(args, &stdout, &stderr); code != 2 {
			t.Errorf("run(%v) = %d, want 2", args, code)
		}
		if !strings.Contains(stderr.String(), "-src and -out are required") {
			t.Errorf("run(%v) stderr = %q, want the missing flags named", args, stderr.String())
		}
		if stdout.Len() != 0 {
			t.Errorf("run(%v) wrote %q on stdout; the label value is the only stdout", args, stdout.String())
		}
	}
}

func TestRunRejectsAnUnknownFlag(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("run(-nope) = %d, want 2", code)
	}
}
