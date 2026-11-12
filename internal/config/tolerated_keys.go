package config

import (
	"fmt"
	"io"
	"strings"
	"sync"
)

// A tolerated key is one the schema no longer declares and a load accepts: the
// live settings file is edited by hand, so the daemon must not refuse to start
// over a key that cannot do anything. Every load that sees one names it once
// per process, so the operator knows to remove the line.
//
// This is not the retired-key list (retired_keys.go), which refuses a key: a
// retired key still has a replacement to migrate to, and a tolerated key has
// nowhere to go but out.

// toleratedKeys names the keys the schema stopped declaring. Each path is
// written as it appears in the town settings file; a patrol document read on
// its own (mayor/daemon.json, or settings/config.json's "daemon" section)
// names the same key without the leading "daemon.".
var toleratedKeys = map[string]bool{
	// The sweep's idle wait went with its own Docker engine (gt-qpeei.8).
	"daemon.patrols.tier_sweep.runner_idle_command": true,
	"daemon.patrols.tier_sweep.runner_idle_wait":    true,
}

// toleratedKeyPath returns path in the form toleratedKeys writes it, and
// whether the key is tolerated.
func toleratedKeyPath(path string) (string, bool) {
	if strings.HasPrefix(path, "patrols.") {
		path = "daemon." + path
	}
	return path, toleratedKeys[path]
}

// toleratedWarned is each tolerated key already named this process: a command
// reads the same settings file once per operation, and the line is worth
// saying once.
var (
	toleratedWarnedMu sync.Mutex
	toleratedWarned   = map[string]bool{}
)

// warnToleratedKeys writes one line per tolerated key in paths not yet named
// this process, in the order the key walk found them. paths are as the walk
// saw them.
func warnToleratedKeys(warn io.Writer, paths []string) {
	for _, p := range paths {
		path, ok := toleratedKeyPath(p)
		if !ok {
			continue
		}
		toleratedWarnedMu.Lock()
		said := toleratedWarned[path]
		toleratedWarned[path] = true
		toleratedWarnedMu.Unlock()
		if said {
			continue
		}
		fmt.Fprintf(warn, "warning: %s is no longer read and can be removed\n", path)
	}
}

// forgetToleratedKeyWarnings makes every tolerated key namable again, which a
// test uses to load one file twice and assert one warning per key per process.
func forgetToleratedKeyWarnings() {
	toleratedWarnedMu.Lock()
	defer toleratedWarnedMu.Unlock()
	toleratedWarned = map[string]bool{}
}
