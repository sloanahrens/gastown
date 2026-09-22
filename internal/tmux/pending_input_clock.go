package tmux

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// How long input has been waiting (gt-afa7).
//
// The stall verdict is a conjunction: unsubmitted input is visible in the pane
// AND a clock has run out on it. One of those clocks is #{window_activity},
// which any write to the pane resets — so every nudge typed into a stalled
// composer refreshes the clock that would have caught it. This clock lives
// outside the pane and is immune to that.
//
// It is not, on its own, evidence that anything is wrong: pane shape cannot
// separate a wedged composer from a working agent legitimately carrying a queued
// nudge (gt-hkhu), and input merely seen twice says nothing. So a run has to be
// EARNED before its age may be acted on, by three things this clock also
// records:
//
//   - continuity: several consecutive observations, spanning a minimum window,
//     rather than the first and latest samples a caller happens to take;
//   - identity: the tmux session id the run started in, so a stamp cannot
//     outlive the session that wrote it and date a fresh agent whose pane
//     happens to look the same;
//   - no progress: the pane's transcript region unchanged for the whole run,
//     so an agent that was working at any point in the window restarts the
//     clock rather than tripping it (see PaneProgressSignature).

// composerPendingSubdir is appended to the caller-supplied state directory.
// Callers pass <townRoot>/.runtime, putting these stamps beside the nudge
// queue's.
const composerPendingSubdir = "composer_pending"

// pendingInputMinSamples is how many consecutive observations must see the
// composer pending before the age clock may speak. Two samples cannot separate
// a run from a gap: input consumed and replaced between probes reads as one
// continuous wait (gt-afa7).
const pendingInputMinSamples = 3

// pendingInputMinSpan is the minimum wall-clock span those samples must cover.
// It is separate from the caller's frozen threshold so that a burst of fast
// re-probes — the witness re-checks a composer 750ms after submitting, three
// times — cannot accumulate the samples the threshold was meant to earn.
const pendingInputMinSpan = 2 * time.Minute

// PendingWait reports one observation of a composer holding unsubmitted input.
type PendingWait struct {
	// Waiting is how long the composer has been continuously observed holding
	// unsubmitted input. Zero on the first observation of a run, and zero when
	// no run is in progress.
	Waiting time.Duration
	// Samples is how many consecutive observations have seen it pending.
	Samples int
	// Continuous reports whether the run has earned the right to speak: at
	// least pendingInputMinSamples observations spanning at least
	// pendingInputMinSpan, uninterrupted by a progress change or a session
	// restart. Only a continuous run may trip the age clock.
	Continuous bool
}

// pendingStamp is the on-disk record of one run. JSON rather than a bare
// timestamp so the run can carry the identity and progress evidence that
// decide whether its age means anything.
type pendingStamp struct {
	// SessionID is the tmux session id the run started in ("$3"). A stamp whose
	// id no longer matches was written by a session that has since died.
	SessionID string `json:"session_id,omitempty"`
	// First is when the run started, in Unix seconds — the same representation
	// GetWindowActivity returns, so both clocks read alike in a log line.
	First int64 `json:"first_unix"`
	// Last is the previous observation, which is what the spans are measured
	// between.
	Last int64 `json:"last_unix"`
	// Samples counts the observations in this run, including the first.
	Samples int `json:"samples"`
	// Progress is the pane's transcript-region digest at the start of the run.
	// A different digest means the agent did something, so the run is over.
	Progress string `json:"progress,omitempty"`
}

// PendingInputClock tracks, per session, how long a composer has been
// continuously observed holding unsubmitted input.
//
// File-backed because its two callers are processes with different lifetimes —
// the daemon heartbeat and the witness patrol — and both must agree on when the
// wait started. A nil clock reports zero for every observation.
type PendingInputClock struct {
	dir string
}

// NewPendingInputClock returns a clock storing its stamps under
// <stateDir>/composer_pending, or nil when stateDir is empty. A clock with
// nowhere to persist is worse than none: the two callers would each measure
// their own wait and neither would reach the threshold.
func NewPendingInputClock(stateDir string) *PendingInputClock {
	if strings.TrimSpace(stateDir) == "" {
		return nil
	}
	return &PendingInputClock{dir: filepath.Join(stateDir, composerPendingSubdir)}
}

