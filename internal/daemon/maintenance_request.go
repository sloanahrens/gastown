package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/update"
)

// The daemon side of the update job's run-now request (gt-o4sk8.12): the job
// writes maintenance-request in the town's update directory and waits for it
// to disappear, then reads maintenance-result.json. The result is written
// before the request is removed, so the job never sees the request gone
// without an answer.

// errMalformedRequest marks a request file that exists but cannot be answered
// (bad JSON or no run id). The tick removes it rather than retrying it every
// five minutes.
var errMalformedRequest = errors.New("malformed maintenance request")

// errInvalidRunID marks a request whose run id is not bounded to
// [A-Za-z0-9._-] and maxRequestRunIDLen characters. The tick answers it failed
// and removes it, but never logs or echoes the value.
var errInvalidRunID = errors.New("invalid maintenance run id")

// maxRequestRunIDLen bounds the untrusted run id so it cannot bloat the result
// file or a log line.
const maxRequestRunIDLen = 64

// requestFiles is the three file operations the protocol needs, so a test can
// prove the result-then-remove order against a recording fake.
type requestFiles interface {
	ReadFile(path string) ([]byte, error)
	WriteFile(path string, data []byte, mode os.FileMode) error
	Remove(path string) error
}

// osRequestFiles is the town's file system.
type osRequestFiles struct{}

func (osRequestFiles) ReadFile(path string) ([]byte, error) { return os.ReadFile(path) }

func (osRequestFiles) WriteFile(path string, data []byte, mode os.FileMode) error {
	return os.WriteFile(path, data, mode)
}

func (osRequestFiles) Remove(path string) error { return os.Remove(path) }

func requestPath(dir string) string { return filepath.Join(dir, update.RequestFileName) }

func resultPath(dir string) string { return filepath.Join(dir, update.ResultFileName) }

// readMaintenanceRequest reads dir's request. ok is false when the file does
// not exist; a file that exists but cannot be read or parsed returns an error,
// which errors.Is(err, errMalformedRequest) separates from a read failure.
func readMaintenanceRequest(files requestFiles, dir string) (update.RequestFile, bool, error) {
	path := requestPath(dir)
	data, err := files.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return update.RequestFile{}, false, nil
	case err != nil:
		return update.RequestFile{}, false, fmt.Errorf("read maintenance request %s: %w", path, err)
	}
	var req update.RequestFile
	if err := json.Unmarshal(data, &req); err != nil {
		return update.RequestFile{}, false, fmt.Errorf("%w: parse %s: %v", errMalformedRequest, path, err)
	}
	if req.RunID == "" {
		// The job never writes one, and an empty id could not be matched to the
		// result the job reads back.
		return update.RequestFile{}, false, fmt.Errorf("%w: %s has no run id", errMalformedRequest, path)
	}
	if !validRequestRunID(req.RunID) {
		// Never echo the value: the id is untrusted and could forge a log line
		// or bloat the result file.
		return update.RequestFile{}, false, fmt.Errorf("%w: %s", errInvalidRunID, path)
	}
	return req, true, nil
}

// validRequestRunID reports whether runID is bounded to letters, digits, dot,
// dash and underscore, and at most maxRequestRunIDLen characters. An empty id
// is rejected earlier, as malformed.
func validRequestRunID(runID string) bool {
	if len(runID) > maxRequestRunIDLen {
		return false
	}
	for i := 0; i < len(runID); i++ {
		c := runID[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '.', c == '-', c == '_':
		default:
			return false
		}
	}
	return true
}

// writeMaintenanceResult writes dir's result file for runID.
func writeMaintenanceResult(files requestFiles, dir, runID string, at time.Time, outcome, detail string) error {
	data, err := json.Marshal(update.MaintenanceResult{RunID: runID, Outcome: outcome, At: at, Detail: detail})
	if err != nil {
		return fmt.Errorf("encode maintenance result: %w", err)
	}
	path := resultPath(dir)
	if err := files.WriteFile(path, data, 0o600); err != nil {
		return fmt.Errorf("write maintenance result %s: %w", path, err)
	}
	return nil
}

// answerMaintenanceRequest writes the result and only then removes the
// request it served. A write failure leaves the request in place, so the next
// tick retries instead of the job reading a missing result.
func answerMaintenanceRequest(files requestFiles, dir, runID string, at time.Time, outcome, detail string) error {
	if err := writeMaintenanceResult(files, dir, runID, at, outcome, detail); err != nil {
		return err
	}
	return removeAnsweredRequest(files, dir, runID)
}

