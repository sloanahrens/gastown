// Package liveness is the town's one answer to "is this seat's agent alive
// and making progress" (ADR 0003, gt-4k3fj.2). Every kill or restart
// decision reads a verdict from Assess:
//
//	Unknown  a query failed; the caller never acts on it
//	Dead     no session, or a session whose agent process is gone
//	Stalled  alive, but no progress evidence has changed for StallAfter
//	Alive    everything else
//
// Progress is measured by CHANGE between two samples, never by elapsed turn
// time: the pane's work region (tmux.PaneProgressSignature, which ignores the
// composer and spinner chrome), the Claude transcript's mtime and size, and an
// optional heartbeat cycle number. The previous sample lives in the seat's
// intent record (internal/intent), so the evidence survives any number of
// daemon restarts; there is no tracker in memory.
package liveness

import (
	"fmt"
	"os"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/intent"
	"github.com/steveyegge/gastown/internal/tmux"
)

// DefaultStallAfter is how long a seat may show no progress evidence before
// it is Stalled (epic gt-4k3fj item 7; tunable after a measured week).
const DefaultStallAfter = 30 * time.Minute

// paneLines is how much of the pane a sample captures.
const paneLines = 200

// Verdict is the liveness answer.
type Verdict int

const (
	// Unknown is the zero value: an unanswered question is never a death.
	Unknown Verdict = iota
	Alive
	Dead
	Stalled
)

func (v Verdict) String() string {
	switch v {
	case Alive:
		return "alive"
	case Dead:
		return "dead"
	case Stalled:
		return "stalled"
	default:
		return "unknown"
	}
}

// The two reasons a verdict is Dead.
const (
	ReasonNoSession = "no session"
	ReasonAgentGone = "agent process not running"
)

// Probe is the tmux surface Assess reads. *tmux.Tmux implements it.
type Probe interface {
	HasSession(name string) (bool, error)
	// IsAgentAliveChecked inspects the pane's process tree; an error is
	// unknown, never dead.
	IsAgentAliveChecked(session string) (bool, error)
	CapturePane(session string, lines int) (string, error)
	GetSessionCreatedTime(name string) (time.Time, error)
	// PaneDead reports whether the pane exited but was kept; an error is
	// unknown.
	PaneDead(session string) (bool, error)
}

var _ Probe = (*tmux.Tmux)(nil)

// Heartbeat is optional progress evidence from a role that writes a cycle
// counter (the deacon). Only a change of Cycle counts; the file's timestamp
// is refreshed on a timer and dates nothing.
type Heartbeat struct {
	Cycle int64
}

// TranscriptFunc locates the newest transcript for workDir and reports its
// mtime and size. An empty path means none.
type TranscriptFunc func(workDir string) (path string, mtime time.Time, size int64, err error)

// Input is one assessment's parameters.
type Input struct {
	Session string
	// WorkDir locates the Claude transcript; empty skips that evidence.
	WorkDir string
	// Prev is the sample from the seat's intent record, nil for none.
	Prev *intent.Progress
	// Now is the sample time; zero means time.Now().
	Now time.Time
	// StallAfter; zero means DefaultStallAfter.
	StallAfter time.Duration
	Heartbeat  *Heartbeat
	// Transcript; nil means the Claude Code project directory lookup.
	Transcript TranscriptFunc
}

// Result is the verdict plus the sample to persist.
type Result struct {
	Verdict Verdict
	// Reason explains the verdict in one phrase, for logs.
	Reason string
	// QuietFor is how long no evidence has changed (Alive or Stalled).
	QuietFor time.Duration
	// Err is the failed query behind an Unknown verdict.
	Err error
	// Sample is what the caller writes back to the intent record. For an
	// Unknown verdict it is Prev, unchanged: an unanswered question must not
	// move the evidence either way.
	Sample *intent.Progress
}

