package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// TestTownDeployerStepsCalls: the plan and apply steps call the plan and apply
// code once with the deployer's kubeconfigs and return its error unchanged, and
// Plan returns the plan the record names (gt-1afrbd).
func TestTownDeployerStepsCalls(t *testing.T) {
	t.Parallel()
	kubeconfigs := planFlags{policyKubeconfig: "/d/policy.kubeconfig", applyKubeconfig: "/d/apply.kubeconfig"}
	name := "2026-11-19-first"
	sentinel := errors.New("the code failed")

	t.Run("Plan calls the plan code once with the deployer's kubeconfigs", func(t *testing.T) {
		t.Parallel()
		state := t.TempDir()
		s := &realSteps{state: state, kubeconfigs: kubeconfigs}
		id := strings.Repeat("a1", 32)
		var calls int
		var dir string
		var got planFlags
		s.render = func(_ context.Context, _ update.RunFunc, _ io.Writer, d string, f planFlags) error {
			calls, dir, got = calls+1, d, f
			return writeStepsPlan(t, d, id)
		}

		plan, err := s.Plan(context.Background(), name)

		if err != nil {
			t.Fatalf("Plan = %v", err)
		}
		if calls != 1 || dir != filepath.Join(state, name) || got != kubeconfigs {
			t.Errorf("the plan code ran %d times at %q with %+v, want once at %q with %+v", calls, dir, got, filepath.Join(state, name), kubeconfigs)
		}
		wantVersions := map[string]string{"deployment/gt-system/gt-daemon": "7"}
		if plan.Cluster != clusterapproval.ClusterControl || plan.SHA256 != id || !slices.Equal(plan.Phases, []string{"policy", "workloads"}) || !maps.Equal(plan.Versions, wantVersions) {
			t.Errorf("Plan = %+v, want the plan the record names", plan)
		}
		s.render = func(context.Context, update.RunFunc, io.Writer, string, planFlags) error { return sentinel }
		if _, err := s.Plan(context.Background(), name); err != sentinel {
			t.Errorf("Plan = %v, want the plan code's error unchanged", err)
		}
	})

	t.Run("Apply calls the apply code once with the deployer's kubeconfigs", func(t *testing.T) {
		t.Parallel()
		state, approvals := t.TempDir(), "/d/state/approvals"
		s := &realSteps{state: state, approvals: approvals, kubeconfigs: kubeconfigs}
		verified := clusterapproval.Plan{Cluster: clusterapproval.ClusterControl, SHA256: strings.Repeat("a1", 32), Phases: []string{"workloads"}}
		_, id, err := clusterapproval.NewProposal(verified)
		if err != nil {
			t.Fatalf("the verified plan is not a proposal: %v", err)
		}
		var calls int
		var dir, approval string
		var got planFlags
		s.apply = func(_ context.Context, _ update.RunFunc, _ io.Writer, d, a string, f planFlags) error {
			calls, dir, approval, got = calls+1, d, a, f
			return sentinel
		}

		err = s.Apply(context.Background(), name, verified, nil)

		if err != sentinel {
			t.Fatalf("Apply = %v, want the apply code's error unchanged", err)
		}
		if calls != 1 || dir != filepath.Join(state, name) || approval != filepath.Join(approvals, id+".json") || got != kubeconfigs {
			t.Errorf("the apply code ran %d times at %q with approval %q and %+v, want once at %q with %q and %+v", calls, dir, approval, got, filepath.Join(state, name), filepath.Join(approvals, id+".json"), kubeconfigs)
		}
	})

	t.Run("refuses a plan that is not a directory name or a proposal, and runs no code", func(t *testing.T) {
		t.Parallel()
		for _, bad := range []string{"", ".", "..", "../x", "a/b"} {
			s := &realSteps{state: t.TempDir()}
			called := false
			s.render = func(context.Context, update.RunFunc, io.Writer, string, planFlags) error { called = true; return nil }
			s.apply = func(context.Context, update.RunFunc, io.Writer, string, string, planFlags) error {
				called = true
				return nil
			}
			s.rollback = func(context.Context, update.RunFunc, io.Writer, string, string) error { called = true; return nil }
			if _, err := s.Plan(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "not a plan directory name") {
				t.Errorf("Plan(%q) = %v, want a refusal naming the plan", bad, err)
			}
			if err := s.Apply(context.Background(), bad, clusterapproval.Plan{}, nil); err == nil || !strings.Contains(err.Error(), "not a plan directory name") {
				t.Errorf("Apply(%q) = %v, want a refusal naming the plan", bad, err)
			}
			if err := s.Rollback(context.Background(), bad, clusterapproval.Plan{}); err == nil || !strings.Contains(err.Error(), "not a plan directory name") {
				t.Errorf("Rollback(%q) = %v, want a refusal naming the plan", bad, err)
			}
			if called {
				t.Errorf("the code ran for the name %q, want nothing", bad)
			}
		}
		s := &realSteps{state: t.TempDir()}
		called := false
		s.apply = func(context.Context, update.RunFunc, io.Writer, string, string, planFlags) error {
			called = true
			return nil
		}
		if err := s.Apply(context.Background(), name, clusterapproval.Plan{}, nil); err == nil || !strings.Contains(err.Error(), "no proposal id") {
			t.Errorf("Apply = %v, want a refusal naming the proposal id", err)
		}
		if called {
			t.Error("the apply code ran for a plan that is not a proposal")
		}
	})

	t.Run("Plan reports a plan directory and a plan record it cannot read", func(t *testing.T) {
		t.Parallel()
		root := t.TempDir()
		file := filepath.Join(root, "state-file")
		if err := os.WriteFile(file, nil, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := (&realSteps{state: file}).Plan(context.Background(), name); err == nil || !strings.Contains(err.Error(), "clear the plan directory") {
			t.Errorf("Plan over a state file = %v, want a refusal naming the plan directory", err)
		}
		dangling := filepath.Join(root, "dangling")
		if err := os.Symlink(filepath.Join(root, "missing"), dangling); err != nil {
			t.Fatal(err)
		}
		if _, err := (&realSteps{state: dangling}).Plan(context.Background(), name); err == nil || !strings.Contains(err.Error(), "make the plan directory") {
			t.Errorf("Plan over a dangling state = %v, want a refusal naming the plan directory", err)
		}
		s := &realSteps{state: t.TempDir(), kubeconfigs: kubeconfigs}
		s.render = func(context.Context, update.RunFunc, io.Writer, string, planFlags) error { return nil }
		if _, err := s.Plan(context.Background(), name); err == nil || !strings.Contains(err.Error(), "plan record") {
			t.Errorf("Plan without a plan.json = %v, want a refusal naming the plan record", err)
		}
	})

	t.Run("the steps of later beads refuse until they are wired", func(t *testing.T) {
		t.Parallel()
		s := &realSteps{}
		if _, err := s.LiveVersions(context.Background(), clusterapproval.Plan{}); err == nil || !strings.Contains(err.Error(), "live versions") {
			t.Errorf("LiveVersions = %v, want a refusal naming the live versions", err)
		}
		if _, err := s.ReadRecord(context.Background(), "0123456789ab"); err == nil || !strings.Contains(err.Error(), "approval record") {
			t.Errorf("ReadRecord = %v, want a refusal naming the approval record", err)
		}
	})

	t.Run("WriteProposal writes the proposal under the proposals directory", func(t *testing.T) {
		t.Parallel()
		proposals := t.TempDir()
		data := []byte(`{"act":"apply-plan"}`)
		if err := (&realSteps{proposals: proposals}).WriteProposal(context.Background(), name, data, "0123456789ab"); err != nil {
			t.Fatalf("WriteProposal = %v", err)
		}
		if got, err := os.ReadFile(filepath.Join(proposals, "0123456789ab.json")); err != nil || !bytes.Equal(got, data) {
			t.Errorf("the proposal file = %q, %v; want the proposal's bytes", got, err)
		}
	})

	t.Run("newRealSteps takes the deployer's kubeconfigs from --state's parent", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		state := filepath.Join(dir, "state")
		s := newRealSteps(state, filepath.Join(dir, "inbox"))
		want := planFlags{
			policyKubeconfig: filepath.Join(dir, "gt-deployer-policy.kubeconfig"),
			applyKubeconfig:  filepath.Join(dir, "gt-deployer-apply.kubeconfig"),
		}
		if s.kubeconfigs != want {
			t.Errorf("the kubeconfigs = %+v, want %+v", s.kubeconfigs, want)
		}
		if s.proposals != filepath.Join(dir, "proposals") || s.approvals != filepath.Join(state, "approvals") {
			t.Errorf("the proposals and approvals directories = %q and %q, want them beside the inbox and under --state", s.proposals, s.approvals)
		}
		if s.rollback == nil {
			t.Error("newRealSteps does not wire the rollback code")
		}
	})
}

