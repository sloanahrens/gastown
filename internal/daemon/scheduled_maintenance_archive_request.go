package daemon

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// The archive-now request (gt-xr2byt): the operator's command writes
// archive-request in the town's update directory, and the next
// scheduled_maintenance tick runs the history archive pass alone and answers in
// archive-result.json. The pass is maintenanceArchive, the same function the
// 03:00 window calls, in the gc cycle's single-flight slot; nothing here backs
// up, gc's or consumes the maintenance window.

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
// The tick has no caller to return to, so a failure is logged with the run id:
// the request stays and the command sees its timeout.
func (d *Daemon) answerArchive(req update.RequestFile, outcome maintenanceGCOutcome, detail string) {
	if err := answerArchiveRequest(osRequestFiles{}, update.Dir(d.config.TownRoot), req.RunID, d.maintenance().now(), outcome.String(), detail); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer the archive request for run %s: %v — the request stays", req.RunID, err)
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
// up to: a database the pass did not archive (its export failed, or a
// configuration failure marked every database) makes the request failed, while
// the others are reported archived.
func archiveRequestDetail(databases []string, unarchived map[string]bool) (string, maintenanceGCOutcome) {
	lines := make([]string, 0, len(databases))
	outcome := gcOutcomeCompleted
	for _, db := range databases {
		if unarchived[db] {
			outcome = gcOutcomeFailed
			lines = append(lines, db+": not archived; the daemon log names the failed export and the next run retries from the watermark")
			continue
		}
		lines = append(lines, db+": archived")
	}
	return strings.Join(lines, "\n"), outcome
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
		d.answerArchive(req, gcOutcomeDeferred, reason)
		return
	}
	if err := d.archiveRequestReady(); err != nil {
		d.answerArchive(req, gcOutcomeFailed, err.Error())
		return
	}
	if external, why := d.maintenance().gcExternal(d); external {
		d.answerArchive(req, gcOutcomeFailed, "no local server can run the archive: "+why)
		return
	}
	dataDir := d.maintenanceDataDir()
	databases, err := d.maintenance().gcDatabases(dataDir)
	if err != nil || len(databases) == 0 {
		detail := fmt.Sprintf("no databases discovered in %s", dataDir)
		if err != nil {
			detail = fmt.Sprintf("%s: %v", detail, err)
		}
		d.answerArchive(req, gcOutcomeFailed, detail)
		return
	}
	d.logger.Printf("scheduled_maintenance: archive request for run %s: %d database(s) discovered", req.RunID, len(databases))
	d.maintenance().dispatch(func() {
		defer d.maintenanceGCRunning.Store(false)
		res, unarchived := d.maintenanceArchive(databases)
		if res.outcome == gcOutcomeDeferred {
			d.logger.Printf("scheduled_maintenance: archive request for run %s deferred: %s — the request stays", req.RunID, res.reason)
			return
		}
		detail, outcome := archiveRequestDetail(databases, unarchived)
		d.logger.Printf("scheduled_maintenance: archive request for run %s %s", req.RunID, outcome)
		d.answerArchive(req, outcome, detail)
	})
	dispatched = true
}
