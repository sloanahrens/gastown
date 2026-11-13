package daemon

import (
	"bytes"
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jonboulle/clockwork"
	agentconfig "github.com/steveyegge/gastown/internal/config"
)

// buildCheckoutPodRun runs Run as a pod's daemon on town, with the daemon's full
// build commit answered by fullSHA and its Ensure recorded in rec. It is the
// pod start runPodStart runs, with container mode and the build commit set, so
// Run reaches the start check and ends at its first select. It returns Run's
// error and the daemon log.
func buildCheckoutPodRun(t *testing.T, town string, fullSHA func() (string, error), rec *buildCheckoutEnsureRecord) (string, error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(town, "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	kv := map[string]string{
		agentconfig.ContainerModeEnv: agentconfig.SupervisorContainer,
		agentconfig.ContainerNameEnv: podSelf.ID,
	}
	var logs bytes.Buffer
	d := &Daemon{
		config:            DefaultConfig(town),
		logger:            log.New(&logs, "", 0),
		getenv:            ownerEnv(kv),
		hostname:          fixtureHostname("gastown-pod"),
		clock:             clockwork.NewFakeClockAt(guardNow),
		ownerProbe:        podProbe(t, podEngine, unreadJob(t)),
		containerMode:     true,
		fullBuildCommitFn: fullSHA,
		buildCheckoutEnsureFn: func(_ context.Context, townRoot, rig, rev string) (string, error) {
			rec.calls++
			rec.townRoot, rec.rig, rec.rev = townRoot, rig, rev
			return rec.checkout, rec.returnErr
		},
	}
	d.disabledPatrols = map[string]bool{
		"landing_infra":    true,
		"resource_sampler": true,
		"spec_dispatch":    true,
	}
	d.seams.heartbeatWork = func(*Daemon, *State, bool) {}
	d.seams.cleanupLegacySockets = func(string) (int, int) { return 0, 0 }
	d.seams.startLocalRoundPatrols = func(*Daemon) {}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.cancel()
	err := d.Run()
	return logs.String(), err
}

// A pod whose full build commit and checkout resolve runs the start check and
// goes on: Run does not refuse, and Ensure is asked once for the daemon's own
// full sha.
func TestBuildCheckoutStartPassesAtTheRun(t *testing.T) {
	t.Parallel()
	town, checkout := buildCheckoutTown(t)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	_, err := buildCheckoutPodRun(t, town, func() (string, error) {
		return buildCheckoutTestSHA, nil
	}, rec)
	if err != nil {
		t.Fatalf("Run = %v, want a pod that started", err)
	}
	if rec.calls != 1 || rec.rev != buildCheckoutTestSHA {
		t.Fatalf("Ensure called %d times at rev %q, want once at %q", rec.calls, rec.rev, buildCheckoutTestSHA)
	}
}

// A pod whose build commit is not known in full does not start: Run refuses at
// the start check, before the daemon dispatches anything, and never reaches the
// checkout.
func TestBuildCheckoutStartRefusesTheRunOfAPodWithNoFullSHA(t *testing.T) {
	t.Parallel()
	town, checkout := buildCheckoutTown(t)
	rec := &buildCheckoutEnsureRecord{checkout: checkout}
	_, err := buildCheckoutPodRun(t, town, func() (string, error) {
		return "", errors.New("the binary records no vcs.revision")
	}, rec)
	if err == nil || !strings.Contains(err.Error(), "refusing to start") {
		t.Fatalf("Run = %v, want the start refusal", err)
	}
	if rec.calls != 0 {
		t.Fatalf("Ensure called %d times on a refused start, want none", rec.calls)
	}
}
