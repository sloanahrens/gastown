package session

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/tmux"
)

func init() {
	SandboxDeleter = func(townRoot string, rec SandboxRecord) error {
		return recordBackend(townRoot, rec).deleteSandbox(rec.Session)
	}
	SandboxLiveness = func(townRoot string, rec SandboxRecord, in liveness.Input) liveness.Result {
		return recordBackend(townRoot, rec).ProgressSample(rec.Session, in)
	}
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
	if err != nil {
		if isSandboxNotFound(err) {
			return false, nil
		}
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
	if in.Transcript == nil {
		in.Transcript = func(string) (string, time.Time, int64, error) {
			return b.transcript(session)
		}
	}
	return liveness.Assess(b.probeFor(session), in)
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

// isSandboxNotFound matches openshell's missing-sandbox failure, which the
// CLI wrapper carries as the command's stderr.
func isSandboxNotFound(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "notfound")
}
