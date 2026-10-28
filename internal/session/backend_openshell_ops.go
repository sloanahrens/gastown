package session

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/tmux"
)

func init() {
	SandboxDeleter = deleteSandboxRecord
	SandboxPusher = pushSandboxRecord
	SandboxLiveness = sampleSandboxRecord
}

// deleteSandboxRecord is SandboxDeleter: the sandbox a registry record names,
// deleted from the record alone so cleanup and the daemon need no Prepare.
func deleteSandboxRecord(townRoot string, rec SandboxRecord) error {
	return recordBackend(townRoot, rec).deleteSandbox(rec.Session)
}

// pushSandboxRecord is SandboxPusher: the record's clone branch pushed to origin
// from inside its sandbox.
func pushSandboxRecord(townRoot string, rec SandboxRecord) error {
	return recordBackend(townRoot, rec).pushWork(rec)
}

// sandboxPushTimeout bounds the push a stop makes before it deletes a sandbox.
const sandboxPushTimeout = 2 * time.Minute

// pushWork pushes rec's clone branch from inside its sandbox, where the clone
// and its Forgejo credential helper live. A sandbox that is already gone has
// nothing left to push. A record without a branch or work dir predates the
// push, so its clone cannot be found and the caller must keep the sandbox.
func (b *openshellBackend) pushWork(rec SandboxRecord) error {
	if rec.Branch == "" || rec.WorkDir == "" {
		return fmt.Errorf("sandbox %s: its record names no clone, so its commits cannot be pushed", rec.Session)
	}
	ctx, cancel := context.WithTimeout(context.Background(), sandboxPushTimeout)
	defer cancel()
	_, _, err := b.cli.Exec(ctx, rec.Session, nil, []string{"git", "-C", rec.WorkDir, "push", "origin", rec.Branch}, nil)
	if err != nil && !isSandboxNotFound(err) {
		return fmt.Errorf("pushing %s from sandbox %s: %w", rec.Branch, rec.Session, err)
	}
	return nil
}

// sampleSandboxRecord is SandboxLiveness: the record's seat assessed where its
// sandbox runs.
func sampleSandboxRecord(townRoot string, rec SandboxRecord, in liveness.Input) liveness.Result {
	return recordBackend(townRoot, rec).ProgressSample(rec.Session, in)
}

// transcriptTimeout bounds the exec that reads the sandbox's transcript.
const transcriptTimeout = 30 * time.Second

// transcriptScript prints "<mtime epoch> <size> <path>" for the newest Claude
// transcript under the sandbox's projects directory, and nothing when there
// is none (so "no transcript yet" is not an exec failure).
const transcriptScript = `f=$(ls -t ` + sandboxHome + `/.claude/projects/*/*.jsonl 2>/dev/null | head -1); if [ -n "$f" ]; then stat -c '%Y %s %n' "$f"; fi`

// recordBackend is the backend for the sandbox rec describes, built from the
// registry alone: the daemon and cleanup have no Prepare to carry state over.
func recordBackend(townRoot string, rec SandboxRecord) *openshellBackend {
	return &openshellBackend{
		cli:      &sandbox.CLI{Gateway: rec.Gateway},
		townRoot: townRoot,
		profile:  Profile{Name: rec.Profile, Gateway: rec.Gateway, Image: rec.Image},
	}
}

// probeFor is the tmux client that runs inside session's sandbox.
func (b *openshellBackend) probeFor(session string) liveness.Probe {
	if b.probe != nil {
		return b.probe(session)
	}
	return tmux.NewTmuxWithExecPrefix(SandboxTmuxSocket, SandboxExecPrefix(b.profile.Gateway, session)).Routed()
}

// SessionExists reports whether the sandbox is Ready and its tmux server has
// the session. A sandbox that is gone or not Ready has no session, which is
// an answer; any other failure to ask is an error.
func (b *openshellBackend) SessionExists(session string) (bool, error) {
	st, err := b.cli.Get(context.Background(), session)
	if isSandboxNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !st.Ready {
		return false, nil
	}
	return b.probeFor(session).HasSession(session)
}

// ProgressSample runs liveness.Assess against the sandbox's tmux server. The
// transcript is read inside the sandbox unless the caller supplied a lookup:
// the host's .claude-town never sees it.
func (b *openshellBackend) ProgressSample(session string, in liveness.Input) liveness.Result {
	if in.Session == "" {
		in.Session = session
	}
	readErr := error(nil)
	if in.Transcript == nil {
		in.Transcript = func(string) (string, time.Time, int64, error) {
			path, mtime, size, err := b.transcript(session)
			readErr = err
			return path, mtime, size, err
		}
	}
	res := liveness.Assess(b.probeFor(session), in)
	// Assess reads any Transcript error as evidence it could not collect, which
	// answers alive. For this backend that error is its own exec: an unreachable
	// sandbox would be reported alive with no evidence rather than as a
	// question it could not answer (om 63998d067063).
	if readErr != nil && res.Verdict == liveness.Alive {
		res = liveness.Result{Verdict: liveness.Unknown, Reason: "transcript query failed", Err: readErr, Sample: in.Prev}
	}
	return b.phaseVerdict(session, in, res)
}

