package git

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town.
//
// It also turns off git's fsync in the sandbox's global gitconfig. Every
// commit, push and ref update otherwise flushes to disk: measured on the
// gate host, a commit took 56 ms with fsync and 19 ms without, and parallel
// tests queue on the flushes. Durability after a crash is the only thing
// fsync buys, and nothing here outlives the test binary.
func TestMain(m *testing.M) {
	h, err := testutil.StartHermetic()
	if err != nil {
		fmt.Fprintf(os.Stderr, "hermetic harness setup failed: %v\n", err)
		os.Exit(1)
	}
	if err := appendSandboxGitConfig(h.HomeDir, "[core]\n\tfsync = none\n"); err != nil {
		fmt.Fprintf(os.Stderr, "configuring sandbox git: %v\n", err)
		os.Exit(h.Finish(1))
	}
	os.Exit(h.Finish(m.Run()))
}

func appendSandboxGitConfig(home, cfg string) error {
	f, err := os.OpenFile(filepath.Join(home, ".gitconfig"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(cfg); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}
