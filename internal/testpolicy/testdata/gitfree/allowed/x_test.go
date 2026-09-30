package allowed

import (
	"os/exec"
	"testing"
)

func TestAllowed(t *testing.T) {
	t.Parallel()
	//testpolicy:allow no-git — fixture: an exemption with a reason is honored
	_ = exec.Command("git", "version")
}
