package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/deployercred"
	"github.com/steveyegge/gastown/internal/update"
)

// realSteps is deployerinbox.Steps over the real plan and apply code: each plan
// is a directory under the runner's state, and the runs take the deployer's own
// two kubeconfigs. render and apply are the seams their test replaces, so the
// wiring is checked with no cluster and no town.
type realSteps struct {
	state       string // the runner's --state
	proposals   string // where WriteProposal writes
	approvals   string // where Apply reads the record its approval step wrote
	kubeconfigs planFlags
	kube        update.RunFunc
	render      func(context.Context, update.RunFunc, io.Writer, string, planFlags) error
	apply       func(context.Context, update.RunFunc, io.Writer, string, string, planFlags) error
}

// newRealSteps is the steps `gt town deployer serve` runs unless a test injects
// its own. --state's parent is where `gt town deployer kubeconfig --dir` wrote
// the deployer's kubeconfigs, and the proposals directory sits beside the
// inbox, as the answers directory does.
func newRealSteps(state, inbox string) *realSteps {
	dir := filepath.Dir(state)
	s := &realSteps{
		state:     state,
		proposals: filepath.Join(filepath.Dir(inbox), "proposals"),
		approvals: filepath.Join(state, "approvals"),
		kubeconfigs: planFlags{
			policyKubeconfig: filepath.Join(dir, deployerNamedKubeconfig(deployercred.IdentityPolicy)),
			applyKubeconfig:  filepath.Join(dir, deployerNamedKubeconfig(deployercred.IdentityApply)),
		},
		kube: realRunFunc(),
	}
	s.render = func(ctx context.Context, kube update.RunFunc, out io.Writer, dir string, f planFlags) error {
		return runTownClusterPlan(ctx, realClusterRenderDeps(out), kube, out, dir, f, atomicfile.WriteJSON)
	}
	s.apply = func(ctx context.Context, kube update.RunFunc, out io.Writer, planDir, approvalPath string, f planFlags) error {
		return runTownClusterApply(ctx, kube, out, planDir, approvalPath, f, applyRecordVerifier(time.Now))
	}
	return s
}

// Plan renders the plan named name into <state>/<name>, replacing the earlier
// rendering, and reads the plan its approval record will name. The code's
// error is returned unchanged.
func (s *realSteps) Plan(ctx context.Context, name string) (clusterapproval.Plan, error) {
	dir, err := s.planDir(name)
	if err != nil {
		return clusterapproval.Plan{}, err
	}
	if err := os.RemoveAll(dir); err != nil {
		return clusterapproval.Plan{}, fmt.Errorf("gt town deployer steps: cannot clear the plan directory %s: %w", dir, err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return clusterapproval.Plan{}, fmt.Errorf("gt town deployer steps: cannot make the plan directory %s: %w", dir, err)
	}
	if err := s.render(ctx, s.kube, io.Discard, dir, s.kubeconfigs); err != nil {
		return clusterapproval.Plan{}, err
	}
	return readPlanApproval(dir, s.kubeconfigs)
}

// WriteProposal writes proposal into the proposals directory, named for the
// proposal id Handle derived from the plan.
func (s *realSteps) WriteProposal(_ context.Context, _ string, proposal []byte, id string) error {
	return os.WriteFile(filepath.Join(s.proposals, id+".json"), proposal, 0o644)
}

// Apply applies the plan named name under the approval record the plan's own
// proposal id names, and returns the apply code's error unchanged.
func (s *realSteps) Apply(ctx context.Context, name string, verified clusterapproval.Plan, _ map[string]string) error {
	dir, err := s.planDir(name)
	if err != nil {
		return err
	}
	// The record's path comes from the plan Verify checked, never from a value
	// read since: the proposal id is the plan's own hash.
	_, id, err := clusterapproval.NewProposal(verified)
	if err != nil {
		return fmt.Errorf("gt town deployer steps: the verified plan has no proposal id: %w", err)
	}
	return s.apply(ctx, s.kube, io.Discard, dir, filepath.Join(s.approvals, id+".json"), s.kubeconfigs)
}

// LiveVersions, ReadRecord and Rollback are beads gt-fxl8ab's and gt-ws27qr's;
// until they land each refuses, so a request that needs one is answered failed
// rather than acted on unchecked.
func (s *realSteps) LiveVersions(context.Context, clusterapproval.Plan) (map[string]string, error) {
	return nil, errors.New("gt town deployer steps: no live versions read is wired, so the plan cannot be verified")
}

func (s *realSteps) ReadRecord(context.Context, string) (clusterapproval.Record, error) {
	return clusterapproval.Record{}, errors.New("gt town deployer steps: no approval record read is wired, so no plan is approved")
}

func (s *realSteps) Rollback(context.Context, string, clusterapproval.Plan) error {
	return errors.New("gt town deployer steps: no rollback is wired, so nothing is undone")
}

// planDir is the directory the plan named name is rendered into. The name comes
// from a request file, so it is held to a plain directory name before it reaches
// a path: a name that is a path, ".", "..", or empty is refused.
func (s *realSteps) planDir(name string) (string, error) {
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return "", fmt.Errorf("gt town deployer steps: the plan %q is not a plan directory name", name)
	}
	return filepath.Join(s.state, name), nil
}

// readPlanApproval reads the plan the plan code wrote in dir as the approval
// record names it: the full id as its SHA-256, its phases in apply order, and
// each held Deployment's resourceVersion keyed as readApplyDeployment builds it.
// An object the cluster does not hold carries no version, so it is not in the
// map: Verify checks the objects the plan names and no others.
func readPlanApproval(dir string, f planFlags) (clusterapproval.Plan, error) {
	// loadApplyPlan holds every file to its phase's kubeconfig and refuses a
	// phase but P and W, so the switch below sees only those two.
	record, held, err := loadApplyPlan(dir, f)
	if err != nil {
		return clusterapproval.Plan{}, fmt.Errorf("gt town deployer steps: %w", err)
	}
	plan := clusterapproval.Plan{Cluster: record.Cluster, SHA256: record.ID, Versions: map[string]string{}}
	for _, d := range held {
		plan.Versions[d.key] = d.version
	}
	for _, file := range record.Files {
		phase := map[string]string{planPhaseStatic: "policy", planPhaseWorkloads: "workloads"}[file.Phase]
		if !slices.Contains(plan.Phases, phase) {
			plan.Phases = append(plan.Phases, phase)
		}
	}
	return plan, nil
}
