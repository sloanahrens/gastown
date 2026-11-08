package config

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"time"
)

// The dispatch values a scheduled_slings entry accepts: "spec" creates the run
// bead for the spec dispatcher, and "shadow" runs the deep-review plan without
// slinging anything.
const (
	ScheduledDispatchSpec   = "spec"
	ScheduledDispatchShadow = "shadow"
)

// scheduledSlingNamePattern is an entry's name, the identity its run beads
// carry as the label "scheduled:<name>".
var scheduledSlingNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ValidateScheduledSlingEntry refuses a scheduled_slings entry the daemon
// cannot run, with the patrol's own error text. It is the one check the patrol
// applies to each entry and a settings writer applies to the document, so the
// two cannot drift (gt-nj23, gt-mukgk, gt-yjzepk.4).
func ValidateScheduledSlingEntry(e ScheduledSlingEntry) error {
	if !scheduledSlingNamePattern.MatchString(e.Name) {
		return fmt.Errorf("scheduled_slings: name %q must match %s", e.Name, scheduledSlingNamePattern)
	}
	if e.Rig == "" || e.Formula == "" {
		return fmt.Errorf("scheduled_slings[%s]: rig and formula are required", e.Name)
	}
	d, err := time.ParseDuration(e.IntervalStr)
	if err != nil || d <= 0 {
		return fmt.Errorf("scheduled_slings[%s]: interval %q must be a positive Go duration", e.Name, e.IntervalStr)
	}
	if err := ValidateScheduledSlingDispatch(e); err != nil {
		return err
	}
	if e.GateTimeoutStr != "" {
		if gt, err := time.ParseDuration(e.GateTimeoutStr); err != nil || gt <= 0 {
			return fmt.Errorf("scheduled_slings[%s]: gate_timeout %q must be a positive Go duration", e.Name, e.GateTimeoutStr)
		}
	}
	return nil
}

// ValidateScheduledSlingDispatch refuses a dispatch value the patrol does not
// know, and a var the dispatcher could not read back out of the "## Formula
// vars" section.
func ValidateScheduledSlingDispatch(e ScheduledSlingEntry) error {
	if e.RunAgent != "" && e.Dispatch != ScheduledDispatchSpec {
		return fmt.Errorf("scheduled_slings[%s]: run_agent is valid only with dispatch %q", e.Name, ScheduledDispatchSpec)
	}
	if e.Agent != "" && e.Dispatch == ScheduledDispatchShadow {
		return fmt.Errorf("scheduled_slings[%s]: agent is valid only for a slung entry; dispatch %q slings nothing", e.Name, ScheduledDispatchShadow)
	}
	switch e.Dispatch {
	case "":
		return nil
	case ScheduledDispatchSpec, ScheduledDispatchShadow:
	default:
		return fmt.Errorf("scheduled_slings[%s]: dispatch %q must be empty, %q or %q", e.Name, e.Dispatch, ScheduledDispatchSpec, ScheduledDispatchShadow)
	}
	for _, k := range slices.Sorted(maps.Keys(e.Vars)) {
		v := e.Vars[k]
		// One var is one line of the section: a newline would end it early, a
		// key with "=" or a leading "#" would be read as another key or a comment.
		if k == "" || k != strings.TrimSpace(k) || strings.ContainsAny(k, "=\n") || strings.HasPrefix(k, "#") || strings.Contains(v, "\n") {
			return fmt.Errorf("scheduled_slings[%s]: var %q cannot be written as a key=value line", e.Name, k)
		}
	}
	return nil
}
