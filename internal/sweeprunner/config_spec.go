package sweeprunner

import (
	"fmt"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
)

// ConfigSpec returns the Spec fields cfg's runner block supplies — the run's
// amounts and engine settings — leaving the rest to the caller; a nil cfg
// returns the zero Spec, which Validate refuses.
func ConfigSpec(cfg *config.TierSweepRunnerConfig) Spec {
	if cfg == nil {
		return Spec{}
	}
	return Spec{
		CPUs:                cfg.CPUs,
		MemoryGB:            cfg.Memory,
		Jobs:                cfg.Jobs,
		Dind:                cfg.Dind,
		DindCPUs:            cfg.DindCPUs,
		RunnerCPURequest:    cfg.RunnerCPURequest,
		Context:             cfg.Context,
		Namespace:           cfg.NamespaceName(),
		ProtectedNamespaces: cfg.ProtectedNamespaceList(),
	}
}

// The names a capacity preflight renders under. The Job's amount comes from
// the runner block alone, so none of these changes a number the check judges.
const (
	preflightRig   = "capacity"
	preflightSHA   = "0000000000000000000000000000000000000000"
	preflightRunID = "capacity-check"
	preflightImage = "capacity-check:none"
)

// CapacityPreflight returns the capacity check a run sized by cfg must pass
// before it applies anything, or the error refusing one: a nil cfg names no
// tier sweep runner, and a block Render cannot size is returned wrapped.
func CapacityPreflight(cfg *config.TierSweepRunnerConfig, townRoot string) (*Preflight, error) {
	if cfg == nil {
		return nil, fmt.Errorf("sweeprunner: the config names no tier sweep runner")
	}
	spec := ConfigSpec(cfg)
	spec.Rig = preflightRig
	spec.SHA = preflightSHA
	spec.RunID = preflightRunID
	spec.Image = preflightImage
	spec.OutDir = filepath.Join(cfg.OutputRoot, preflightRig, preflightRunID)
	spec.RepoDir = filepath.Join(townRoot, "repo.git")
	spec.Snapshot = filepath.Join(townRoot, "snapshot.git")
	plan, err := Kubernetes{}.Render(spec)
	if err != nil {
		return nil, fmt.Errorf("sweeprunner: rendering the capacity preflight: %w", err)
	}
	return plan.Preflight, nil
}
