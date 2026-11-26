package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// approvalVerifier checks an approval record against a plan, the live
// resourceVersions and the moment. The command passes clusterapproval.Verify.
type approvalVerifier func(rec clusterapproval.Record, p clusterapproval.Plan, live map[string]string, now time.Time) error

// applyPlanCluster is the cluster a plan targets: plan.json names none, and the
// plan renders only the control cluster's set.
const applyPlanCluster = clusterapproval.ClusterControl

// applyDeployment is a Deployment the plan holds at a resourceVersion: its
// Versions key, the name and namespace its live read names, and the kubeconfig
// of its phase.
type applyDeployment struct {
	key, namespace, name, version, kubeconfig string
}

// loadApplyPlan reads plan.json and the held Deployments' documents. No cluster
// is called here. Each file's phase must have its kubeconfig.
func loadApplyPlan(planDir string, kubeconfigs planFlags) (planRecord, []applyDeployment, error) {
	data, err := os.ReadFile(filepath.Join(planDir, planFileName))
	if err != nil {
		return planRecord{}, nil, fmt.Errorf("gt town cluster apply: the plan record cannot be read, so nothing was applied: %w", err)
	}
	var record planRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return planRecord{}, nil, errors.New("gt town cluster apply: the plan record is not a plan record, so nothing was applied")
	}
	var held []applyDeployment
	for _, file := range record.Files {
		kubeconfig, err := phaseKubeconfig(file.Phase, kubeconfigs)
		if err != nil {
			return planRecord{}, nil, err
		}
		if err := checkClusterKubeconfig(kubeconfig); err != nil {
			return planRecord{}, nil, err
		}
		if file.Absent || file.ResourceVersion == "" {
			continue
		}
		d, err := readApplyDeployment(planDir, file, kubeconfig)
		if err != nil {
			return planRecord{}, nil, err
		}
		held = append(held, d)
	}
	return record, held, nil
}

