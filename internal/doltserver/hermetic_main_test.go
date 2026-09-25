package doltserver_test

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs this package's tests under the hermetic harness (gt-lwi):
// GT_*/BD_* env scrubbed, HOME and town root redirected to a sandbox, Dolt
// ports poisoned so nothing reaches the production server, and a tripwire
// that fails the run if any state leaks into a live town.
func TestMain(m *testing.M) {
	// Port-holder helper (pid_identity_test.go): the test binary re-executed
	// as a non-dolt process holding a loopback port. Handled before the
	// harness so the child never builds (and leaks) sandbox temp dirs.
	if os.Getenv(portHolderHelperEnv) == "1" {
		runPortHolderHelper()
	}
	os.Exit(testutil.HermeticMain(m))
}

// portHolderHelperEnv must match pid_identity_test.go. Not GT_*: the harness
// scrubs those.
const portHolderHelperEnv = "DOLTSERVER_TEST_PORT_HOLDER"

// runPortHolderHelper listens on a loopback port, prints it, and waits to be
// killed (or exits on its own after a minute).
func runPortHolderHelper() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		fmt.Printf("ERR=%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("PORT=%d\n", ln.Addr().(*net.TCPAddr).Port)
	time.Sleep(time.Minute)
	os.Exit(0)
}
