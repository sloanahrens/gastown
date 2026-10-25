//go:build !integration

package migrate

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/tracker/internal/leakcheck"
)

func TestMain(m *testing.M) { os.Exit(leakcheck.Main(m)) }