// readApplyDeployment reads the document a held file names. The file must be one
// name in planDir, and the document a Deployment whose name and namespace kubectl
// can take. Errors name the file, never its text.
func readApplyDeployment(planDir string, file planRecordFile, kubeconfig string) (applyDeployment, error) {
	if filepath.Base(file.Name) != file.Name {
		return applyDeployment{}, fmt.Errorf("gt town cluster apply: the plan names %q, which is not a file in the plan directory, so nothing was applied", file.Name)
	}
	data, err := os.ReadFile(filepath.Join(planDir, file.Name))
	if err != nil {
		return applyDeployment{}, fmt.Errorf("gt town cluster apply: the plan file %s cannot be read, so nothing was applied: %w", file.Name, err)
	}
	var doc struct {
		Kind     string `yaml:"kind"`
		Metadata struct {
			Name      string `yaml:"name"`
			Namespace string `yaml:"namespace"`
		} `yaml:"metadata"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil || doc.Kind != "Deployment" {
		return applyDeployment{}, fmt.Errorf("gt town cluster apply: the plan file %s is not a Deployment, so nothing was applied", file.Name)
	}
	if err := errors.Join(checkClusterName(doc.Metadata.Name), checkClusterNamespace(doc.Metadata.Namespace)); err != nil {
		return applyDeployment{}, err
	}
	return applyDeployment{
		key:        "deployment/" + doc.Metadata.Namespace + "/" + doc.Metadata.Name,
		namespace:  doc.Metadata.Namespace,
		name:       doc.Metadata.Name,
		version:    file.ResourceVersion,
		kubeconfig: kubeconfig,
	}, nil
}

// readApplyApproval reads the approval record at path. A record that does not
// parse is refused without ParseRecord's text, which can carry a value from it.
func readApplyApproval(path string) (clusterapproval.Record, error) {
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return clusterapproval.Record{}, fmt.Errorf("gt town cluster apply: there is no approval record at %s, so nothing was applied", path)
	case err != nil:
		return clusterapproval.Record{}, fmt.Errorf("gt town cluster apply: the approval record %s cannot be read, so nothing was applied: %w", path, err)
	}
	rec, err := clusterapproval.ParseRecord(data)
	if err != nil {
		return clusterapproval.Record{}, fmt.Errorf("gt town cluster apply: the approval record %s is not a valid approval record, so nothing was applied", path)
	}
	return rec, nil
}

// runTownClusterApply applies planDir only under the approval at approvalPath,
// which verify must accept against the plan, the held Deployments' live
// resourceVersions and now. The plan and the approval are read before the first
// cluster call, so a plan with no approval reads nothing live. Every refusal
// returns non-nil with nothing applied. A verified approval is refused too: the
// apply is a later step of this command, and nil would read as applied.
func runTownClusterApply(ctx context.Context, kube update.RunFunc, planDir, approvalPath string, kubeconfigs planFlags, verify approvalVerifier, now time.Time) error {
	record, held, err := loadApplyPlan(planDir, kubeconfigs)
	if err != nil {
		return err
	}
	rec, err := readApplyApproval(approvalPath)
	if err != nil {
		return err
	}
	plan := clusterapproval.Plan{Cluster: applyPlanCluster, SHA256: record.ID, Versions: map[string]string{}}
	live := map[string]string{}
	for _, d := range held {
		plan.Versions[d.key] = d.version
		// The read runs under the phase's kubeconfig, the identity it names and no other.
		under := func(ctx context.Context, dir string, env []string, name string, args ...string) (string, int, error) {
			return kube(ctx, dir, env, name, append([]string{"--kubeconfig", d.kubeconfig}, args...)...)
		}
		obj, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: under}, "deployment/"+d.name, d.namespace, false)
		if err != nil {
			return fmt.Errorf("gt town cluster apply: %w, so nothing was applied", err)
		}
		if absent {
			continue // Verify refuses a plan object the live cluster does not hold
		}
		metadata, _ := obj["metadata"].(map[string]any)
		resourceVersion, _ := metadata["resourceVersion"].(string)
		if resourceVersion == "" {
			return fmt.Errorf("gt town cluster apply: the live %s has no resourceVersion, so nothing was applied", d.key)
		}
		live[d.key] = resourceVersion
	}
	if err := verify(rec, plan, live, now); err != nil {
		return fmt.Errorf("gt town cluster apply: the approval does not verify against the plan: %w, so nothing was applied", err)
	}
	return errors.New("gt town cluster apply: the approval verifies, but this build does not apply the plan yet, so nothing was applied")
}

var (
	clusterApplyPlanDir, clusterApplyApproval string
	clusterApplyKubeconfigs                   planFlags
)

// townClusterApplyCmd applies a plan made by gt town cluster plan, only under an
// approval record for the plan's full sha256.
var townClusterApplyCmd = &cobra.Command{
	Use:   "apply",
	Short: "Apply a plan only under an approval record that verifies against it",
	Long: `Apply the plan gt town cluster plan wrote into --plan, only under the approval record at
--approval for the plan's full sha256. It refuses with nothing applied when there is no
approval record, when the approval does not parse or verify, and when a live Deployment
cannot be read. Each kubectl call runs under the kubeconfig of its phase. This build does
not apply a verified plan yet, so a verified approval is refused too.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTownClusterApply(cmd.Context(), realRunFunc(), clusterApplyPlanDir, clusterApplyApproval, clusterApplyKubeconfigs, clusterapproval.Verify, time.Now())
	},
}

func init() {
	flags := townClusterApplyCmd.Flags()
	flags.StringVar(&clusterApplyPlanDir, "plan", "", "the plan directory gt town cluster plan wrote (required)")
	flags.StringVar(&clusterApplyApproval, "approval", "", "the approval record for the plan's full sha256 (required)")
	flags.StringVar(&clusterApplyKubeconfigs.policyKubeconfig, "policy-kubeconfig", "", "phase P's kubeconfig, required when phase P has files")
	flags.StringVar(&clusterApplyKubeconfigs.applyKubeconfig, "apply-kubeconfig", "", "phase W's kubeconfig, required when phase W has files")
	_ = townClusterApplyCmd.MarkFlagRequired("plan")
	_ = townClusterApplyCmd.MarkFlagRequired("approval")
	townClusterCmd.AddCommand(townClusterApplyCmd)
}
