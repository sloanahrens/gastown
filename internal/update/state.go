// Package update is the record of one `gt update` run: the file the job
// rewrites at every step and the daemon and dashboard read.
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/constants"
)

// stateVersion is the only record version this package writes and reads; a
// reader refuses a later version rather than guess at fields it does not know.
const stateVersion = 1

// stateFileName is the record's name inside the state directory.
const stateFileName = "state.json"

// stagingSeq numbers the staging files this process has published, so two
// stores in one process never share a staging name.
var stagingSeq atomic.Uint64

const (
	dirMode  os.FileMode = 0o700
	fileMode os.FileMode = 0o600
)

// ErrNoState reports that no run has written state.json.
var ErrNoState = errors.New("no update state")

// ErrNoCurrent reports a mutation before this store's first Write.
var ErrNoCurrent = errors.New("no current update state")

// Clock reports the current time; a run takes every timestamp from it so a
// test can substitute a fixed one.
type Clock interface{ Now() time.Time }

// Outcome is how a run ended.
type Outcome string

// Run outcomes.
const (
	OutcomeRunning     Outcome = "running"
	OutcomeOK          Outcome = "ok"
	OutcomePartial     Outcome = "partial"
	OutcomeAborted     Outcome = "aborted"
	OutcomeFailed      Outcome = "failed"
	OutcomeInterrupted Outcome = "interrupted"
)

// StepName names one step of an update run.
type StepName string

// Update steps, in the order a run works through them.
const (
	StepPlan        StepName = "plan"
	StepDrain       StepName = "drain"
	StepMaintenance StepName = "maintenance"
	StepInstall     StepName = "install"
	StepRestart     StepName = "restart"
	StepVerify      StepName = "verify"
	StepLift        StepName = "lift"
)

// StepResult is where one step stands.
type StepResult string

// Step results.
const (
	ResultPending  StepResult = "pending"
	ResultRunning  StepResult = "running"
	ResultOK       StepResult = "ok"
	ResultSkipped  StepResult = "skipped"
	ResultFailed   StepResult = "failed"
	ResultDeferred StepResult = "deferred"
)

// Run sources.
const (
	SourceManual = "manual"
	SourceDaemon = "daemon"
)

// StepRecord is one step of the plan and how it went.
type StepRecord struct {
	Name      StepName   `json:"name"`
	WillRun   bool       `json:"will_run"`
	Reason    string     `json:"reason"`
	Result    StepResult `json:"result"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   time.Time  `json:"ended_at"`
	Detail    string     `json:"detail"`
}

// Target is the build the run set out to install.
type Target struct {
	SHA          string `json:"sha"`
	InstalledSHA string `json:"installed_sha"`
}

// HoldRecord says whether the run owns the dispatch hold file.
type HoldRecord struct {
	Owned bool   `json:"owned"`
	Note  string `json:"note"`
}

// State is the record one update run writes at every step.
type State struct {
	V           int          `json:"v"`
	RunID       string       `json:"run_id"`
	Source      string       `json:"source"` // "manual" or "daemon"
	StartedAt   time.Time    `json:"started_at"`
	HeartbeatAt time.Time    `json:"heartbeat_at"`
	EndedAt     time.Time    `json:"ended_at"`
	Target      Target       `json:"target"`
	Steps       []StepRecord `json:"steps"`
	Current     StepName     `json:"current"`
	Hold        HoldRecord   `json:"hold"`
	Outcome     Outcome      `json:"outcome"`
}

// Dir returns <town>/.runtime/update, where a run keeps its state file.
func Dir(townRoot string) string {
	return filepath.Join(constants.TownRuntimePath(townRoot), "update")
}

// Store is one process's handle on the run's state file.
type Store struct {
	// Dir is the state directory; Write creates it owner-only.
	Dir string
	// Rename publishes a new record; nil means os.Rename. A test replaces it
	// to make the publish fail.
	Rename func(oldpath, newpath string) error
	// openHistory opens history.jsonl for appending; nil means os.OpenFile.
	// A test replaces it to make the open, the write or the close fail.
	openHistory func(path string) (historyWriter, error)

	mu      sync.Mutex
	current *State
	// write writes the staging file; nil means atomicfile.WriteFile. A test
	// replaces it to make the staging write fail.
	write func(path string, data []byte, perm os.FileMode) error
}

// Write records st as the current state and publishes it atomically to
// state.json. On failure state.json keeps the record it held before the call,
// the store's current record is left as it was, and the staging file is
// removed, or named in the error if it cannot be.
func (s *Store) Write(st State) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.publish(st)
}

// Mutate applies fn to the current state under the store's lock, publishes the
// result and returns a copy.
func (s *Store) Mutate(fn func(*State)) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return State{}, ErrNoCurrent
	}
	next := cloneState(*s.current)
	fn(&next)
	if err := s.publish(next); err != nil {
		return State{}, err
	}
	return cloneState(next), nil
}

// Read returns state.json as it stands on disk, for a process other than the
// run.
func (s *Store) Read() (State, error) {
	path := s.path()
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return State{}, fmt.Errorf("%s: %w", path, ErrNoState)
	}
	if err != nil {
		return State{}, fmt.Errorf("read update state %s: %w", path, err)
	}
	var st State
	if err := json.Unmarshal(data, &st); err != nil {
		return State{}, fmt.Errorf("parse update state %s: %w", path, err)
	}
	if st.V != stateVersion {
		return State{}, fmt.Errorf("update state %s: unsupported version %d, want %d", path, st.V, stateVersion)
	}
	return st, nil
}

// publish stamps the version and replaces state.json in one rename. The caller
// holds s.mu.
func (s *Store) publish(st State) error {
	st.V = stateVersion
	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		return fmt.Errorf("create update state directory %s: %w", s.Dir, err)
	}
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return fmt.Errorf("encode update state: %w", err)
	}
	// The staging name is unique to this publish: a second process writing
	// the record at the same time stages its own file, so its rename cannot
	// take this publish's staging file away.
	staging := filepath.Join(s.Dir, fmt.Sprintf("%s.next.%d.%d", stateFileName, os.Getpid(), stagingSeq.Add(1)))
	// The staging write is atomic, and the rename publishes it whole: a reader
	// sees the previous record or the complete new one, never a partial file.
	write := s.write
	if write == nil {
		write = atomicfile.WriteFile
	}
	if err := write(staging, data, fileMode); err != nil {
		return discardStaging(staging, fmt.Errorf("write update state: %w", err))
	}
	rename := s.Rename
	if rename == nil {
		rename = os.Rename
	}
	if err := rename(staging, s.path()); err != nil {
		return discardStaging(staging, fmt.Errorf("publish update state: %w", err))
	}
	// Adopt only what reached the disk, so a failed publish leaves the
	// previous record current.
	adopted := cloneState(st)
	s.current = &adopted
	return nil
}

// path is state.json inside Dir.
func (s *Store) path() string {
	return filepath.Join(s.Dir, stateFileName)
}

// discardStaging removes the staging file a failed publish left behind and
// returns err, naming the removal failure too when the file cannot be removed.
func discardStaging(staging string, err error) error {
	if rmErr := os.Remove(staging); rmErr != nil && !errors.Is(rmErr, fs.ErrNotExist) {
		return fmt.Errorf("%w (remove staging file: %v)", err, rmErr)
	}
	return err
}

// cloneState copies st so the caller cannot alias the steps slice the store
// keeps.
func cloneState(st State) State {
	st.Steps = slices.Clone(st.Steps)
	return st
}
