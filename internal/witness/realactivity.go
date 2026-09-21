package witness

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/agentlog"
	"github.com/steveyegge/gastown/internal/session"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/workspace"
)

// RealActivity is a snapshot of the most recent evidence that an agent session
// is doing real work, as opposed to merely being alive.
//
// gt-xb27: witness stall detection used to be inferred — by the patrol agent —
// from a pane's rendered elapsed label (Claude Code's per-turn spinner, e.g.
// "Imagining... (8h 54m)"). That label measures the CURRENT TURN, not time
// since output, so a polecat deep inside one long turn shows an enormous value
// while working perfectly. Reading it as "no output 30+ min" killed a healthy
// polecat (opal, gt-hmaf) with 408 tool calls and a file write 17 minutes
// before the restart. This type exists to give the patrol a sound signal.
//
// Two signals are sound, and both are captured here:
//
//	(a) transcript recency — Claude Code appends to its JSONL transcript on
//	    conversation events (tool calls, file writes, assistant messages) and
//	    NOT on terminal redraws, so its mtime is "last real work";
//	(b) pane content change — the pane's non-volatile lines (spinner chrome
//	    stripped) are hashed, so an unchanged signature means the screen has
//	    not changed in any way that reflects work.
//
// Deliberately NOT used, because they do not discriminate: the pane's rendered
// elapsed label, session uptime, token count, and the string "stalled" from
// gt polecat list on a freshly dispatched polecat.
type RealActivity struct {
	// Session is the tmux session name that was observed.
	Session string
	// Polecat is the polecat name, when known.
	Polecat string
	// WorkDir is the directory the activity was resolved against.
	WorkDir string
	// AgentAlive reports whether the agent process is running in the session.
	// A stall verdict is meaningless without it: a dead agent is a different
	// failure class with its own restart path (ZombieAgentDeadInSession).
	AgentAlive bool
	// ObservedAt is when this snapshot was taken. Compare two snapshots'
	// ObservedAt to prove a comparison window actually elapsed.
	ObservedAt time.Time

	// LastActivity is the timestamp of the last real work event. Zero means
	// unknown — there is no transcript to date the session by.
	LastActivity time.Time
	// ActivitySource names where LastActivity came from: "transcript" when a
	// Claude Code JSONL backed it, "none" when nothing could be dated.
	ActivitySource string
	// TranscriptPath is the Claude Code transcript for this session, if found.
	TranscriptPath string
	// TranscriptBytes is the transcript's size at the time of observation.
	// Size, unlike mtime, cannot be preserved by a same-second write.
	TranscriptBytes int64
	// PaneSignature digests the pane's non-volatile content.
	PaneSignature string

	// Errors collects non-fatal observation failures. A snapshot with errors
	// must not be used as the sole basis for a restart.
	Errors []string
}

// ActivitySourceTranscript marks LastActivity as transcript-derived.
const ActivitySourceTranscript = "transcript"

// ActivitySourceNone marks LastActivity as unknown.
const ActivitySourceNone = "none"

