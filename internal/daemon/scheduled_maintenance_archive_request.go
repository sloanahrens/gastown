package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// The archive-now request (gt-xr2byt): the command writes the archive-request
// file in the town's update directory, and the next scheduled_maintenance tick
// runs the history archive pass alone and answers in archive-result.json. The
// pass is maintenanceArchive, the same function the 03:00 window calls, run in
// the gc cycle's single-flight slot. Nothing here backs up, starts a gc, or
// consumes the maintenance window.

func archiveRequestPath(dir string) string {
	return filepath.Join(dir, update.ArchiveRequestFileName)
}

func archiveResultPath(dir string) string {
	return filepath.Join(dir, update.ArchiveResultFileName)
}

// readArchiveRequest reads dir's archive request. A missing file is
// (zero, false, nil); the read and parse failures are the maintenance request's
// (errMalformedRequest, errInvalidRunID, or the read error).
func readArchiveRequest(files requestFiles, dir string) (update.RequestFile, bool, error) {
	return readRequestAt(files, archiveRequestPath(dir), "archive")
}

// answerArchiveRequest writes dir's archive result for runID and only then
// removes the request it served.
func answerArchiveRequest(files requestFiles, dir, runID string, at time.Time, outcome, detail string) error {
	return answerRequestAt(files, archiveRequestPath(dir), archiveResultPath(dir), "archive", runID, at, outcome, detail)
}

// answerInvalidArchiveRequest answers an archive request whose run id is
// untrusted, without echoing the id.
func (d *Daemon) answerInvalidArchiveRequest(files requestFiles, dir string, cause error) {
	d.answerInvalidRequestAt(files, archiveRequestPath(dir), archiveResultPath(dir), "archive", d.maintenance().now(), cause)
}

// answerArchive applies answerArchiveRequest to the town's update directory.
// The tick has no caller to return to, so a failure is logged with the run id.
//
// When the answer cannot be written the request is dropped, not left for the
// next tick: the pass it asked for has already run (or was refused before
// running), and leaving the request would run the whole archive again every
// five minutes for as long as the write keeps failing. The command then finds
// the request gone with no result and reports that the daemon removed it
// without answering, which is what happened.
func (d *Daemon) answerArchive(req update.RequestFile, outcome, detail string) {
	files := osRequestFiles{}
	dir := update.Dir(d.config.TownRoot)
	if err := answerArchiveRequest(files, dir, req.RunID, d.maintenance().now(), outcome, detail); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer the archive request for run %s: %v", req.RunID, err)
		if err := removeAnsweredRequestAt(files, archiveRequestPath(dir), "archive", req.RunID); err != nil {
			d.logger.Printf("scheduled_maintenance: WARNING: cannot drop the unanswered archive request for run %s: %v — the next tick serves it again", req.RunID, err)
			return
		}
		d.logger.Printf("scheduled_maintenance: WARNING: the archive request for run %s was dropped instead of left for the next tick", req.RunID)
	}
}

// archiveRequestReady reports why the history archive cannot run at all: a
// directory that cannot be parsed, or one left unset. maintenanceArchive treats
// an unset directory as "off" and completes without archiving, which an
// archive-now answer must not report as done.
func (d *Daemon) archiveRequestReady() error {
	dir, err := d.maintenanceArchiveDir()
	if err != nil {
		return fmt.Errorf("the history archive is misconfigured: %w", err)
	}
	if dir == "" {
		return fmt.Errorf("the history archive is off: %s is unset in the daemon config", maintenanceArchiveDirKey)
	}
	return nil
}

