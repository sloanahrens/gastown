package session

import "testing"

// SandboxTaskFilePath is where the runtime archive stages a seat's task:
// runtimeTaskFile under the sandbox root. gt prime's file mode reads exactly
// this path, so a move of either half must fail here (gt-w1mzol).
func TestNoBeadAccessSandboxTaskFilePath(t *testing.T) {
	t.Parallel()
	if got, want := SandboxTaskFilePath(), "/sandbox/.gt/task/TASK.md"; got != want {
		t.Errorf("SandboxTaskFilePath = %q, want %q", got, want)
	}
}
