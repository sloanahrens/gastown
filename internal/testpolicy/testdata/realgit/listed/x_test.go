package listed

import (
	"os/exec"
	"testing"
)

func TestListed(t *testing.T) {
	t.Parallel()
	_ = exec.Command("git", "status")
}
