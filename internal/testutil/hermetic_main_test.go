//go:build !integration

package testutil

import (
	"os"
	"testing"
)

// TestMain runs the harness's own unit tier under the harness, with a git
// that refuses to run (internal/testpolicy/gitfree.txt): the tests drive
// the harness through a fake host and start no process. The integration
// tier's TestMain is in testmain_integration_test.go.
func TestMain(m *testing.M) {
	os.Exit(HermeticMain(m, WithoutGit()))
}
