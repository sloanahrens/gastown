package prodsleepclock

import (
	"time"

	"github.com/jonboulle/clockwork"
)

var clock = clockwork.NewRealClock()

func Wait() {
	time.Sleep(time.Second)
}