// Observe records that session is holding unsubmitted input at now, and returns
// how long it has been continuously doing so.
//
// sessionID is the tmux session id and progress the pane's transcript-region
// digest (PaneProgressSignature); either being empty means the caller could not
// measure it, which starts a fresh run rather than extending an old one. Same
// for a stamp written in a different session, or one whose progress digest has
// moved: both mean the run this stamp describes is over.
//
// It reports no error: a clock that cannot be read or written must not change a
// stall verdict, so the worst case here is a fresh run, which falls back to the
// silence window. An unreadable stamp reads as "no run in progress".
func (c *PendingInputClock) Observe(session, sessionID, progress string, now time.Time) PendingWait {
	if c == nil || strings.TrimSpace(session) == "" {
		return PendingWait{}
	}
	path := c.path(session)
	if path == "" {
		return PendingWait{}
	}

	// An unmeasurable identity is not a run we may date: a stamp we cannot tie
	// to this session could belong to a previous agent in the same worktree.
	if sessionID == "" || progress == "" {
		_ = os.Remove(path)
		return PendingWait{}
	}

	prev, ok := c.read(path)
	if ok && (prev.SessionID != sessionID || prev.Progress != progress || prev.Samples <= 0 || prev.First <= 0) {
		ok = false // A different session, or the agent did something. Start over.
	}
	if !ok {
		c.write(path, pendingStamp{
			SessionID: sessionID,
			First:     now.Unix(),
			Last:      now.Unix(),
			Samples:   1,
			Progress:  progress,
		})
		return PendingWait{Samples: 1}
	}

	first := time.Unix(prev.First, 0)
	wait := PendingWait{Samples: prev.Samples + 1}
	if age := now.Sub(first); age > 0 {
		wait.Waiting = age
	}
	// The span is measured across observations, not to now: a burst of samples
	// taken seconds apart has seen one moment of the pane, not a window of it.
	span := time.Unix(prev.Last, 0).Sub(first)
	wait.Continuous = wait.Samples >= pendingInputMinSamples && span >= pendingInputMinSpan

	prev.Last = now.Unix()
	prev.Samples = wait.Samples
	c.write(path, prev)
	return wait
}

// Reset forgets session's pending run, for a caller that has just acted on the
// input — the wait it measured was unattended, and it no longer is — or that
// has observed the composer is no longer holding input. Any run a later
// observation starts begins its own clock.
func (c *PendingInputClock) Reset(session string) {
	if c == nil || strings.TrimSpace(session) == "" {
		return
	}
	if path := c.path(session); path != "" {
		_ = os.Remove(path)
	}
}

// path returns the stamp file for a session, or "" when the session name
// cannot be made into a filename.
func (c *PendingInputClock) path(session string) string {
	name := strings.NewReplacer("/", "_", string(filepath.Separator), "_").Replace(session)
	if name == "" || name == "." || name == ".." || strings.Trim(name, ".") == "" {
		return ""
	}
	return filepath.Join(c.dir, name)
}

// read returns the stamp for session's pending run, if there is a readable one.
// A stamp that cannot be understood reads as "no run in progress": the caller
// then starts a fresh run, which is the conservative direction — a new run has
// no age to act on.
func (c *PendingInputClock) read(path string) (pendingStamp, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return pendingStamp{}, false
	}
	var stamp pendingStamp
	if err := json.Unmarshal(data, &stamp); err != nil {
		return pendingStamp{}, false
	}
	if stamp.First <= 0 || stamp.Samples <= 0 {
		return pendingStamp{}, false
	}
	return stamp, true
}

// write stores session's pending run, best effort — a clock that cannot write
// reports no run, leaving the caller on the silence window.
func (c *PendingInputClock) write(path string, stamp pendingStamp) {
	data, err := json.Marshal(stamp)
	if err != nil {
		return
	}
	if err := os.MkdirAll(c.dir, 0o755); err != nil {
		return
	}
	// Rename so a concurrent reader never sees a partially-written stamp.
	tmp := fmt.Sprintf("%s.%d.tmp", path, os.Getpid())
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
}
