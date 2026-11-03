package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gofrs/flock"
)

const (
	// historyFileName is where finished runs are appended, one JSON line each.
	historyFileName = "history.jsonl"
	// lockFileName is the exclusive lock a running update holds.
	lockFileName = "lock"
)

// ErrLocked reports that another `gt update` run holds the lock.
var ErrLocked = errors.New("another update run holds the lock")

// historyWriter is the open history file AppendHistory writes to. *os.File
// satisfies it; a test substitutes one whose write or close fails.
type historyWriter interface {
	io.Writer
	io.Closer
}

// HistoryLine is one finished run in history.jsonl: the state record it
// published plus how long each step took.
type HistoryLine struct {
	State
	// DurationsMS is each step's ended_at minus started_at in milliseconds,
	// keyed by step name. A step without both times is absent.
	DurationsMS map[string]int64 `json:"durations_ms"`
}

// AppendHistory appends st to history.jsonl as one JSON line. A run whose
// outcome is still "running" is not recorded, so a call at start is harmless.
// The directory and file are created owner-only when missing.
func (s *Store) AppendHistory(st State) error {
	if st.Outcome == OutcomeRunning {
		return nil
	}
	if err := os.MkdirAll(s.Dir, dirMode); err != nil {
		return fmt.Errorf("create update state directory %s: %w", s.Dir, err)
	}
	st.V = stateVersion
	line := HistoryLine{State: st, DurationsMS: stepDurations(st.Steps)}
	data, err := json.Marshal(line)
	if err != nil {
		return fmt.Errorf("encode update history: %w", err)
	}
	data = append(data, '\n')
	path := filepath.Join(s.Dir, historyFileName)
	open := s.openHistory
	if open == nil {
		open = func(path string) (historyWriter, error) {
			return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, fileMode)
		}
	}
	f, err := open(path)
	if err != nil {
		return fmt.Errorf("open update history %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		if closeErr := f.Close(); closeErr != nil {
			return fmt.Errorf("append update history %s: %w (close file: %v)", path, err, closeErr)
		}
		return fmt.Errorf("append update history %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close update history %s: %w", path, err)
	}
	return nil
}

// stepDurations maps each step that has both times to its length in
// milliseconds.
func stepDurations(steps []StepRecord) map[string]int64 {
	durations := make(map[string]int64, len(steps))
	for _, step := range steps {
		if step.StartedAt.IsZero() || step.EndedAt.IsZero() {
			continue
		}
		durations[string(step.Name)] = step.EndedAt.Sub(step.StartedAt).Milliseconds()
	}
	return durations
}

// RunHeartbeat keeps the run's heartbeat fresh while a long step runs, so a
// hung job looks different from a slow one. On every value received from ticks
// it sets HeartbeatAt through Store.Mutate, which serializes it with the run's
// own writes. An error from Mutate, including ErrNoCurrent before the run has
// written its first state, goes to onError when it is not nil and the loop
// keeps running. RunHeartbeat returns when ctx is done or ticks is closed. The
// caller supplies time.NewTicker(15 * time.Second).C; tests send ticks
// themselves.
func RunHeartbeat(ctx context.Context, s *Store, ticks <-chan time.Time, clock Clock, onError func(error)) {
	for {
		select {
		case <-ctx.Done():
			return
		case _, ok := <-ticks:
			if !ok {
				return
			}
			if _, err := s.Mutate(func(st *State) { st.HeartbeatAt = clock.Now() }); err != nil {
				if onError != nil {
					onError(err)
				}
			}
		}
	}
}

// AcquireLock takes the exclusive run lock in dir, creating the directory
// owner-only when missing. It returns ErrLocked when another holder has the
// lock. The returned release frees it, is safe to call more than once, and
// need not be called at all: the kernel drops the lock when the process exits.
func AcquireLock(dir string) (release func(), err error) {
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return nil, fmt.Errorf("create update directory %s: %w", dir, err)
	}
	lock := flock.New(filepath.Join(dir, lockFileName))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire update lock %s: %w", lock.Path(), err)
	}
	if !locked {
		return nil, ErrLocked
	}
	var once sync.Once
	return func() {
		// release reports no error, and the kernel drops the flock when the
		// process exits, so a failed Unlock has no caller to act on it.
		once.Do(func() { _ = lock.Unlock() })
	}, nil
}