// Assess returns the seat's verdict. It never kills or writes anything.
func Assess(p Probe, in Input) Result {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	stallAfter := in.StallAfter
	if stallAfter <= 0 {
		stallAfter = DefaultStallAfter
	}

	exists, err := p.HasSession(in.Session)
	if err != nil {
		return Result{Verdict: Unknown, Reason: "session query failed", Err: err, Sample: in.Prev}
	}
	if !exists {
		return deadResult(in.Prev, now, ReasonNoSession)
	}
	alive, err := p.IsAgentAliveChecked(in.Session)
	if err != nil {
		// A pane kept after its process exited has no current command, so
		// the process query cannot answer; tmux's pane_dead can. A dead pane
		// is a confirmed dead agent, and nothing else revives it now that the
		// auto-respawn hook is gone.
		if dead, derr := p.PaneDead(in.Session); derr == nil && dead {
			return deadResult(in.Prev, now, ReasonAgentGone)
		}
		return Result{Verdict: Unknown, Reason: "agent process query failed", Err: err, Sample: in.Prev}
	}
	if !alive {
		return deadResult(in.Prev, now, ReasonAgentGone)
	}

	cur := sample(p, in, now)
	prev := in.Prev
	if prev == nil || prev.SampledAt.IsZero() || otherIncarnation(prev, cur) {
		cur.ChangedAt = now
		return Result{Verdict: Alive, Reason: "baseline sample", Sample: cur}
	}
	carryForward(prev, cur)
	if changed(prev, cur) {
		cur.ChangedAt = now
		return Result{Verdict: Alive, Reason: "progress", Sample: cur}
	}
	cur.ChangedAt = prev.ChangedAt
	if cur.ChangedAt.IsZero() {
		cur.ChangedAt = prev.SampledAt
	}
	quiet := now.Sub(cur.ChangedAt)
	if !hasEvidence(cur) {
		return Result{Verdict: Alive, Reason: "no progress evidence to compare", QuietFor: quiet, Sample: cur}
	}
	if quiet >= stallAfter {
		return Result{Verdict: Stalled, Reason: fmt.Sprintf("no progress for %s", quiet.Round(time.Second)), QuietFor: quiet, Sample: cur}
	}
	return Result{Verdict: Alive, Reason: "quiet", QuietFor: quiet, Sample: cur}
}

// deadResult returns a Dead result whose sample counts consecutive dead
// samples.
func deadResult(prev *intent.Progress, now time.Time, reason string) Result {
	s := &intent.Progress{SampledAt: now, DeadSamples: 1}
	if prev != nil {
		s.DeadSamples = prev.DeadSamples + 1
	}
	return Result{Verdict: Dead, Reason: reason, Sample: s}
}

// sample collects the progress evidence for a live session. Individual
// evidence failures leave that field empty; they are not a verdict.
func sample(p Probe, in Input, now time.Time) *intent.Progress {
	s := &intent.Progress{SampledAt: now}
	if created, err := p.GetSessionCreatedTime(in.Session); err == nil {
		s.SessionCreated = created
	}
	if pane, err := p.CapturePane(in.Session, paneLines); err == nil {
		s.PaneHash = tmux.PaneProgressSignature(pane, tmux.DefaultReadyPromptPrefix)
	}
	if in.WorkDir != "" || in.Transcript != nil {
		find := in.Transcript
		if find == nil {
			find = claudeTranscript
		}
		if path, mtime, size, err := find(in.WorkDir); err == nil && path != "" {
			s.TranscriptPath, s.TranscriptMtime, s.TranscriptBytes = path, mtime, size
		}
	}
	if in.Heartbeat != nil {
		s.HasHeartbeat = true
		s.HeartbeatCycle = in.Heartbeat.Cycle
	}
	return s
}

// otherIncarnation reports whether the two samples come from different
// sessions. An unknown creation time cannot prove a difference.
func otherIncarnation(prev, cur *intent.Progress) bool {
	return !prev.SessionCreated.IsZero() && !cur.SessionCreated.IsZero() && !prev.SessionCreated.Equal(cur.SessionCreated)
}

// carryForward fills evidence this sample could not collect from the
// previous one, so a failed query is neither progress nor a lost baseline.
func carryForward(prev, cur *intent.Progress) {
	if cur.PaneHash == "" {
		cur.PaneHash = prev.PaneHash
	}
	if cur.TranscriptPath == "" {
		cur.TranscriptPath, cur.TranscriptMtime, cur.TranscriptBytes = prev.TranscriptPath, prev.TranscriptMtime, prev.TranscriptBytes
	}
	if !cur.HasHeartbeat {
		cur.HasHeartbeat, cur.HeartbeatCycle = prev.HasHeartbeat, prev.HeartbeatCycle
	}
	if cur.SessionCreated.IsZero() {
		cur.SessionCreated = prev.SessionCreated
	}
}

// changed reports whether any evidence differs between the samples.
func changed(prev, cur *intent.Progress) bool {
	if cur.PaneHash != prev.PaneHash {
		return true
	}
	if cur.TranscriptPath != prev.TranscriptPath || !cur.TranscriptMtime.Equal(prev.TranscriptMtime) || cur.TranscriptBytes != prev.TranscriptBytes {
		return true
	}
	return cur.HasHeartbeat && prev.HasHeartbeat && cur.HeartbeatCycle != prev.HeartbeatCycle
}

func hasEvidence(s *intent.Progress) bool {
	return s.PaneHash != "" || s.TranscriptPath != "" || s.HasHeartbeat
}

// claudeTranscript is the default TranscriptFunc.
func claudeTranscript(workDir string) (string, time.Time, int64, error) {
	path, err := agentlog.LatestTranscript(workDir)
	if err != nil || path == "" {
		return "", time.Time{}, 0, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", time.Time{}, 0, err
	}
	return path, info.ModTime(), info.Size(), nil
}
