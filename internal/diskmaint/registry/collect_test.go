package registry

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"github.com/steveyegge/gastown/internal/diskmaint"
)

func TestRegistryCollectUnavailableIsRecordedAsSkipped(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, run.run)
	cfg.CollectUnavailable = "the Mac job collects"

	result, err := NewTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1")))
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if len(result.Done) != 1 {
		t.Fatalf("done = %+v, want the delete kept", result.Done)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want no collect from a process that cannot run one")
	}
	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none", run.calls)
	}
	want := []string{"collect skipped: the Mac job collects"}
	if entry := readLedger(t, path)[0]; !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v", entry.Errors, want)
	}
}

func TestRegistryCollectOnlyRunsWithoutDeletes(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, run.run)
	target := NewCollectTarget(cfg)

	plan, err := target.Plan(context.Background(), diskmaint.Policy{Keep: 5, Apply: true})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !result.Collected {
		t.Fatal("result.Collected = false; want the collect run although nothing was deleted")
	}
	if len(result.Done) != 0 {
		t.Fatalf("done = %+v, want no delete", result.Done)
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none from a collect-only run", got)
	}
	argv, env := wantCollect(cfg.Container, cfg.DockerHost)
	if len(run.calls) != 1 || !slices.Equal(run.calls[0], argv) {
		t.Fatalf("commands = %v, want the one collect %v", run.calls, argv)
	}
	if !slices.Equal(run.envs[0], env) {
		t.Fatalf("collect env = %v, want %v", run.envs[0], env)
	}
	entry := readLedger(t, path)[0]
	if entry.Target != collectTarget || entry.Done != 0 || len(entry.Errors) != 0 {
		t.Fatalf("ledger = %+v, want a clean %s line with no deletes", entry, collectTarget)
	}
}

func TestRegistryCollectOnlyRefusesWhilePushInFlight(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, run.run)
	cfg.PushInFlight = func() bool { return true }
	target := NewCollectTarget(cfg)

	plan, err := target.Plan(context.Background(), diskmaint.Policy{Keep: 5, Apply: true})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if result.Collected {
		t.Fatal("result.Collected = true; want no collect while a push is in flight")
	}
	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none", run.calls)
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none", got)
	}
	want := []string{"collect skipped: a push is in flight"}
	if entry := readLedger(t, path)[0]; !slices.Equal(entry.Errors, want) {
		t.Fatalf("ledger errors = %v, want %v", entry.Errors, want)
	}
}

func TestRegistryCollectOnlyRefusesADeletePlan(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	path := ledgerPath(t)
	cfg := gcConfig(t, reg, path, run.run)

	_, err := NewCollectTarget(cfg).Apply(context.Background(), deletePlan(refInRepo("a1")))
	if err == nil {
		t.Fatal("apply of a delete plan = nil error; want the collect-only target to refuse it")
	}
	if got := reg.deleted(); len(got) != 0 {
		t.Fatalf("deletes = %v, want none", got)
	}
	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want none", run.calls)
	}
	if entries := readLedger(t, path); len(entries) != 1 || len(entries[0].Errors) == 0 {
		t.Fatalf("ledger = %+v, want the refusal recorded", entries)
	}
}

func TestRegistryCollectOnlyDefersWhenLockHeld(t *testing.T) {
	t.Parallel()
	reg := newRecordingRegistry(nil)
	run := &fakeRunner{}
	cfg := gcConfig(t, reg, ledgerPath(t), run.run)
	cfg.Lock = func(context.Context, func(context.Context) error) error {
		return fmt.Errorf("polecat-image lock: %w", ErrDeferred)
	}
	target := NewCollectTarget(cfg)

	plan, err := target.Plan(context.Background(), diskmaint.Policy{Keep: 5, Apply: true})
	if err != nil {
		t.Fatalf("plan: %v", err)
	}
	result, err := target.Apply(context.Background(), plan)
	if err != nil {
		t.Fatalf("apply: %v; a held lock defers and is not a failure", err)
	}
	if result.Deferred == "" {
		t.Fatal("Result.Deferred is empty; want the reason the collect deferred")
	}
	if len(run.calls) != 0 {
		t.Fatalf("commands = %v, want no collect while another run holds the lock", run.calls)
	}
}

func TestRegistryCollectOnlyMeasureRefuses(t *testing.T) {
	t.Parallel()
	cfg := gcConfig(t, newRecordingRegistry(nil), ledgerPath(t), nil)

	if _, err := NewCollectTarget(cfg).Measure(context.Background()); err == nil {
		t.Fatal("Measure() = nil error; a collect-only target measures nothing and must say so")
	}
}