// phaseVerdict is the gateway's half of a sandbox seat's liveness answer: a
// sandbox whose phase has been Error or Failed for SandboxDeadGrace is dead,
// whatever its tmux said, which is what a sandbox whose supervisor died looks
// like (gt-tj446). The grace runs from when the gateway says the sandbox
// stopped being Ready, so a patrol that first looks minutes into the failure
// does not add that lag to the seat's hold (gt-j18af). Any other phase, and a
// Ready one in particular, starts the clock over and leaves the tmux answer
// standing. A gateway that cannot answer leaves it standing too: an outage is
// not a dead sandbox.
//
// The gateway is asked only when the tmux sample is not Alive. A tmux that
// answers is proof the sandbox runs, so a healthy seat costs no gateway call
// per sample (om f9ae48011e3e), and any failure clock it carries is stale: a
// sandbox that answers starts the clock over.
func (b *openshellBackend) phaseVerdict(session string, in liveness.Input, res liveness.Result) liveness.Result {
	if res.Verdict == liveness.Alive {
		b.phases().forget(session)
		return res
	}
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	st, err := b.sandboxPhase(session)
	if err != nil {
		if isSandboxNotFound(err) {
			// The name is free again: a sandbox created under it later must
			// not inherit this one's clock.
			b.phases().forget(session)
		}
		if res.Verdict == liveness.Unknown {
			res.Reason = fmt.Sprintf("%s; the gateway reported no sandbox phase: %v", res.Reason, err)
		}
		return res
	}
	if !sandboxPhaseTerminal(st.Phase) {
		b.phases().forget(session)
		return res
	}
	failing := b.phases().failing(session, st.NotReadySince, now)
	if failing < SandboxDeadGrace {
		if res.Verdict == liveness.Unknown {
			res.Reason = fmt.Sprintf("%s; sandbox phase %s for %s of %s", res.Reason, st.Phase, failing.Round(time.Second), SandboxDeadGrace)
		}
		return res
	}
	return liveness.DeadResult(in.Prev, now, fmt.Sprintf("sandbox phase %s for %s", st.Phase, failing.Round(time.Second)))
}

// sandboxPhase asks the gateway for the sandbox, bounded well under the two
// minutes an openshell call may otherwise take, so a hung gateway cannot hold
// a patrol tick.
func (b *openshellBackend) sandboxPhase(session string) (sandbox.Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), sandboxPhaseTimeout)
	defer cancel()
	return b.cli.Get(ctx, session)
}

// phases is the clock this backend's sandbox phase readings are measured from.
func (b *openshellBackend) phases() *sandboxPhaseWatch {
	if b.phaseWatch != nil {
		return b.phaseWatch
	}
	return sandboxPhases
}

// transcript is the newest transcript's path, mtime and size inside the
// sandbox. An empty path means there is none.
func (b *openshellBackend) transcript(session string) (string, time.Time, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), transcriptTimeout)
	defer cancel()
	out, _, err := b.cli.Exec(ctx, session, nil, []string{"sh", "-c", transcriptScript}, nil)
	if err != nil {
		return "", time.Time{}, 0, err
	}
	return parseTranscriptStat(string(out))
}

// parseTranscriptStat reads transcriptScript's "<mtime epoch> <size> <path>"
// line; blank output is no transcript.
func parseTranscriptStat(out string) (string, time.Time, int64, error) {
	line := strings.TrimSpace(out)
	if line == "" {
		return "", time.Time{}, 0, nil
	}
	fields := strings.SplitN(line, " ", 3)
	if len(fields) != 3 {
		return "", time.Time{}, 0, fmt.Errorf("transcript stat %q: want \"<mtime> <size> <path>\"", line)
	}
	epoch, err := strconv.ParseInt(fields[0], 10, 64)
	if err != nil {
		return "", time.Time{}, 0, fmt.Errorf("transcript stat mtime %q: %w", fields[0], err)
	}
	size, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return "", time.Time{}, 0, fmt.Errorf("transcript stat size %q: %w", fields[1], err)
	}
	return fields[2], time.Unix(epoch, 0), size, nil
}

// Stop stops the sandbox, keeping its workspace and record.
func (b *openshellBackend) Stop(session string) error {
	return b.cli.Stop(context.Background(), session)
}

// Delete removes the sandbox and its registry record. A sandbox that is
// already gone counts as deleted; any other failure keeps the record so the
// delete can be retried.
func (b *openshellBackend) Delete(session string) error {
	if err := b.deleteSandbox(session); err != nil {
		return err
	}
	routing.forget(session)
	return RemoveSandboxRecord(b.townRoot, session)
}

// deleteSandbox is `sandbox delete` with "not found" as success.
func (b *openshellBackend) deleteSandbox(session string) error {
	err := b.cli.Delete(context.Background(), session)
	if err != nil && !isSandboxNotFound(err) {
		return err
	}
	return nil
}

// sandboxNotFound matches the two missing-sandbox messages openshell prints,
// which the CLI wrapper carries in the wrapped error: `sandbox not found:
// <name>` from get and `sandbox "<name>" not found` from delete.
var sandboxNotFound = regexp.MustCompile(`\bsandbox (?:not found\b|"[^"\n]*" not found\b)`)

// isSandboxNotFound reports whether err is openshell's missing-sandbox failure.
// A gateway, host or profile that is not found stays an error: those leave the
// sandbox in place, so reading them as gone would drop its record and report a
// delete that did not happen (om 1dc191fba3ee).
func isSandboxNotFound(err error) bool {
	return err != nil && sandboxNotFound.MatchString(strings.ToLower(err.Error()))
}
