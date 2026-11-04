package session

import (
	"errors"
	"fmt"

	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// ErrNothingToPush is what a SandboxPusher returns when the sandbox is already
// gone: no commits remain to push, which is not the same as a push that ran.
var ErrNothingToPush = errors.New("sandbox already gone: nothing to push")

// The openshell implementation sets these two hooks when it is linked in.
// They are package variables rather than an injected object because the
// implementation is an optional link-time dependency of this package's
// callers (the daemon, gt sling, gt polecat), the same way NewOpenShellBackend
// is: every entry point that needs them takes an explicit function too
// (CleanupSandboxWith, ResolveWith), and tests inject through those.
var (
	// SandboxDeleter deletes the sandbox a record describes. Nil means the
	// record is removed but the sandbox is left for the operator.
	SandboxDeleter func(townRoot string, rec SandboxRecord) error
	// SandboxPusher pushes the commits a sandbox's clone holds to origin, so a
	// stop can delete the sandbox without losing them (gt-hwpfq). Nil means
	// nothing can be pushed, and the caller keeps the sandbox.
	SandboxPusher func(townRoot string, rec SandboxRecord) error
	// SandboxLiveness assesses a sandboxed session where it lives (the
	// transcript is inside the sandbox). Nil leaves a sandboxed session's
	// verdict Unknown: the host probe's transcript read would not see it.
	SandboxLiveness func(townRoot string, rec SandboxRecord, in liveness.Input) liveness.Result
)

// CleanupSandboxWith removes what a sandboxed session left: the sandbox
// itself through deleter, then its registry record and routed client. A
// session without a record is a host session and nothing happens. Callers
// run it after the session's processes were killed (the kill is routed), and
// pass SandboxDeleter unless they have a deleter of their own; nil leaves the
// sandbox and drops the record.
func CleanupSandboxWith(deleter func(townRoot string, rec SandboxRecord) error, townRoot, session string) error {
	rec, ok, err := ReadSandboxRecord(townRoot, session)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	return CleanupSandboxWithRecord(deleter, townRoot, session, rec)
}

// CleanupSandboxWithRecord is CleanupSandboxWith for a caller that already read
// the record: the same deletion over rec, without reading it again, so the
// sandbox removed is the one the caller saw.
func CleanupSandboxWithRecord(deleter func(townRoot string, rec SandboxRecord) error, townRoot, session string, rec SandboxRecord) error {
	if deleter != nil {
		if err := deleter(townRoot, rec); err != nil {
			return fmt.Errorf("deleting sandbox %s: %w", session, err)
		}
	}
	routing.forget(session)
	return RemoveSandboxRecord(townRoot, session)
}

// CleanupErroredSandbox is CleanupSandboxWith for a seat whose session is already
// gone, when its sandbox may have failed under it (gt-jhoi1). The record's sandbox
// is deleted, and its record dropped, only when the gateway reports that sandbox
// in a terminal failure phase, so the seat name is free for the next dispatch.
// Any other phase, a sandbox the gateway no longer has, and a session with no
// record leave the record alone. A gateway that cannot answer is an error, never
// a phase. The bool reports whether the record was removed.
func CleanupErroredSandbox(status func(gateway, name string) (sandbox.Status, error), deleter func(townRoot string, rec SandboxRecord) error, townRoot, session string) (bool, error) {
	rec, ok, err := ReadSandboxRecord(townRoot, session)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	st, err := status(rec.Gateway, session)
	if isSandboxNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("asking gateway %q for sandbox %s: %w", rec.Gateway, session, err)
	}
	if !sandboxPhaseTerminal(st.Phase) {
		return false, nil
	}
	// A nil deleter would drop the record and leave the failed sandbox behind, so
	// it is a refusal rather than a cleanup.
	if deleter == nil {
		return false, fmt.Errorf("sandbox %s is in phase %s, but no sandbox deleter is linked", session, st.Phase)
	}
	if err := CleanupSandboxWithRecord(deleter, townRoot, session, rec); err != nil {
		return false, err
	}
	return true, nil
}

// IsSandboxed reports whether session's record exists; a registry that
// cannot be read is an error, not "no".
func IsSandboxed(townRoot, session string) (bool, error) {
	_, ok, err := ReadSandboxRecord(townRoot, session)
	return ok, err
}

// AssessWhereItLives runs the liveness assessment for in.Session: through
// SandboxLiveness when the session is sandboxed and the hook is set, else
// through host (whose tmux probes route to the sandbox on their own).
func AssessWhereItLives(townRoot string, host liveness.Probe, in liveness.Input) liveness.Result {
	return assessWhereItLivesWith(SandboxLiveness, townRoot, host, in)
}

func assessWhereItLivesWith(sandboxed func(string, SandboxRecord, liveness.Input) liveness.Result, townRoot string, host liveness.Probe, in liveness.Input) liveness.Result {
	rec, ok, err := ReadSandboxRecord(townRoot, in.Session)
	if err != nil {
		// Not answered: a registry that cannot be read must not turn into a
		// host verdict on a session that may live in a sandbox.
		return liveness.Result{Verdict: liveness.Unknown, Reason: "sandbox registry unreadable", Err: err}
	}
	if ok {
		if sandboxed == nil {
			return liveness.Result{Verdict: liveness.Unknown, Reason: "sandboxed session, no sandbox liveness linked"}
		}
		return sandboxed(townRoot, rec, in)
	}
	return liveness.Assess(host, in)
}
