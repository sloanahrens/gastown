package daemon

import (
	"os"
	"strings"
	"testing"
)

// censusLogWiring pins the single call Run makes into the bd call census
// (gt-7h3vm): a one-minute ticker, the daemon's own logger, and a stop. Run
// itself is too heavy for a unit test, so this reads the source.
func TestCensusLogWiringUsesMinuteTickerAndDaemonLogger(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("daemon.go")
	if err != nil {
		t.Fatalf("read daemon.go: %v", err)
	}
	source := strings.Join(strings.Fields(string(src)), " ")
	for _, want := range []string{
		"censusTicker := time.NewTicker(time.Minute)",
		"defer censusTicker.Stop()",
		"go beads.StartCensusLog(d.ctx, censusTicker.C, d.logger.Printf)",
	} {
		if got := strings.Count(source, want); got != 1 {
			t.Errorf("daemon.go contains %q %d times, want exactly 1", want, got)
		}
	}
}
