package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// RequestFileName is the file the job uses to ask the daemon for a
// maintenance cycle.
const RequestFileName = "maintenance-request"

// ResultFileName is the file the daemon writes the cycle's outcome to before
// it removes the request.
const ResultFileName = "maintenance-result.json"

// Outcomes Wait reports itself; a daemon-written result may also carry
// "completed", which Wait returns unchanged.
const (
	maintenanceDeferred = "deferred"
	maintenanceFailed   = "failed"
)

// RequestFile is the content of the request file.
type RequestFile struct {
	RunID string    `json:"run_id"`
	At    time.Time `json:"at"`
}

// MaintenanceResult is the content of the result file.
type MaintenanceResult struct {
	RunID   string    `json:"run_id"`
	Outcome string    `json:"outcome"` // "completed", "deferred" or "failed"
	At      time.Time `json:"at"`
	Detail  string    `json:"detail"`
}

// MaintFiles is the file system the request and result files are read and
// written through, so a test drives every branch against a fake. It is a
// smaller interface than the hold's FileOps on purpose: the two protocols do
// not share a file.
type MaintFiles interface {
	ReadFile(path string) ([]byte, error)
	// CreateExclusive creates path and fails with an error satisfying
	// errors.Is(err, fs.ErrExist) when it already exists, so a request from a
	// second job is never overwritten.
	CreateExclusive(path string, data []byte, mode os.FileMode) error
	Remove(path string) error
}

// MaintenanceRequest is one run's handle on the request and result files in
// the update directory.
type MaintenanceRequest struct {
	// Dir is the .runtime/update directory.
	Dir string
	// Files is the file system both files are read and written through.
	Files MaintFiles
	// Clock stamps the request and measures the wait.
	Clock Clock
}

// Request asks the daemon for a maintenance cycle for runID.
//
// It clears a result file left by an earlier run first, so Wait can never read
// one that predates this request. A request file already holding runID makes
// the call a no-op; one holding another run id is an error, because that job
// is still waiting for it.
func (m *MaintenanceRequest) Request(runID string) error {
	if runID == "" {
		return errors.New("write maintenance request: empty run id")
	}
	resultPath := m.resultPath()
	if err := m.Files.Remove(resultPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear stale maintenance result %s: %w", resultPath, err)
	}
	data, err := json.Marshal(RequestFile{RunID: runID, At: m.Clock.Now()})
	if err != nil {
		// RequestFile holds only a string and a time, so this cannot fail; the
		// check keeps a future field from turning the error away silently.
		return fmt.Errorf("encode maintenance request: %w", err)
	}
	reqPath := m.requestPath()
	if err := m.Files.CreateExclusive(reqPath, data, fileMode); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create maintenance request %s: %w", reqPath, err)
		}
		return m.settleExistingRequest(reqPath, runID)
	}
	return nil
}

// settleExistingRequest decides what a request file that already exists means
// for runID: nil when it is this run's own, an error naming the other run
// otherwise.
func (m *MaintenanceRequest) settleExistingRequest(path, runID string) error {
	data, err := m.Files.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read existing maintenance request %s: %w", path, err)
	}
	var req RequestFile
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("parse existing maintenance request %s: %w", path, err)
	}
	if req.RunID == runID {
		return nil
	}
	return fmt.Errorf("maintenance request %s is for run %q: another update job is waiting for it", path, req.RunID)
}

// Wait polls the request file through sleep until it is gone, then returns the
// daemon's result for runID.
//
// A missing request means the daemon finished; a result for another run id, or
// no result at all, is reported as Outcome "failed" rather than passed on. At
// timeout the result is Outcome "deferred" with the request left in place for
// the daemon's next tick. A cancelled context returns its error.
func (m *MaintenanceRequest) Wait(ctx context.Context, sleep func(context.Context, time.Duration) error, runID string, timeout, poll time.Duration) (MaintenanceResult, error) {
	if runID == "" {
		return MaintenanceResult{}, errors.New("wait for maintenance result: empty run id")
	}
	start := m.Clock.Now()
	reqPath := m.requestPath()
	for {
		if err := ctx.Err(); err != nil {
			return MaintenanceResult{}, err
		}
		_, err := m.Files.ReadFile(reqPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return m.readResult(runID), nil
		case err != nil:
			// An unreadable request is not a finished one: report it instead of
			// reading the result as if the daemon had answered.
			return MaintenanceResult{}, fmt.Errorf("read maintenance request %s: %w", reqPath, err)
		}
		now := m.Clock.Now()
		if now.Sub(start) >= timeout {
			return MaintenanceResult{Outcome: maintenanceDeferred, Detail: "the daemon did not finish in time"}, nil
		}
		if err := sleep(ctx, poll); err != nil {
			return MaintenanceResult{}, err
		}
	}
}

// readResult reads the result file after the request is gone and returns it
// only when it answers this run; every other state is a failure the caller
// must not mistake for a completed cycle.
func (m *MaintenanceRequest) readResult(runID string) MaintenanceResult {
	path := m.resultPath()
	data, err := m.Files.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("the daemon removed the maintenance request without writing %s", ResultFileName)}
	case err != nil:
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("read maintenance result %s: %v", path, err)}
	}
	var res MaintenanceResult
	if err := json.Unmarshal(data, &res); err != nil {
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("parse maintenance result %s: %v", path, err)}
	}
	if res.RunID != runID {
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("maintenance result %s is for run %q, not %q", path, res.RunID, runID)}
	}
	return res
}

// requestPath is the request file inside Dir.
func (m *MaintenanceRequest) requestPath() string {
	return filepath.Join(m.Dir, RequestFileName)
}

// resultPath is the result file inside Dir.
func (m *MaintenanceRequest) resultPath() string {
	return filepath.Join(m.Dir, ResultFileName)
}
