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
	return req, true, nil
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
// request. A write failure leaves the request in place, so the next tick
// retries instead of the job reading a missing result.
func answerMaintenanceRequest(files requestFiles, dir, runID string, at time.Time, outcome, detail string) error {
	if err := writeMaintenanceResult(files, dir, runID, at, outcome, detail); err != nil {
		return err
	}
	path := requestPath(dir)
	if err := files.Remove(path); err != nil {
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
	if err := finishMaintenanceRequest(osRequestFiles{}, update.Dir(d.config.TownRoot), d.maintenance().now(), req.RunID, outcome, detail); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer request for run %s: %v — the request stays", req.RunID, err)
	}
}

// deferRequest answers a request under a standby with a "deferred" result
// naming the standby, so the waiting job does not sit out its timeout for a
// cycle this tick will not start.
func (d *Daemon) deferRequest(req update.RequestFile, reason string) {
	if err := answerMaintenanceRequest(osRequestFiles{}, update.Dir(d.config.TownRoot), req.RunID, d.maintenance().now(), "deferred", reason); err != nil {
		d.logger.Printf("scheduled_maintenance: WARNING: cannot answer request for run %s: %v — the request stays", req.RunID, err)
	}
}

// dropMalformedRequest removes a request the tick cannot answer and logs why.
func (d *Daemon) dropMalformedRequest(files requestFiles, dir string, cause error) {
	path := requestPath(dir)
	if err := files.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		d.logger.Printf("scheduled_maintenance: WARNING: %v; cannot remove %s: %v", cause, path, err)
		return
	}
	d.logger.Printf("scheduled_maintenance: WARNING: %v — dropped", cause)
}