// ObserveRealActivity snapshots the sound stall signals for one session.
//
// workDir may be empty, in which case the session's own pane working directory
// is used. This is the preferred path: it follows the session wherever it was
// spawned instead of assuming a directory layout.
func ObserveRealActivity(t *tmux.Tmux, polecatName, sessionName, workDir string) RealActivity {
	act := RealActivity{
		Session:        sessionName,
		Polecat:        polecatName,
		ObservedAt:     time.Now(),
		ActivitySource: ActivitySourceNone,
	}

	if t == nil {
		act.Errors = append(act.Errors, "no tmux client")
		return act
	}

	act.AgentAlive = t.IsAgentAlive(sessionName)

	if workDir == "" {
		if path, err := t.PaneCurrentPath(sessionName); err == nil {
			workDir = path
		} else {
			act.Errors = append(act.Errors, fmt.Sprintf("resolving pane cwd: %v", err))
		}
	}
	act.WorkDir = workDir

	// Signal (a): transcript recency.
	if workDir != "" {
		transcript, err := agentlog.LatestTranscript(workDir)
		if err != nil {
			act.Errors = append(act.Errors, fmt.Sprintf("locating transcript: %v", err))
		} else if transcript != "" {
			act.TranscriptPath = transcript
			if info, err := os.Stat(transcript); err != nil {
				act.Errors = append(act.Errors, fmt.Sprintf("stat transcript: %v", err))
			} else if sessionStart, err := t.GetSessionCreatedTime(sessionName); err == nil && !transcriptDatesSession(info.ModTime(), sessionStart) {
				// The newest transcript predates this session, so it belongs to a
				// previous run in the same (reused) worktree and dates nothing.
				// Reporting its age would nominate a polecat that was dispatched
				// moments ago as a stale candidate — the spawn transient the
				// witness must not act on.
				act.Errors = append(act.Errors, fmt.Sprintf(
					"newest transcript predates session (mtime %s, session started %s)",
					info.ModTime().Format(time.RFC3339), sessionStart.Format(time.RFC3339)))
			} else {
				act.LastActivity = info.ModTime()
				act.TranscriptBytes = info.Size()
				act.ActivitySource = ActivitySourceTranscript
			}
		}
	}

	// Signal (b): pane content change, spinner chrome stripped.
	if sig, err := t.PaneContentSignature(sessionName, paneSignatureLines); err == nil {
		act.PaneSignature = sig
	} else {
		act.Errors = append(act.Errors, fmt.Sprintf("hashing pane content: %v", err))
	}

	return act
}

// paneSignatureLines is how much pane scrollback the signature covers. The
// tail is where work in progress appears; a deeper window would hash history
// that cannot change and only dilute the signal.
const paneSignatureLines = 60

// transcriptDatesSession reports whether a transcript modification time can be
// attributed to the session that is being judged. A transcript written before
// the session started belongs to an earlier run in the same worktree.
func transcriptDatesSession(transcriptModTime, sessionStart time.Time) bool {
	return !transcriptModTime.Before(sessionStart)
}

