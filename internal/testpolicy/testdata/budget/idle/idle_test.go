// Package idle is a budget-runner fixture: its one test takes wall time but
// almost no CPU, so a runner that judges CPU passes it under any budget above
// its startup cost.
package idle

import (
	"testing"
	"time"
)

func TestWaits(t *testing.T) {
	time.Sleep(2 * time.Second)
}
