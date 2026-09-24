package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/config"
)

// testcontainersModule is the Go module whose presence in a rig's go.mod is
// what lets that rig's test suite start testcontainers containers at all.
const testcontainersModule = "github.com/testcontainers/testcontainers-go"

// preVerifySlot is who the --pre-verified test gate holds a container-gate
// slot as (gt-l6by): the town whose pool it draws from and the holder role,
// "<rig>/<polecat>" — the role the default test-verify gate already uses, so a
// polecat is one holder to `gt slot status` whichever gate it ran.
//
// acquire is the seam a test replaces so the gate never touches the town's
// real slot pool; nil means acquireVerifySlotWithProgress, the default gate's
// own acquire path (slot.AcquirePool over containerGatePool, with the
// reserved-for-gate slots and the reentrant marker handled there).
type preVerifySlot struct {
	townRoot string
	role     string
	acquire  func(townRoot, role string, timeout time.Duration, logFile *os.File) (release func(), waited time.Duration, err error)
}

// preVerifySlotDecision is whether the pre-verified test gate must hold a
// container-gate slot, and the reason written to the gate log.
type preVerifySlotDecision struct {
	needed bool
	reason string
}

// resolvePreVerifyTestSlot decides whether the --pre-verified test gate can
// start a container-backed suite, and so must hold a slot.
//
// Unlike the default gate (resolveContainerSwitch), this run cannot write the
// container opt-in off: a stamped MR skips the refinery's gate
// (Engineer.resolveFastPath), so this run is the only one that exercises the
// rig's Docker suite. It therefore runs the rig's command as configured and
// decides from what that command can reach:
//
//   - a non-Go rig's command is opaque, so it takes a slot — the same
//     conservative direction the default gate takes;
//   - a Go module that does not require testcontainers-go cannot start a
//     testcontainers container, so it takes none (om's `make test`, say);
//   - a Go module that does require it takes a slot unless the command itself
//     writes GT_TEST_DOCKER=0. `make test` defaults the opt-in on
//     (GT_TEST_DOCKER=$${GT_TEST_DOCKER:-1}), so a command that says nothing
//     starts containers. The ambient value is not read, for the reason
//     resolveContainerSwitch gives (gt-0hbm): it does not describe this run.
//
// A go.mod that cannot be read is treated as requiring the module: the wrong
// guess there only costs a slot wait, the other one runs a suite unwrapped.
func resolvePreVerifyTestSlot(worktree, testCmd string) preVerifySlotDecision {
	data, err := os.ReadFile(filepath.Join(worktree, "go.mod"))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return preVerifySlotDecision{true, "not a Go module, so the test command's container use is unknown"}
	case err != nil:
		return preVerifySlotDecision{true, fmt.Sprintf("go.mod unreadable (%v), so the test command may start containers", err)}
	case !strings.Contains(string(data), testcontainersModule):
		return preVerifySlotDecision{false, "the Go module does not require " + testcontainersModule + ", so its suite starts no containers"}
	}
	if v, ok := inlineDockerTestsValue(testCmd); ok && v == "0" {
		return preVerifySlotDecision{false, fmt.Sprintf("the test command writes %s=0, so its container-backed tests skip", dockerTestsEnv)}
	}
	return preVerifySlotDecision{true, fmt.Sprintf("the Go module requires %s and the test command does not write %s=0 (make test defaults it on)", testcontainersModule, dockerTestsEnv)}
}

// inlineDockerTestsValue returns the last value the command text assigns to
// the container opt-in (GT_TEST_DOCKER=<v>, bare, quoted, or via env/export),
// and whether it assigns one at all. The last assignment is the one a shell
// running the command would leave in effect.
func inlineDockerTestsValue(command string) (string, bool) {
	prefix := dockerTestsEnv + "="
	value, found := "", false
	for _, tok := range shellTokenize(command) {
		i := strings.Index(tok, prefix)
		if i < 0 || (i > 0 && tok[i-1] != ' ') {
			continue
		}
		value, found = strings.Trim(tok[i+len(prefix):], `"'`), true
	}
	return value, found
}

// hold takes the container-gate slot for the test gate when
// resolvePreVerifyTestSlot says the gate needs one, and returns its release.
// The decision and the grant are written to logFile, so the pre-verify log
// says whether the suite ran inside a slot. A slot that cannot be acquired is
// an error: the gate must not run a container suite without one, and the
// caller turns the error into "no stamp", never into a test failure.
func (s preVerifySlot) hold(worktree, testCmd string, mq *config.MergeQueueConfig, logFile *os.File) (func(), error) {
	decision := resolvePreVerifyTestSlot(worktree, testCmd)
	if !decision.needed {
		fmt.Fprintf(logFile, "=== gate test: no container-gate slot (%s) ===\n", decision.reason)
		return func() {}, nil
	}
	if s.townRoot == "" {
		return nil, fmt.Errorf("the test gate needs a container-gate slot (%s) but no town root was resolved to take it in", decision.reason)
	}
	acquire := s.acquire
	if acquire == nil {
		acquire = acquireVerifySlotWithProgress
	}
	timeout := resolveTestVerifyBudgets(mq).slotTimeout
	fmt.Fprintf(logFile, "=== gate test: taking a container-gate slot as %s (%s; cap %s) ===\n", s.role, decision.reason, humanDuration(timeout))
	release, waited, err := acquire(s.townRoot, s.role, timeout, logFile)
	if err != nil {
		fmt.Fprintf(logFile, "=== gate test: container-gate slot not acquired after %s: %v ===\n", waited.Round(time.Second), err)
		return nil, fmt.Errorf("acquiring the container-gate slot for the test gate after %s (cap %s): %w — slot contention, not a test failure", waited.Round(time.Second), humanDuration(timeout), err)
	}
	fmt.Fprintf(logFile, "=== gate test: container-gate slot acquired after %s ===\n", waited.Round(time.Second))
	return release, nil
}
