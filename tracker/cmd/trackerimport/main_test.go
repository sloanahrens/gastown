//go:build !integration

package main

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
)

// TestMain runs the unit tier under the goroutine-leak check. The integration
// tier's TestMain, which starts a PostgreSQL for the run test, is in
// main_integration_test.go; the !integration tag keeps the two out of each
// other's way when `go test -tags integration` compiles the package.
func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }
