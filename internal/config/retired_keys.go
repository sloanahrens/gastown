package config

import (
	"fmt"
	"strings"
)

// The two-seat keys the seat map replaced (gt-mukgk). A file that still holds
// one does not load: the dispatcher would otherwise read config that looks
// authoritative and does nothing. The error names the replacement and says to
// run gt pool migrate, which converts the file.

// retiredSeatKeys maps each retired key, as its path from the town settings
// file, to what replaced it. A patrol document read on its own (the five-file
// layout's mayor/daemon.json) names the same key without the leading "daemon.".
var retiredSeatKeys = map[string]string{
	"polecat_pool.agent":                        "polecat_pool.seats",
	"polecat_pool.max_seats":                    "polecat_pool.seats.<agent>.cap",
	"polecat_pool.overflow_agent":               "polecat_pool.seats",
	"polecat_pool.max_overflow":                 "polecat_pool.seats.<agent>.cap",
	"polecat_pool.pro_agent":                    "polecat_pool.seats",
	"polecat_pool.pro_max":                      "polecat_pool.seats.<agent>.cap",
	"polecat_pool.pro_label":                    "an agent:<name> label on the bead",
	"daemon.patrols.spec_dispatch.hooked_agent": "polecat_pool.seats",
	"daemon.patrols.spec_dispatch.max_hooked":   "polecat_pool.seats.<agent>.cap",
	"daemon.patrols.spec_dispatch.prefer_hooked": "nothing: a bead with no agent label draws " +
		"uniformly among the agents with a free seat",
	"role_agents.polecat": "polecat_pool.seats",
}

// retiredScheduledAgentReplacement is what replaced the agent on a scheduled
// entry the spec dispatcher runs. The entry's index stays in the reported path.
const retiredScheduledAgentReplacement = "an agent:<name> label on the bead, run_agent on the entry (the label its run beads carry), or a seat in polecat_pool.seats"

// retiredKeyReplacement returns what replaced the key at path, and whether the
// key is retired. A path from a patrol document read on its own is matched as
// though it sat under "daemon".
func retiredKeyReplacement(path string) (string, bool) {
	if strings.HasPrefix(path, "patrols.") {
		path = "daemon." + path
	}
	if r, ok := retiredSeatKeys[path]; ok {
		return r, true
	}
	if isScheduledEntryAgentPath(path) {
		return retiredScheduledAgentReplacement, true
	}
	return "", false
}

// isScheduledEntryAgentPath reports whether path is the agent of a scheduled
// sling entry: daemon.patrols.scheduled_slings.entries[<i>].agent.
func isScheduledEntryAgentPath(path string) bool {
	const prefix = "daemon.patrols.scheduled_slings.entries["
	rest, ok := strings.CutPrefix(path, prefix)
	if !ok {
		return false
	}
	_, tail, ok := strings.Cut(rest, "]")
	return ok && tail == ".agent"
}

// retiredKeysError is the error text for a file that holds retired keys: each
// retired key and its replacement, then the command that converts the file.
// Keys that are not retired are named as unknown, so one pass reports them all.
func retiredKeysError(keys []string) error {
	var parts []string
	retired := false
	for _, k := range keys {
		if r, ok := retiredKeyReplacement(k); ok {
			retired = true
			parts = append(parts, fmt.Sprintf("%s was replaced by %s", k, r))
			continue
		}
		parts = append(parts, fmt.Sprintf("unknown key %q", k))
	}
	msg := strings.Join(parts, "; ")
	if retired {
		msg += ": run gt pool migrate"
	}
	return fmt.Errorf("%s", msg)
}

// anyRetired reports whether one of keys is a retired seat key.
func anyRetired(keys []string) bool {
	for _, k := range keys {
		if _, ok := retiredKeyReplacement(k); ok {
			return true
		}
	}
	return false
}

// retiredKeyFinder is the hook a config type uses to report retired keys the
// strict key walk cannot see: a key inside a map (role_agents.polecat), or one
// retired for some entries only (agent on a spec-dispatched scheduled entry).
// DecodeJSONFile asks after a successful decode.
type retiredKeyFinder interface {
	retiredKeys() []string
}

// retiredKeys reports the retired keys a decoded town settings file holds that
// the strict key walk passes: role_agents.polecat, and the same findings in
// the patrol document the file carries.
func (s *TownSettings) retiredKeys() []string {
	if s == nil {
		return nil
	}
	var keys []string
	if _, ok := s.RoleAgents["polecat"]; ok {
		keys = append(keys, "role_agents.polecat")
	}
	for _, k := range s.Daemon.retiredKeys() {
		keys = append(keys, "daemon."+k)
	}
	return keys
}

// retiredKeys reports the scheduled entries the spec dispatcher runs that still
// carry an agent. The paths are relative to the patrol document.
func (c *DaemonPatrolConfig) retiredKeys() []string {
	if c == nil || c.Patrols == nil || c.Patrols.ScheduledSlings == nil {
		return nil
	}
	var keys []string
	for i, e := range c.Patrols.ScheduledSlings.Entries {
		if e.Dispatch == "spec" && e.Agent != "" {
			keys = append(keys, fmt.Sprintf("patrols.scheduled_slings.entries[%d].agent", i))
		}
	}
	return keys
}
