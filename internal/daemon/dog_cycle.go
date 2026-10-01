package daemon

import (
	"fmt"
	"strings"

	"github.com/steveyegge/gastown/internal/events"
)

// dogCycleOutcome is how a dog job's cycle ended.
type dogCycleOutcome string

const (
	// dogCycleRan: every step the cycle recorded closed or was skipped.
	dogCycleRan dogCycleOutcome = "ran"

	// dogCycleFailed: at least one step failed. The reason names each one.
	dogCycleFailed dogCycleOutcome = "failed"
)

// Step results a dogCycle records.
const (
	dogStepDone    = "done"
	dogStepSkipped = "skipped"
	dogStepFailed  = "failed"
)

// dogStep is one step of a dog cycle and how it ended.
type dogStep struct {
	name   string
	result string
	reason string
}

// dogCycle is the receipt of one daemon dog job cycle (doctor_dog,
// wisp_reaper, jsonl_git_backup, compactor_dog, checkpoint_dog,
// mayor_dispatch). The job records each step as it finishes, and close reports
// the cycle exactly once: a daemon log line for every cycle, plus a feed event
// for a cycle with a failed step, so a failure reaches a reader who never sees
// the daemon log (gt-i3rpw).
//
// It writes nothing to beads. Each cycle used to pour a mol-dog-* wisp
// molecule as its receipt and close its step wisps one bd call at a time;
// under load those pours hit the 15s bd deadline or lost to Dolt
// serialization, so the receipt skipped cycles the job itself could have run
// (gt-4k3fj.8.1). Nothing ever picked the molecules up.
type dogCycle struct {
	job    string
	logger interface{ Printf(string, ...interface{}) }
	steps  []dogStep

	// feed records the failed-cycle event. The daemon writes the town feed;
	// tests capture the call.
	feed func(eventType string, payload map[string]interface{}) error
}

// startDogCycle begins the receipt for one cycle of job.
func (d *Daemon) startDogCycle(job string) *dogCycle {
	feed := d.dogFeedFn
	if feed == nil {
		townRoot := d.config.TownRoot
		feed = func(eventType string, payload map[string]interface{}) error {
			return events.LogFeedTo(townRoot, eventType, events.ActorDaemon, payload)
		}
	}
	return &dogCycle{job: job, logger: d.logger, feed: feed}
}

// closeStep records a step that ran to completion.
func (c *dogCycle) closeStep(name string) {
	c.steps = append(c.steps, dogStep{name: name, result: dogStepDone})
}

// skipStep records a step that ran and decided not to act. A skipped step is
// not a failure: a job whose correct outcome is silence has to be able to say
// so (gt-59o9).
func (c *dogCycle) skipStep(name, reason string) {
	c.steps = append(c.steps, dogStep{name: name, result: dogStepSkipped, reason: reason})
}

// failStep records a step that failed, with the reason.
func (c *dogCycle) failStep(name, reason string) {
	c.steps = append(c.steps, dogStep{name: name, result: dogStepFailed, reason: reason})
}

// outcome is how the cycle ended: failed if any step failed, with the failed
// steps and their reasons as the reason, else ran.
func (c *dogCycle) outcome() (dogCycleOutcome, string) {
	var failed []string
	for _, s := range c.steps {
		if s.result == dogStepFailed {
			failed = append(failed, fmt.Sprintf("%s: %s", s.name, s.reason))
		}
	}
	if len(failed) == 0 {
		return dogCycleRan, ""
	}
	return dogCycleFailed, strings.Join(failed, "; ")
}

// close reports the cycle. Every job defers it, so every cycle leaves exactly
// one `outcome=` line in the daemon log.
func (c *dogCycle) close() {
	outcome, reason := c.outcome()

	steps := make([]string, 0, len(c.steps))
	for _, s := range c.steps {
		steps = append(steps, s.name+"="+s.result)
	}
	c.logger.Printf("dog_cycle: %s outcome=%s steps=[%s] reason=%q", c.job, outcome, strings.Join(steps, " "), reason)

	if outcome == dogCycleRan {
		return
	}
	payload := map[string]interface{}{
		"job":     c.job,
		"outcome": string(outcome),
		"reason":  reason,
	}
	if err := c.feed(events.TypeDogCycleOutcome, payload); err != nil {
		c.logger.Printf("dog_cycle: recording the %s outcome failed: %v", c.job, err)
	}
}
