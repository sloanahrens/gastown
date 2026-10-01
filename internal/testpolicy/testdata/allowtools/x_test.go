package allowtools

import (
	"os"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil"
	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

var more = "tmux"

func TestMain(m *testing.M) {
	_ = unittier.AllowTools("ps")
	os.Exit(testutil.HermeticMain(m, testutil.AllowTools("bd", "sh", more)))
}