// ObserveRigRealActivity snapshots every live polecat session in a rig.
// Polecats without a live tmux session are skipped — zombie detection owns
// those. Sessions are visited in name order so output is stable between runs.
func ObserveRigRealActivity(workDir, rigName string) []RealActivity {
	townRoot, err := workspace.Find(workDir)
	if err != nil || townRoot == "" {
		townRoot = workDir
	}

	entries, err := os.ReadDir(filepath.Join(townRoot, rigName, "polecats"))
	if err != nil {
		return nil
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && !strings.HasPrefix(entry.Name(), ".") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	t := tmux.NewTmux()
	prefix := session.PrefixFor(rigName)

	observations := make([]RealActivity, 0, len(names))
	for _, name := range names {
		sessionName := session.PolecatSessionName(prefix, name)
		alive, err := t.HasSession(sessionName)
		if err != nil || !alive {
			continue
		}
		observations = append(observations, ObserveRealActivity(t, name, sessionName, ""))
	}
	return observations
}

// StallVerdict is the outcome of a stall assessment.
type StallVerdict struct {
	// Stalled is true only when there is POSITIVE evidence that a live agent
	// has stopped working. Anything weaker is reported as not-stalled.
	Stalled bool
	// Reason explains the verdict in one line, suitable for a patrol log.
	Reason string
}

// AssessStall decides whether cur — compared against an earlier observation
// prev taken at least window ago — is positive evidence that a live agent has
// stopped working.
//
// A true verdict requires ALL of:
//  1. the agent process is alive (a dead agent is a different class, handled by
//     the zombie restart path);
//  2. the transcript has not advanced — no new real work event;
//  3. the pane's non-volatile content is byte-identical — nothing on screen
//     changed except spinner chrome.
//
// Anything less is INSUFFICIENT EVIDENCE, and the caller must nudge rather than
// restart. This is the whole point of the function (gt-xb27): "when in doubt,
// nudge, do not restart". In particular, note that a long-running turn looks
// identical to a stall on every signal except these, so a verdict of Stalled
// must be earned by agreement across both signals over a full window.
func AssessStall(prev, cur RealActivity, window time.Duration) StallVerdict {
	if window <= 0 {
		return StallVerdict{Reason: "no comparison window configured"}
	}
	if !cur.AgentAlive {
		return StallVerdict{Reason: "agent process is not alive — not a stall; use the zombie restart path"}
	}

	elapsed := cur.ObservedAt.Sub(prev.ObservedAt)
	if elapsed < window {
		return StallVerdict{Reason: fmt.Sprintf("observations are only %s apart, need %s",
			elapsed.Round(time.Second), window)}
	}

	if prev.ActivitySource != ActivitySourceTranscript || prev.LastActivity.IsZero() {
		return StallVerdict{Reason: "no transcript baseline — cannot show work stopped"}
	}
	if cur.ActivitySource != ActivitySourceTranscript {
		// The transcript vanished or no longer dates this session. That is an
		// observation failure, not evidence that work stopped.
		return StallVerdict{Reason: "transcript no longer dates this session — insufficient evidence"}
	}

	if cur.LastActivity.After(prev.LastActivity) || cur.TranscriptBytes != prev.TranscriptBytes {
		return StallVerdict{Reason: fmt.Sprintf("transcript advanced (grew by %d bytes) — agent is working",
			cur.TranscriptBytes-prev.TranscriptBytes)}
	}

	if prev.PaneSignature == "" || cur.PaneSignature == "" {
		return StallVerdict{Reason: "pane signature unavailable — insufficient evidence"}
	}
	if cur.PaneSignature != prev.PaneSignature {
		return StallVerdict{Reason: "pane content changed — agent is working"}
	}

	return StallVerdict{
		Stalled: true,
		Reason: fmt.Sprintf("no transcript change and no pane content change for %s (last work %s ago)",
			elapsed.Round(time.Second), time.Since(cur.LastActivity).Round(time.Second)),
	}
}

// Age returns how long ago real work last happened, and whether that is known.
func (a RealActivity) Age(now time.Time) (time.Duration, bool) {
	if a.LastActivity.IsZero() {
		return 0, false
	}
	return now.Sub(a.LastActivity), true
}

// ConfirmsStoppedWork reports whether the snapshot is positive evidence that
// the session did no real work after since.
//
// Only the transcript answers this: Claude Code appends to it on conversation
// events, not on terminal redraws, so a transcript no newer than since proves
// no turn has progressed. A live process and a redrawing pane are consistent
// with both work and a wedge, so a snapshot that cannot date the session
// confirms nothing and returns false — the caller must then leave the session
// alone (gt-z7vr).
func (a RealActivity) ConfirmsStoppedWork(since time.Time) bool {
	if !a.AgentAlive || a.ActivitySource != ActivitySourceTranscript || a.LastActivity.IsZero() {
		return false
	}
	return !a.LastActivity.After(since)
}

// IsStaleCandidate reports whether the snapshot's last real activity is older
// than threshold. This is NOT a stall verdict — it only marks a polecat worth
// re-sampling. Call AssessStall with two samples before acting.
func (a RealActivity) IsStaleCandidate(now time.Time, threshold time.Duration) bool {
	age, ok := a.Age(now)
	if !ok {
		return false
	}
	return age > threshold
}

// Describe renders the snapshot for a patrol log line.
func (a RealActivity) Describe(now time.Time) string {
	if !a.AgentAlive {
		return "agent process not alive"
	}
	parts := make([]string, 0, 3)
	if age, ok := a.Age(now); ok {
		parts = append(parts, fmt.Sprintf("last real activity %s ago (%s)", age.Round(time.Second), a.ActivitySource))
	} else {
		parts = append(parts, "no transcript — last activity unknown")
	}
	if a.PaneSignature != "" {
		parts = append(parts, fmt.Sprintf("pane %s", a.PaneSignature))
	}
	if len(a.Errors) > 0 {
		parts = append(parts, fmt.Sprintf("%d observation error(s)", len(a.Errors)))
	}
	return strings.Join(parts, ", ")
}
