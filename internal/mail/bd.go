package mail

import (
	"context"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

// bdTimeout bounds every bd call mail makes, read or write. 60s accommodates
// concurrent agent load where several bd processes compete for Dolt locks and
// memory (30s was killed with signal:killed under contention).
const bdTimeout = 60 * time.Second

// bdCall is one bd invocation as mail's unit tier sees it: the working
// directory, the whole environment bd was handed, and the argv after "bd".
// Production builds the same environment and the seam runs bd with it; only
// a test's bdAnswer is handed the call instead.
type bdCall struct {
	Dir  string
	Env  []string
	Args []string
}

// bdAnswer answers one bd call in place of the process: mail's unit-tier
// double, recording or scripting what bd would say. nil runs the real bd,
// which the store's plain wrapper starts with the environment bdSubprocessEnv
// derived, in its own process group and under the store's deadline.
type bdAnswer func(ctx context.Context, c bdCall) (stdout, stderr []byte, err error)

// bdStore returns the beads client mail reads through for one beads directory:
// a plain wrapper carrying the read-only environment, the stale-PID
// pre-flight and the deadline. The store runs bd itself; a test's bdAnswer
// only replaces what bd answers.
func bdStore(run bdAnswer, workDir, beadsDir string) beads.Client {
	return newBdStore(run, workDir, beadsDir, true, "")
}

// bdAdminStore is bdStore's admin surface, for the store verbs Client does
// not carry (the wisp list).
func bdAdminStore(run bdAnswer, workDir, beadsDir string) beads.Admin {
	return newBdStore(run, workDir, beadsDir, true, "")
}

// bdWriteStore is bdStore for a write: the mutation environment. It names no
// actor, so bd records the identity the environment resolves, as the raw label
// and update commands it replaces did.
func bdWriteStore(run bdAnswer, workDir, beadsDir string) beads.Client {
	return newBdStore(run, workDir, beadsDir, false, "")
}

// bdCloseStore is bdWriteStore for the one write that must name its identity:
// bd's assignee==actor close guard refuses a close by any other actor, and a
// refused close fails `gt mail archive` town-wide (gt-ovem, sibling of
// gt-cut). The seam owns the argv now, so the identity rides the environment
// bd resolves its --actor flag default from ("default: $BEADS_ACTOR, git
// user.name, $USER"); the read and label stores keep the ambient identity.
func bdCloseStore(run bdAnswer, workDir, beadsDir, actor string) beads.Client {
	return newBdStore(run, workDir, beadsDir, false, actor)
}

// bdThreadStore is bdStore for a thread read: BD_IDENTITY names the mailbox
// the thread is read as, which the raw message-thread command passed too
// (gt-p85pw.18).
func bdThreadStore(run bdAnswer, workDir, beadsDir, identity string) beads.Client {
	if identity == "" {
		return bdStore(run, workDir, beadsDir)
	}
	return newBdStore(run, workDir, beadsDir, true, "", "BD_IDENTITY="+identity)
}

// newBdStore builds the store for one bd call: the stale-PID pre-flight runs
// here, so a caller that makes several calls builds one store per call rather
// than reusing one across them.
func newBdStore(run bdAnswer, workDir, beadsDir string, readOnly bool, actor string, extraEnv ...string) *beads.Beads {
	// Remove stale dolt-server.pid before bd runs. A stale PID file causes
	// bd to connect to port 3307 which may be occupied by a different Dolt server
	// serving different databases, resulting in hangs until the deadline kills it.
	beads.CleanStaleDoltServerPID(beadsDir)

	env := bdSubprocessEnv(beads.WithMachineEnvIn(workDir, nil), beadsDir, readOnly)
	if actor != "" {
		env = withActorEnv(env, actor)
	}
	env = withEnvOverrides(env, extraEnv...)
	var opts []beads.Option
	if run != nil {
		opts = append(opts, beads.WithDirRunner(
			func(ctx context.Context, dir string, args []string) ([]byte, []byte, error) {
				return run(ctx, bdCall{Dir: dir, Env: env, Args: args})
			}))
	}
	return beads.NewPlain(workDir, env, opts...).WithProcessGroup().WithTimeout(bdTimeout)
}

// withActorEnv makes bd resolve actor as the call's author, naming both
// variables bd reads one from: BEADS_ACTOR, which its --actor flag defaults
// to, and BD_ACTOR, the fallback the seam's WithActor sets (the pair gt's
// attribution guard treats as a bd author's environment,
// internal/cmd/tap_guard_polecat_attribution.go). The inherited values go
// first, because which of two entries for one key the child reads is not
// something this code can rely on.
func withActorEnv(env []string, actor string) []string {
	env = beads.StripEnvKey(beads.StripEnvKey(env, "BD_ACTOR"), "BEADS_ACTOR")
	return append(env, "BEADS_ACTOR="+actor, "BD_ACTOR="+actor)
}

// withEnvOverrides sets kv ("KEY=VALUE") in env, replacing every inherited
// entry for the same key: which of two entries for one key a child reads is
// not something this code can rely on.
func withEnvOverrides(env []string, kv ...string) []string {
	for _, entry := range kv {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env = beads.StripEnvKey(env, key)
		env = append(env, entry)
	}
	return env
}

func bdSubprocessEnv(baseEnv []string, beadsDir string, readOnly bool) []string {
	mode := beads.MutationRouting
	if readOnly {
		mode = beads.ReadOnlyRouting
	}
	if beadsDir != "" {
		if readOnly {
			mode = beads.ReadOnlyPinned
		} else {
			mode = beads.MutationPinned
		}
	}
	return beads.EnvForSubprocessMode(baseEnv, beadsDir, mode)
}
