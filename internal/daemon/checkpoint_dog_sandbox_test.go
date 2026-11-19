package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// revertGuardSandboxScript is the in-sandbox git of a sandbox seat's clone,
// as the checkpoint revert guard sees it: it records every call, refuses one
// through session.SandboxGitRefusal when the allowlist lacks its verb (the
// rule the real runner applies), and answers the rest from a table keyed by
// the call's arguments. A call the table does not hold is reported, never
// answered with an empty success the guard could read as a clean tree.
type revertGuardSandboxScript struct {
	t     *testing.T
	rec   session.SandboxRecord
	calls []string
	// answers is what the clone prints for one call, or the error it answers
	// with.
	answers map[string]overrunSandboxSaveAnswer
}

func (s *revertGuardSandboxScript) run(dir string, args ...string) ([]byte, error) {
	key := strings.Join(args, " ")
	s.calls = append(s.calls, key)
	if err := session.SandboxGitRefusal(s.rec, dir, args); err != nil {
		s.t.Errorf("checkpointRevertGuard issued in-sandbox git %q, which the allowlist refuses: %v", key, err)
		return nil, err
	}
	answer, ok := s.answers[key]
	if !ok {
		s.t.Errorf("checkpointRevertGuard issued an unscripted in-sandbox git call %q", key)
		return nil, errors.New("unscripted in-sandbox git call: " + key)
	}
	return []byte(answer.out), answer.err
}

// TestCheckpointRevertGuardSandboxRunsOnlyAllowedVerbs pins the in-sandbox
// allowlist against every verb the checkpoint revert guard issues through a
// sandbox seat's handle (gt-fzuvn5).
//
// The guard fails closed, so a verb the allowlist lacks refuses every
// checkpoint of the seat's work: the daemon log of 2026-11-18 21:23:47 CST
// records a seat on gt-k6t2b3 holding about 395 uncommitted rework lines being
// stopped with "could not inspect the pending checkpoint tree (git write-tree
// failed): refusing git verb \"write-tree\"". The script below refuses what
// session.SandboxGitRefusal refuses, so a later guard change that issues one
// more verb fails this test naming that verb rather than losing a seat's work
// again.
func TestCheckpointRevertGuardSandboxRunsOnlyAllowedVerbs(t *testing.T) {
	t.Parallel()
	const (
		clone     = "/sandbox/gt/gastown/polecats/capable/gastown"
		targetSHA = "1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d"
		treeSHA   = "2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e"
		baseSHA   = "3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f"
	)
	script := &revertGuardSandboxScript{
		t: t,
		answers: map[string]overrunSandboxSaveAnswer{
			// The fork-aware base ref: origin is the only remote, so the
			// target stays origin/main.
			"remote get-url origin":   {out: "git@forgejo:steveyegge/gastown.git\n"},
			"remote get-url upstream": {err: errors.New("fatal: No such remote 'upstream'")},
			// The target resolves, so the guard goes on to read the pending
			// tree and compare it with origin/main's history.
			"rev-parse origin/main":       {out: targetSHA + "\n"},
			"write-tree":                  {out: treeSHA + "\n"},
			"merge-base origin/main HEAD": {out: baseSHA + "\n"},
			// The merge base, the target (by ref, as the guard names it) and
			// the pending tree.
			"ls-tree -r -z --full-tree " + baseSHA:  {},
			"ls-tree -r -z --full-tree origin/main": {},
			"ls-tree -r -z --full-tree " + treeSHA:  {},
			// All three trees are empty, so origin/main's history changes
			// nothing and the pending tree reverts nothing: the checkpoint is
			// safe and the guard passes without a waiver read.
			"log --raw --no-abbrev --no-renames -z --format=C %H %P origin/main -n 2000": {},
		},
	}
	// The handle is the one the overrun stop saves with, built by
	// overrunSandboxGit over the scripted runner: a git.NewGitWithRemoteExec
	// over sandboxGitExec, not a handle assembled by hand.
	d, _ := newOverrunDaemon(t, false)
	reg := session.NewPrefixRegistry()
	reg.Register(overrunStopPrefix, stopSeat.Rig)
	d.prefixRegistryFn = func() *session.PrefixRegistry { return reg }
	d.readSandboxRecord = func(townRoot, name string) (session.SandboxRecord, bool, error) {
		return *overrunStopSandboxRecord(clone), true, nil
	}
	d.sandboxGitRunnerFn = func(townRoot string, rec session.SandboxRecord) (func(dir string, args ...string) ([]byte, error), error) {
		script.rec = rec
		return script.run, nil
	}
	g, sandboxed, err := d.overrunSandboxGit(stopSeat)
	if err != nil || !sandboxed {
		t.Fatalf("overrunSandboxGit = %v, %v; want the seat's in-sandbox handle", sandboxed, err)
	}

	blocked, reason := d.checkpointRevertGuard(g, stopSeat.Rig, stopSeat.Polecat)

	if blocked {
		t.Fatalf("checkpointRevertGuard blocked (%q); the clone's tree reverts nothing, so the calls above are the fault", reason)
	}
	// The guard must have gone past the pending-tree read: without it, a
	// refusal of write-tree (the incident's verb) would leave the remaining
	// calls unrecorded and this pin would pass vacuously.
	if !slices.Contains(script.calls, "write-tree") {
		t.Fatalf("in-sandbox git calls = %v, want the guard to write the pending tree", script.calls)
	}
}
