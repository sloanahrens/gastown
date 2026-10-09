package slot

import (
	"io"
	"os"
	"testing"
	"time"
)

// TestRunLeavesNoSignalRelayBehind pins the cleanup half of gt-1j5rj: by the
// time Run returns, the relay that forwarded the wrapper's signals to the
// child has exited and the channel it read has been closed. unittier's
// end-of-run goroutine check is the assertion — a relay still blocked on an
// open channel is exactly what this test saw before gt-1j5rj, and it fails
// the package's unit tier wherever the relay is exercised.
//
// git is the wrapped command because it is the one tool the unit tier runs
// (docs/testing.md, "no-subprocess"); nothing here needs it to outlive the
// call.
func TestRunLeavesNoSignalRelayBehind(t *testing.T) {
	t.Parallel()
	town := t.TempDir()

	code, err := Run(town, RunOptions{
		Role:    "pid-run-signal-test",
		Timeout: 30 * time.Second,
		Pool:    Pool{Slots: 1},
		Nice:    0,
		Args:    []string{"git", "--version"},
		Path:    os.Getenv("PATH"),
		Env:     os.Environ,
		Stdout:  io.Discard,
		Stderr:  io.Discard,
		gate:    NewGate(WithRuntime(&fakeRuntime{})),
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Errorf("Run(git --version) exit code = %d, want 0", code)
	}
}
