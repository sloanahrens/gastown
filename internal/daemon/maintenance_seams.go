package daemon

import "context"

// maintenanceSeams are scheduled maintenance's side effects and host reads,
// replaceable in tests through Daemon.maint. Each nil field is the production
// behavior (see maintenance): a test sets only the ones it drives, on its own
// Daemon, so tests of different daemons never share one.
type maintenanceSeams struct {
	// exec runs `gt maintain --force --threshold N`: flatten mode's
	// destructive path, which monitor mode must never reach.
	exec func(ctx context.Context, gtPath, dir string, threshold int) ([]byte, error)
	// escalate reports maintenance findings to the mayor.
	escalate func(d *Daemon, source, message string)
	// convoyPause pauses the ConvoyManager's Dolt reads for one database's gc.
	convoyPause func(d *Daemon) (resume func(), ok bool)
	// gcDatabases discovers gc mode's databases under a data dir.
	gcDatabases func(dataDir string) ([]string, error)
	// gcExternal reports whether the Dolt server is not one this daemon can
	// measure on local disk.
	gcExternal func(d *Daemon) (bool, string)
	// dbSize measures a database's on-disk size.
	dbSize func(dataDir, db string) (int64, error)
	// gcExec runs CALL dolt_gc('--full') on one database.
	gcExec func(ctx context.Context, d *Daemon, db string) error
	// quiet is the quiet-window guard, re-checked before each database.
	quiet func(d *Daemon) (bool, string)
	// slotHolders lists the roles holding a container-gate slot.
	slotHolders func(townRoot string) ([]string, error)
	// workingPolecats lists polecats with a fresh "working" heartbeat.
	workingPolecats func(d *Daemon) ([]string, error)
	// dispatch runs fn off the calling goroutine: a gc cycle, which can hold
	// a --full gc for up to maintenanceGCTimeout per database and would freeze
	// the heartbeat inline (gt-uvxy), and the overdue-gc escalation, which
	// runs under the Dolt manager's lock and must not wait on gt escalate.
	dispatch func(fn func())
}

// maintenance returns d's maintenance seams with every unset one filled in
// with its production behavior.
func (d *Daemon) maintenance() maintenanceSeams {
	s := d.maint
	if s.exec == nil {
		s.exec = runGtMaintain
	}
	if s.escalate == nil {
		s.escalate = (*Daemon).escalate
	}
	if s.convoyPause == nil {
		s.convoyPause = pauseConvoyForGC
	}
	if s.gcDatabases == nil {
		s.gcDatabases = discoverMaintenanceDatabases
	}
	if s.gcExternal == nil {
		s.gcExternal = (*Daemon).maintenanceGCExternal
	}
	if s.dbSize == nil {
		s.dbSize = maintenanceDBSize
	}
	if s.gcExec == nil {
		s.gcExec = func(ctx context.Context, d *Daemon, db string) error { return d.doltGCFull(ctx, db) }
	}
	if s.quiet == nil {
		s.quiet = (*Daemon).maintenanceQuiet
	}
	if s.slotHolders == nil {
		s.slotHolders = maintenanceSlotHolders
	}
	if s.workingPolecats == nil {
		s.workingPolecats = (*Daemon).workingPolecats
	}
	if s.dispatch == nil {
		s.dispatch = func(fn func()) { go fn() }
	}
	return s
}
