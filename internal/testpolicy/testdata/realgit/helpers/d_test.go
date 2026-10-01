package helpers_test

import (
	"os/exec"
	"testing"
)

func setup() { _ = exec.Command("git", "init") }

func TestExternal(t *testing.T) {
	t.Parallel()
	setup()
}
