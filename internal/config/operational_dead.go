package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
)

// Dead operational keys (gt-y3pgh.2.13). Nothing reads them; strict decoding
// keeps them declared so an older settings file still loads. Loading a file
// that sets one warns, naming the key and the file, so an operator can delete
// the key before the follow-up release deletes the declaration — at which
// point strict decoding refuses the key outright. Each key's reason is at its
// declaration in types.go; the list is grouped the way that file groups them.
var deadOperationalKeys = []string{
	// Whole sections declared as json.RawMessage: every key inside is dead.
	"operational.deacon",
	"operational.polecat",
	"operational.web",

	// operational.dolt
	"operational.dolt.health_check_interval",
	"operational.dolt.cmd_timeout",
	"operational.dolt.max_connections",
	"operational.dolt.slow_query_threshold",

	// operational.session
	"operational.session.claude_start_timeout",
	"operational.session.shell_ready_timeout",
	"operational.session.graceful_shutdown_timeout",
	"operational.session.gupp_violation_timeout",
	"operational.session.hung_session_threshold",

	// operational.nudge
	"operational.nudge.ready_timeout",
	"operational.nudge.retry_interval",
	"operational.nudge.lock_timeout",
	"operational.nudge.normal_ttl",
	"operational.nudge.urgent_ttl",

	// operational.daemon
	"operational.daemon.dog_idle_session_timeout",
	"operational.daemon.dog_idle_remove_timeout",
	"operational.daemon.stale_working_timeout",
	"operational.daemon.max_dog_pool_size",
	"operational.daemon.boot_spawn_cooldown",
	"operational.daemon.boot_turn_budget",
	"operational.daemon.boot_idle_suppression",
	"operational.daemon.boot_mode",
	"operational.daemon.deacon_grace_period",
	"operational.daemon.mass_death_window",
	"operational.daemon.mass_death_threshold",
	"operational.daemon.max_lifecycle_message_age",
	"operational.daemon.sync_failure_escalation_threshold",
	"operational.daemon.doctor_mol_cooldown",

	// operational.mail
	"operational.mail.idle_notify_timeout",
	"operational.mail.bd_read_timeout",
	"operational.mail.bd_write_timeout",
	"operational.mail.max_concurrent_ack_ops",

	// operational.witness (RecoveryThresholds)
	"operational.witness.startup_stall_threshold",
	"operational.witness.startup_activity_grace",
	"operational.witness.done_intent_stuck_timeout",
	"operational.witness.done_intent_recent_grace",
	"operational.witness.done_intent_max_age",
	"operational.witness.composer_stall_frozen_for",
}

// deadOperationalWarned keys the warnings this process has already printed,
// on the file path plus the key as the file spells it (gt-x2w2g). The loader
// runs on every gt command and every daemon tick, so without it a town that
// still sets a dead key repeats the same lines indefinitely.
var deadOperationalWarned sync.Map

// warnDeadOperationalKeys logs one warning for each dead key that data sets,
// at most once per (file, key) per process.
func warnDeadOperationalKeys(path string, data []byte) {
	for _, w := range newDeadOperationalWarnings(path, data) {
		fmt.Fprintln(os.Stderr, w)
	}
}

// newDeadOperationalWarnings returns the messages for dead keys that data sets
// and this process has not warned about yet, recording each as warned. Two
// files that set the same key each get a message, as do two keys in one file.
func newDeadOperationalWarnings(path string, data []byte) []string {
	var fresh []string
	for _, spelling := range deadOperationalSpellings(data) {
		if _, dup := deadOperationalWarned.LoadOrStore(path+"\x00"+spelling, struct{}{}); dup {
			continue
		}
		fresh = append(fresh, deadOperationalWarning(path, spelling))
	}
	return fresh
}

// deadOperationalWarnings returns one message per dead key that data sets,
// naming the key as the file spells it and the file it came from, and records
// nothing: newDeadOperationalWarnings is the deduplicating loader path. A file
// whose keys were all fixed yields nothing, and so does data that does not walk
// as JSON — the strict decode that runs first reports that once, as an error.
func deadOperationalWarnings(path string, data []byte) []string {
	spellings := deadOperationalSpellings(data)
	warnings := make([]string, 0, len(spellings))
	for _, spelling := range spellings {
		warnings = append(warnings, deadOperationalWarning(path, spelling))
	}
	return warnings
}

// deadOperationalSpellings returns, in deadOperationalKeys order, how data
// spells each dead key it sets.
func deadOperationalSpellings(data []byte) []string {
	present, err := presentKeyPaths(data)
	if err != nil {
		return nil
	}
	var spellings []string
	for _, key := range deadOperationalKeys {
		if spelling, ok := present[key]; ok {
			spellings = append(spellings, spelling)
		}
	}
	return spellings
}

// deadOperationalWarning is the line printed for one dead key in one file.
func deadOperationalWarning(path, spelling string) string {
	return fmt.Sprintf(
		"warning: %s: %s is set but nothing reads it; delete the key, because a future release will refuse it",
		path, spelling)
}

// presentKeyPaths maps every object key path in data, lowercased (encoding/json
// matches struct keys case-insensitively), to the spelling the file uses. An
// array contributes no path segment: the dead keys are all object keys, and a
// section listed whole covers whatever it holds.
func presentKeyPaths(data []byte) (map[string]string, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	paths := make(map[string]string)
	if err := walkKeyPaths(dec, "", paths); err != nil {
		return nil, err
	}
	return paths, nil
}

func walkKeyPaths(dec *json.Decoder, path string, out map[string]string) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar: no keys below it
	}
	switch delim {
	case '{':
		return walkObjectKeys(dec, path, out)
	case '[':
		return walkArrayKeys(dec, path, out)
	}
	return nil
}

func walkObjectKeys(dec *json.Decoder, path string, out map[string]string) error {
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, _ := tok.(string)
		child := joinKey(path, key)
		if _, seen := out[strings.ToLower(child)]; !seen {
			out[strings.ToLower(child)] = child
		}
		if err := walkKeyPaths(dec, child, out); err != nil {
			return err
		}
	}
	_, err := dec.Token() // '}'
	return err
}

func walkArrayKeys(dec *json.Decoder, path string, out map[string]string) error {
	for dec.More() {
		if err := walkKeyPaths(dec, path, out); err != nil {
			return err
		}
	}
	_, err := dec.Token() // ']'
	return err
}
