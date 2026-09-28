package shebangwritten

import (
	"os"
	"strings"
	"testing"
)

func TestA(t *testing.T) {
	t.Parallel()
	script := "#!/bin/sh\necho hi\n"
	_ = strings.HasPrefix(script, "x")
	_ = os.WriteFile("s", []byte(script+"#!"), 0o644)
}