// removeAnsweredRequest removes dir's request only while it still holds
// runID. A cycle answers the request it started with, and a request written
// meanwhile is another job's: removing it by name would answer that job with
// the wrong result and drop work it is waiting for. A request already gone is
// answered; the job only needs the result, and the file's absence is the
// signal it waited for. A file that no longer parses cannot be the served
// request either, so it is left for the next tick to drop.
func removeAnsweredRequest(files requestFiles, dir, runID string) error {
	req, ok, err := readMaintenanceRequest(files, dir)
	switch {
	case errors.Is(err, errMalformedRequest), errors.Is(err, errInvalidRunID):
		return nil
	case err != nil:
		// The file cannot be read, so it cannot be shown to be the one this
		// answer belongs to: leaving it beats deleting another job's request.
		return err
	case !ok, req.RunID != runID:
		return nil
	}
	path := requestPath(dir)
	if err := files.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove maintenance request %s: %w", path, err)
	}
	return nil
}

// finishMaintenanceRequest answers a cycle the next tick will not retry: a
// completed or failed cycle writes its result and removes the request. A
// deferred or interrupted cycle leaves both, so the requested work happens on
// the next tick rather than being reported as finished.
func finishMaintenanceRequest(files requestFiles, dir string, at time.Time, runID string, outcome maintenanceGCOutcome, detail string) error {
	switch outcome {
	case gcOutcomeCompleted:
		return answerMaintenanceRequest(files, dir, runID, at, "completed", detail)
	case gcOutcomeFailed:
		return answerMaintenanceRequest(files, dir, runID, at, "failed", detail)
	}
	return nil
}

// answerRequest applies finishMaintenanceRequest to the town's update
// directory. The tick has no caller to return to, so a failure is logged with
// the run id: the request stays and the job sees its timeout.
func (d *Daemon) answerRequest(req update.RequestFile, outcome maintenanceGCOutcome, detail string) {
	d.maintenanceRequestMu.Lock()
	defer d.maintenanceRequestMu.Unlock()
	if err := finishMaintenanceRequest(osRequestFiles{}, update.Dir(d.config.TownRoot), d.maintenance().now(), req.RunID, outcome, detail); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer request for run %s: %v — the request stays", req.RunID, err)
	}
}

// deferRequest answers a request under a standby with a "deferred" result
// naming the standby, so the waiting job does not sit out its timeout for a
// cycle this tick will not start.
func (d *Daemon) deferRequest(req update.RequestFile, reason string) {
	d.maintenanceRequestMu.Lock()
	defer d.maintenanceRequestMu.Unlock()
	if err := answerMaintenanceRequest(osRequestFiles{}, update.Dir(d.config.TownRoot), req.RunID, d.maintenance().now(), "deferred", reason); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer request for run %s: %v — the request stays", req.RunID, err)
	}
}

// dropMalformedRequest removes a request the tick cannot answer and logs why.
func (d *Daemon) dropMalformedRequest(files requestFiles, dir string, cause error) {
	if requestReplaced(files, dir) {
		d.logger.Printf("scheduled_maintenance: %v — not dropped: a valid request replaced it", cause)
		return
	}
	path := requestPath(dir)
	if err := files.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.logger.Printf("scheduled_maintenance: WARNING: %v; cannot remove %s: %v", cause, path, err)
		return
	}
	d.logger.Printf("scheduled_maintenance: WARNING: %v — dropped", cause)
}

// answerInvalidRequest answers a request whose run id is untrusted with an
// id-less failed result, then removes it. The id itself is never written or
// logged: the job fails its own run-id match, which is the outcome its run
// deserves.
//
// It removes the file directly rather than through answerMaintenanceRequest:
// that removal is guarded by the served run id, and this request's id is the
// untrusted one that must not be echoed anywhere, including into the guard.
func (d *Daemon) answerInvalidRequest(files requestFiles, dir string, cause error) {
	if err := writeMaintenanceResult(files, dir, "", d.maintenance().now(), "failed", "the maintenance request has an invalid run id"); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: %v; cannot answer the request: %v — the request stays", cause, err)
		return
	}
	if requestReplaced(files, dir) {
		d.logger.Printf("scheduled_maintenance: %v — not dropped: a valid request replaced it", cause)
		return
	}
	path := requestPath(dir)
	if err := files.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.logger.Printf("scheduled_maintenance: WARNING: %v; cannot remove %s: %v", cause, path, err)
		return
	}
	d.logger.Printf("scheduled_maintenance: WARNING: %v — answered failed and dropped", cause)
}

// requestReplaced reports whether dir's request file now parses as a valid
// request: the malformed or invalid-id file a tick read has been replaced by
// another job's request since, and removing it by name would drop that job's
// work.
func requestReplaced(files requestFiles, dir string) bool {
	_, ok, err := readMaintenanceRequest(files, dir)
	return ok && err == nil
}

