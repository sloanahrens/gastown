package daemon

import (
	"sort"
	"strings"
	"time"
)

// slowHeartbeatStep is the duration from which a heartbeat step logs a line of
// its own (gt-1uz6p).
const slowHeartbeatStep = 2 * time.Second

// slowestStepsLogged is how many steps the heartbeat summary names.
const slowestStepsLogged = 3

// stepTime is one heartbeat step and how long it ran.
type stepTime struct {
	name string
	took time.Duration
}

// heartbeatTiming collects the steps of one heartbeat and their durations.
type heartbeatTiming struct {
	start time.Time
	steps []stepTime
}

// logStep records a finished step and logs it when it was slow.
func (d *Daemon) logStep(t *heartbeatTiming, name string, took time.Duration) {
	t.steps = append(t.steps, stepTime{name: name, took: took})
	if took >= slowHeartbeatStep {
		d.logger.Printf("Heartbeat step %s took %s", name, took.Round(time.Millisecond))
	}
}

// summary is the line naming the heartbeat's total and its slowest steps,
// longest first; steps of equal length keep their order.
func (t *heartbeatTiming) summary(total time.Duration) string {
	ranked := append([]stepTime(nil), t.steps...)
	sort.SliceStable(ranked, func(i, j int) bool { return ranked[i].took > ranked[j].took })
	if len(ranked) > slowestStepsLogged {
		ranked = ranked[:slowestStepsLogged]
	}
	parts := make([]string, 0, len(ranked))
	for _, s := range ranked {
		parts = append(parts, s.name+" "+s.took.Round(time.Millisecond).String())
	}
	return "Heartbeat took " + total.Round(time.Millisecond).String() + "; slowest: " + strings.Join(parts, ", ")
}
