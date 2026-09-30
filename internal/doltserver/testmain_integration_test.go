//go:build integration

package doltserver_test

import (
	"fmt"
	"net"
	"os"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/testutil"
)

// TestMain runs the integration tier under the hermetic harness with real
// git, ps and lsof, and dolt where installed; the unit tier's TestMain
// (hermetic_main_test.go) runs on a fakeHost.
func TestMain(m *testing.M) {
	// Port-holder helper (process_integration_test.go): the test binary
	// re-executed as a non-dolt process holding a loopback port. Handled
	// before the harness so the child never builds sandbox temp dirs.
	if os.Getenv(portHolderHelperEnv) == "1" {
		runPortHolderHelper()
	}
	os.Exit(testutil.HermeticMain(m))
}

// portHolderHelperEnv must match process_integration_test.go. Not GT_*: the
// harness scrubs those.
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
