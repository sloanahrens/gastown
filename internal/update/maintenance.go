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

// ArchiveRequestFileName is the archive-now command's request file: the
// history archive pass alone, under a name of its own so a daemon that predates
// archive-now never reads it and the command times out still pending
// (gt-xr2byt).
const ArchiveRequestFileName = "archive-request"

// ArchiveResultFileName is the daemon's answer to ArchiveRequestFileName.
const ArchiveResultFileName = "archive-result.json"

// requestPair names one request and result file and the word the protocol's
// error text uses for them.
type requestPair struct {
	request string
	result  string
	kind    string
}

func maintenancePair() requestPair {
	return requestPair{request: RequestFileName, result: ResultFileName, kind: "maintenance"}
}

func archivePair() requestPair {
	return requestPair{request: ArchiveRequestFileName, result: ArchiveResultFileName, kind: "archive"}
}

// requestPath is the request file inside dir.
func (p requestPair) requestPath(dir string) string { return filepath.Join(dir, p.request) }

// resultPath is the result file inside dir.
func (p requestPair) resultPath(dir string) string { return filepath.Join(dir, p.result) }

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

// MaintFiles is the three file operations the request protocol needs, so a
// test drives every branch against a fake and the protocol borrows no other
// step's interface.
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

// ArchiveRequest is the archive-now command's handle on the archive request and
// result files: the same exclusive-create and wait protocol as
// MaintenanceRequest, over the archive pair (gt-xr2byt).
type ArchiveRequest struct {
	// Dir is the .runtime/update directory.
	Dir string
	// Files is the file system both files are read and written through.
	Files MaintFiles
	// Clock stamps the request and measures the wait.
	Clock Clock
}

// Request asks the daemon for a maintenance cycle for runID.
//
// A request file already holding runID makes the call a no-op; one holding
// another run id is an error, because that job is still waiting for it. Only
// once this call owns the request does it clear a result file left by an
// earlier run, so a result another job is still waiting to read is never
// destroyed.
func (m *MaintenanceRequest) Request(runID string) error {
	return requestCycle(m.Dir, maintenancePair(), m.Files, m.Clock, runID)
}

// Request asks the daemon for the history archive pass for runID, with the
// same rules as MaintenanceRequest.Request.
func (a *ArchiveRequest) Request(runID string) error {
	return requestCycle(a.Dir, archivePair(), a.Files, a.Clock, runID)
}

// requestCycle is Request over one file pair.
func requestCycle(dir string, p requestPair, files MaintFiles, clock Clock, runID string) error {
	if runID == "" {
		return fmt.Errorf("write %s request: empty run id", p.kind)
	}
	data, err := json.Marshal(RequestFile{RunID: runID, At: clock.Now()})
	if err != nil {
		// A clock time outside the year range JSON can carry is the one way
		// this fails; refuse rather than write a request the daemon cannot parse.
		return fmt.Errorf("encode %s request: %w", p.kind, err)
	}
	reqPath := p.requestPath(dir)
	if err := files.CreateExclusive(reqPath, data, fileMode); err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return fmt.Errorf("create %s request %s: %w", p.kind, reqPath, err)
		}
		return settleExistingRequest(files, p, reqPath, runID)
	}
	resultPath := p.resultPath(dir)
	if err := files.Remove(resultPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Release the request this call just created: a retry must retry the
		// removal, not return nil as if the stale result were gone.
		releaseErr := files.Remove(reqPath)
		if releaseErr != nil && !errors.Is(releaseErr, fs.ErrNotExist) {
			return fmt.Errorf("clear stale %s result %s: %w; release request %s: %v", p.kind, resultPath, err, reqPath, releaseErr)
		}
		return fmt.Errorf("clear stale %s result %s: %w", p.kind, resultPath, err)
	}
	return nil
}

// settleExistingRequest decides what a request file that already exists means
// for runID: nil when it is this run's own, an error naming the other run
// otherwise.
func settleExistingRequest(files MaintFiles, p requestPair, path, runID string) error {
	data, err := files.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read existing %s request %s: %w", p.kind, path, err)
	}
	var req RequestFile
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("parse existing %s request %s: %w; remove %s to recover", p.kind, path, err, path)
	}
	if req.RunID == runID {
		return nil
	}
	return fmt.Errorf("%s request %s is for run %q: another update job is waiting for it", p.kind, path, req.RunID)
}

// Wait polls the request file through sleep until it is gone, then returns the
// daemon's result for runID.
//
// A missing request means the daemon finished; a result for another run id, or
// no result at all, is reported as Outcome "failed" rather than passed on. At
// timeout the result is Outcome "deferred" with the request left in place for
// the daemon's next tick. A canceled context returns its error.
func (m *MaintenanceRequest) Wait(ctx context.Context, sleep func(context.Context, time.Duration) error, runID string, timeout, poll time.Duration) (MaintenanceResult, error) {
	return waitForCycle(ctx, m.Dir, maintenancePair(), m.Files, m.Clock, sleep, runID, timeout, poll)
}

// Wait polls the archive request file exactly as MaintenanceRequest.Wait polls
// the maintenance one and returns the daemon's answer.
func (a *ArchiveRequest) Wait(ctx context.Context, sleep func(context.Context, time.Duration) error, runID string, timeout, poll time.Duration) (MaintenanceResult, error) {
	return waitForCycle(ctx, a.Dir, archivePair(), a.Files, a.Clock, sleep, runID, timeout, poll)
}

// requestPath is the maintenance request file inside Dir.
func (m *MaintenanceRequest) requestPath() string { return maintenancePair().requestPath(m.Dir) }

// resultPath is the maintenance result file inside Dir.
func (m *MaintenanceRequest) resultPath() string { return maintenancePair().resultPath(m.Dir) }

// waitForCycle is Wait over one file pair.
func waitForCycle(ctx context.Context, dir string, p requestPair, files MaintFiles, clock Clock, sleep func(context.Context, time.Duration) error, runID string, timeout, poll time.Duration) (MaintenanceResult, error) {
	if runID == "" {
		return MaintenanceResult{}, fmt.Errorf("wait for %s result: empty run id", p.kind)
	}
	start := clock.Now()
	reqPath := p.requestPath(dir)
	for {
		if err := ctx.Err(); err != nil {
			return MaintenanceResult{}, err
		}
		_, err := files.ReadFile(reqPath)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			return readResult(p, files, dir, runID), nil
		case err != nil:
			// An unreadable request is not a finished one: report it instead of
			// reading the result as if the daemon had answered.
			return MaintenanceResult{}, fmt.Errorf("read %s request %s: %w", p.kind, reqPath, err)
		}
		now := clock.Now()
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
func readResult(p requestPair, files MaintFiles, dir, runID string) MaintenanceResult {
	path := p.resultPath(dir)
	data, err := files.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("the daemon removed the %s request without writing %s", p.kind, p.result)}
	case err != nil:
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("read %s result %s: %v", p.kind, path, err)}
	}
	var res MaintenanceResult
	if err := json.Unmarshal(data, &res); err != nil {
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("parse %s result %s: %v", p.kind, path, err)}
	}
	if res.RunID != runID {
		return MaintenanceResult{Outcome: maintenanceFailed, Detail: fmt.Sprintf("%s result %s is for run %q, not %q", p.kind, path, res.RunID, runID)}
	}
	return res
}