// answerBusyRequest answers a request that arrives while a cycle is already
// running, which this tick cannot serve. Only the request the running cycle is
// serving gets a "deferred" result and its file removed, so its job does not
// wait out a cycle that can hold one database's gc for minutes; that cycle's
// own result then replaces the deferral. A request the running cycle is not
// serving is left untouched, with no result written: the shared result file
// may still hold the running cycle's answer for its own job, and the request
// stays until a later tick runs its cycle.
//
// The request mutex makes the check and the write one step against the cycle's
// own answer: a cycle that already answered (its result names this run id, or
// it is no longer running) is never overwritten by a deferral.
func (d *Daemon) answerBusyRequest(req update.RequestFile) {
	d.maintenanceRequestMu.Lock()
	defer d.maintenanceRequestMu.Unlock()
	if !d.maintenanceGCRunning.Load() || d.runningRequestRunID() != req.RunID {
		return
	}
	files := osRequestFiles{}
	dir := update.Dir(d.config.TownRoot)
	if answered, err := resultNamesRun(files, dir, req.RunID); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer request for run %s: reading the result: %v — the request stays", req.RunID, err)
		return
	} else if answered {
		return
	}
	if err := writeMaintenanceResult(files, dir, req.RunID, d.maintenance().now(), "deferred", "a maintenance cycle is already running"); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer request for run %s: %v — the request stays", req.RunID, err)
		return
	}
	if err := removeAnsweredRequest(files, dir, req.RunID); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot remove the answered request for run %s: %v", req.RunID, err)
	}
}

// resultNamesRun reports whether dir's result file already answers runID. A
// missing file is no answer; an unreadable or unparseable one is an error, so
// the caller does not write over a result it could not see.
func resultNamesRun(files requestFiles, dir, runID string) (bool, error) {
	data, err := files.ReadFile(resultPath(dir))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	case err != nil:
		return false, err
	}
	var res update.MaintenanceResult
	if err := json.Unmarshal(data, &res); err != nil {
		return false, fmt.Errorf("parse %s: %w", resultPath(dir), err)
	}
	return res.RunID == runID && res.Outcome != "deferred", nil
}

// runningRequestRunID is the run id of the request the in-flight cycle is
// answering, empty when that cycle is a scheduled one (or none is running).
func (d *Daemon) runningRequestRunID() string {
	if v := d.maintenanceGCRequestRunID.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// gcRequestAnswer is the outcome of the last request-driven cycle, persisted in
// the maintenance state file so a request whose result write failed is
// re-answered on a later tick instead of running the cycle again. It assumes a
// run id names one job: a request that reuses a completed run id is answered
// from this record without a cycle, so the update job mints a fresh id per run.
type gcRequestAnswer struct {
	RunID   string    `json:"run_id"`
	Outcome string    `json:"outcome"` // "completed" or "failed"
	At      time.Time `json:"at"`
	Detail  string    `json:"detail"`
}

// rememberRequestAnswer records a completed or failed request-driven cycle
// before its result is written. A write failure is logged: the next tick then
// reruns the cycle, which is safe but costs a cycle.
func (d *Daemon) rememberRequestAnswer(runID string, outcome maintenanceGCOutcome, detail string, at time.Time) {
	if outcome != gcOutcomeCompleted && outcome != gcOutcomeFailed {
		return
	}
	_, err := updateMaintenanceGCState(d.config.TownRoot, func(st *maintenanceGCState) {
		st.LastRequestAnswer = &gcRequestAnswer{RunID: runID, Outcome: outcome.String(), At: at, Detail: detail}
	})
	if err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot remember the answer for run %s: %v — a later tick may rerun the cycle", runID, err)
	}
}

// reanswerRememberedRequest delivers the remembered outcome when req is the
// cycle that already ran for its run id. It reports true whenever the request
// belongs to a remembered cycle, whether or not the result write succeeds: a
// failed write leaves the request for the next tick's re-answer and must never
// run the cycle again.
//
// It also reports true when the state cannot be read: an unreadable state
// cannot show that this request was not already answered, and running the
// cycle again is the one outcome it must never produce. The request is
// answered failed with the reason instead.
func (d *Daemon) reanswerRememberedRequest(files requestFiles, dir string, req update.RequestFile) bool {
	st, err := loadMaintenanceGCState(d.config.TownRoot)
	if err != nil {
		d.answerRequest(req, gcOutcomeFailed, fmt.Sprintf("cannot read the maintenance state: %v", err))
		return true
	}
	ans := st.LastRequestAnswer
	if ans == nil || ans.RunID != req.RunID || !validRequestRunID(ans.RunID) {
		return false
	}
	if ans.Outcome != "completed" && ans.Outcome != "failed" {
		return false
	}
	if err := answerMaintenanceRequest(files, dir, ans.RunID, ans.At, ans.Outcome, ans.Detail); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer request for run %s: %v — the request stays", ans.RunID, err)
	}
	return true
}
