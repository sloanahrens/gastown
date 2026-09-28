package shebangpassthrough

import (
	"os"
	"strings"
	"testing"
)

// TrimPrefix, CutPrefix and Cut return their first argument, so a "#!"
// literal passed to them can still be written as a script.
func TestA(t *testing.T) {
	t.Parallel()
	_ = os.WriteFile("a", []byte(strings.TrimPrefix("#!/bin/sh\necho a\n", "")), 0o644)
	b, _ := strings.CutPrefix("#!/bin/sh\necho b\n", "")
	_ = os.WriteFile("b", []byte(b), 0o644)
	c, _, _ := strings.Cut("#!/bin/sh\necho c\n", "\x00")
	_ = os.WriteFile("c", []byte(c), 0o644)
}