// archiveRequestDetail renders one line per database and the outcome they add
// up to: completed only when every export of every database is caught up,
// partial when nothing failed but an export stopped at the budget or the
// per-cycle batch bound with history still behind, and failed when any export
// failed. A stop is never reported as caught up.
func archiveRequestDetail(databases []string, outcomes map[string]archiveDatabaseOutcome) (string, string) {
	lines := make([]string, 0, len(databases))
	outcome := "completed"
	for _, db := range databases {
		o := outcomes[db]
		lines = append(lines, fmt.Sprintf("%s: commits archived %d (%s); events archived %d (%s)",
			db, o.commits.archived, archiveExportState(o.commits), o.events.archived, archiveExportState(o.events)))
		switch {
		case o.commits.err != nil || o.events.err != nil:
			outcome = "failed"
		case !archiveExportCaughtUp(o.commits) || !archiveExportCaughtUp(o.events):
			if outcome == "completed" {
				outcome = "partial"
			}
		}
	}
	return strings.Join(lines, "\n"), outcome
}

// archiveExportCaughtUp reports whether an export is known to have nothing more
// behind: it read a short batch, or a full batch it could not advance past
// (nothing is known to be behind, as the cycle's log says).
func archiveExportCaughtUp(o archiveExportOutcome) bool {
	return o.err == nil && (o.stop == archiveStopShort || o.stop == archiveStopNoProgress)
}

// archiveExportState is one export's word in a detail line: caught up, a stop
// that leaves history behind, or the failed export's error text.
func archiveExportState(o archiveExportOutcome) string {
	switch {
	case o.err != nil:
		return "failed: " + o.err.Error()
	case o.stop == archiveStopBudget:
		return "stopped at the archive_budget, still behind"
	case o.stop == archiveStopCapped:
		return "stopped at the per-cycle batch bound, still behind"
	default:
		return "caught up"
	}
}

// serveArchiveRequest runs the history archive pass for req and answers it,
// without doing any of the cycle's other work. The tick calls it only after
// its patrol-activity gate, so the patrol has been checked by then.
//
// A cycle already running holds the single-flight slot, so the request stays
// for the next tick. A standby answers deferred at once rather than leaving the
// command to its timeout. A busy town defers the pass itself: maintenanceArchive
// returns deferred before reading any history, and the request stays for the
// next tick, as the update job's request does in a gc cycle.
func (d *Daemon) serveArchiveRequest(req update.RequestFile) {
	if !d.maintenanceGCRunning.CompareAndSwap(false, true) {
		// A cycle holds the slot and this tick must not answer a request that
		// may still be served by it: the request stays for a later tick.
		d.logger.Printf("scheduled_maintenance: archive request for run %s waits: a maintenance cycle is running", req.RunID)
		return
	}
	dispatched := false
	defer func() {
		if !dispatched {
			d.maintenanceGCRunning.Store(false)
		}
	}()
	if reason := d.standbyReason(d.maintenance().now()); reason != "" {
		d.answerArchive(req, "deferred", reason)
		return
	}
	if err := d.archiveRequestReady(); err != nil {
		d.answerArchive(req, "failed", err.Error())
		return
	}
	if external, why := d.maintenance().gcExternal(d); external {
		d.answerArchive(req, "failed", "no local server can run the archive: "+why)
		return
	}
	dataDir := d.maintenanceDataDir()
	databases, err := d.maintenance().gcDatabases(dataDir)
	if err != nil || len(databases) == 0 {
		detail := fmt.Sprintf("no databases discovered in %s", dataDir)
		if err != nil {
			detail = fmt.Sprintf("%s: %v", detail, err)
		}
		d.answerArchive(req, "failed", detail)
		return
	}
	d.logger.Printf("scheduled_maintenance: archive request for run %s: %d database(s) discovered", req.RunID, len(databases))
	d.maintenance().dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		res, _, outcomes := d.maintenanceArchive(databases)
		if res.outcome == gcOutcomeDeferred {
			d.logger.Printf("scheduled_maintenance: archive request for run %s deferred: %s — the request stays", req.RunID, res.reason)
			return
		}
		detail, outcome := archiveRequestDetail(databases, outcomes)
		d.logger.Printf("scheduled_maintenance: archive request for run %s %s", req.RunID, outcome)
		d.answerArchive(req, outcome, detail)
	})
	dispatched = true
}
