package daemon

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"

	"github.com/steveyegge/gastown/internal/attention"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// configParseFailedKey is the attention item's key: one item names the patrol
// config file, so the key is fixed rather than per failure.
const configParseFailedKey = "config-parse-failed"

// errPatrolConfigGone is the reload failure for a file that held a patrol
// config while the daemon runs on one and holds none now.
var errPatrolConfigGone = errors.New("the file no longer holds a patrol config")

// patrolFileStamp is one file as the reload step compares it between
// heartbeats: whether it exists, its modification time and size, and a SHA-256
// of its bytes. A file that cannot be stat'ed reads as absent, so one that
// becomes unreadable changes the stamp, and the reload reports it through the
// read.
type patrolFileStamp struct {
	present bool
	modNano int64
	size    int64
	sum     [sha256.Size]byte
}

// patrolStamp is the pair of files the patrol config is read from: the town
// settings file, and the legacy mayor/daemon.json a town not yet migrated keeps.
type patrolStamp struct {
	settings patrolFileStamp
	legacy   patrolFileStamp
}

// patrolLoadFailure is a reload that did not load the patrol config: the file
// does not parse, or it is gone while the daemon runs on a config. The daemon
// keeps the config in force, and the failure is what the attention item names.
type patrolLoadFailure struct {
	source string
	err    error
}

// takePatrolStamp stats the files the patrol config is read from.
func takePatrolStamp(townRoot string) patrolStamp {
	return patrolStamp{
		settings: statPatrolFile(agentconfig.TownSettingsPath(townRoot)),
		legacy:   statPatrolFile(PatrolConfigFile(townRoot)),
	}
}

func statPatrolFile(path string) patrolFileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return patrolFileStamp{}
	}
	stamp := patrolFileStamp{present: true, modNano: info.ModTime().UnixNano(), size: info.Size()}
	// A same-size edit inside one mtime tick leaves both unchanged on a
	// filesystem with coarse timestamps, so the bytes decide as well. A read
	// that fails leaves the hash zero, so a file that stops reading changes the
	// stamp, and the reload reads it and reports the error.
	if data, err := os.ReadFile(path); err == nil {
		stamp.sum = sha256.Sum256(data)
	}
	return stamp
}

// patrol returns the patrol config in force. Every reader takes it here, never
// the field, so a reload is seen whole: a reader gets one config, never a
// half-swapped one. A reader that uses the config more than once takes it into
// a local once, so one pass sees one config. A value a reader keeps across
// ticks, such as an interval read once at start or the Dolt server manager's
// config, holds until restart.
func (d *Daemon) patrol() *DaemonPatrolConfig {
	d.patrolMu.RLock()
	cfg := d.patrolConfig
	d.patrolMu.RUnlock()
	if d.onPatrolRead != nil {
		d.onPatrolRead()
	}
	return cfg
}

// currentPatrolFailure returns the reload failure in force, or nil.
func (d *Daemon) currentPatrolFailure() *patrolLoadFailure {
	d.patrolMu.RLock()
	defer d.patrolMu.RUnlock()
	return d.patrolFailure
}

// reloadPatrolConfig is the heartbeat step that picks up an edit to the patrol
// config (gt-6frjqf). It runs first in every heartbeat, so the rest of the
// heartbeat reads the edit. It parses only when a file's stamp changed since
// the last read, so an unchanged file is not reparsed and a failure is logged
// once per change.
//
// A file that does not load keeps the config in force, and the daemon never
// falls back to defaults while it runs. The values the daemon applied to its
// own process at start (daemon.env) are not applied again.
func (d *Daemon) reloadPatrolConfig() {
	stamp := takePatrolStamp(d.config.TownRoot)
	if stamp == d.patrolStamp {
		return
	}
	d.patrolStamp = stamp

	cfg, err := d.seams.readPatrol(d.config.TownRoot)
	if err == nil && cfg == nil && d.patrol() != nil {
		err = errPatrolConfigGone
	}
	if err != nil {
		source := patrolFailureSource(d.config.TownRoot, err)
		d.patrolMu.Lock()
		d.patrolFailure = &patrolLoadFailure{source: source, err: err}
		d.patrolMu.Unlock()
		d.logger.Printf("Patrol config %s did not reload, keeping the config in force: %v", source, err)
		return
	}

	d.patrolMu.Lock()
	d.patrolConfig = cfg
	d.patrolFailure = nil
	d.patrolMu.Unlock()
	d.logger.Printf("patrol config reloaded from %s (daemon.env is applied at start and is not re-applied on reload)",
		PatrolConfigSource(d.config.TownRoot))
}

// patrolFailureSource names the file a reload failure is about. A parse error
// names its own file: PatrolConfigSource falls back to the legacy path when the
// settings file does not parse, so it would name the wrong file. Any other
// failure names the patrol config's source.
func patrolFailureSource(townRoot string, err error) string {
	var pe *agentconfig.ParseError
	if errors.As(err, &pe) {
		return agentconfig.SourcePathRel(townRoot, pe.Path)
	}
	return PatrolConfigSource(townRoot)
}

// collectConfigParseFailed raises one item while the patrol config file does
// not load and the daemon runs on the config it had. The next reload that loads
// the file clears it.
func (s *attentionSources) collectConfigParseFailed(context.Context) ([]attention.Item, error) {
	if s.patrolFailure == nil {
		return nil, nil
	}
	return []attention.Item{{
		Key:      configParseFailedKey,
		Kind:     attention.KindConfigParseFailed,
		Severity: attention.SeverityHigh,
		Summary: fmt.Sprintf("%s does not load, so the daemon keeps the patrol config it had before the edit: fix the file, and the daemon picks it up at its next heartbeat",
			s.patrolFailure.source),
	}}, nil
}