// TestTownDeployerStepsRollback: the verified plan calls the rollback code once
// with the deployer-apply kubeconfig and its error is returned unchanged; a plan
// directory holding another digest or no plan record undoes nothing (gt-ws27qr).
func TestTownDeployerStepsRollback(t *testing.T) {
	t.Parallel()
	kubeconfigs := planFlags{policyKubeconfig: "/d/policy.kubeconfig", applyKubeconfig: "/d/apply.kubeconfig"}
	approved, other := strings.Repeat("a1", 32), strings.Repeat("b2", 32)
	sentinel := errors.New("the rollback failed")
	dir := filepath.Join(t.TempDir(), "plan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	calls, kubeconfig := 0, ""
	s := &realSteps{state: filepath.Dir(dir), kubeconfigs: kubeconfigs}
	s.rollback = func(_ context.Context, _ update.RunFunc, _ io.Writer, _, k string) error {
		calls, kubeconfig = calls+1, k
		return sentinel
	}
	if err := writeStepsPlan(t, dir, approved); err != nil {
		t.Fatal(err)
	}

	if err := s.Rollback(context.Background(), "plan", clusterapproval.Plan{SHA256: approved}); err != sentinel || calls != 1 || kubeconfig != kubeconfigs.applyKubeconfig {
		t.Errorf("Rollback = %v after %d calls with kubeconfig %q, want the error unchanged from one call with the apply kubeconfig", err, calls, kubeconfig)
	}
	if err := s.Rollback(context.Background(), "plan", clusterapproval.Plan{SHA256: other}); err == nil || !strings.Contains(err.Error(), approved) || !strings.Contains(err.Error(), other) || calls != 1 {
		t.Errorf("Rollback = %v after %d calls, want a refusal naming both digests %s and %s and no call", err, calls, approved, other)
	}
	// A state with no plan record is refused before the code runs: the fake
	// rollback returns sentinel, which names no plan record.
	s.state = t.TempDir()
	if err := s.Rollback(context.Background(), "plan", clusterapproval.Plan{SHA256: approved}); err == nil || !strings.Contains(err.Error(), "plan record") {
		t.Errorf("Rollback with no plan record = %v, want a refusal naming the plan record", err)
	}
}

// writeStepsPlan writes the plan.json a plan run leaves in dir, with a phase P
// file, a held Deployment in phase W and a Service the cluster does not hold, so
// the read drops the absent object's version.
func writeStepsPlan(t *testing.T, dir, id string) error {
	t.Helper()
	files := []planRecordFile{
		{Name: "01-priorityclass-x.yaml", Phase: planPhaseStatic, SHA256: "aa", DryRun: "ok"},
		{Name: "02-deployment-gt-daemon.yaml", Phase: planPhaseWorkloads, SHA256: "bb", ResourceVersion: "7", DryRun: "ok"},
		{Name: "03-service-x.yaml", Phase: planPhaseWorkloads, SHA256: "cc", Absent: true, DryRun: "ok"},
	}
	data, err := json.Marshal(planRecord{Cluster: clusterapproval.ClusterControl, ID: id, Files: files})
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, planFileName), data, 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "02-deployment-gt-daemon.yaml"), []byte("kind: Deployment\nmetadata:\n  name: gt-daemon\n  namespace: gt-system\n"), 0o644)
}
