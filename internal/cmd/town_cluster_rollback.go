package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/update"
)

// townClusterRollbackCmd is `gt town cluster rollback`, registered by this file's init.
var townClusterRollbackCmd = &cobra.Command{
	Use:   "rollback",
	Short: "Undo a plan's Deployments to their recorded revisions",
	Long: `Undo each Deployment the plan holds to the revision plan.json records for it:
one kubectl rollout undo per Deployment, in the reverse of the apply's order, each
waited for before the next. The deployer's runner checks the approval first; this
command does not. A refused flag or kubeconfig path records nothing; once both are
accepted, whatever the outcome, one line is appended to rollback-ledger.jsonl in
the plan directory.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		planDir, _ := cmd.Flags().GetString("plan")
		kubeconfig, _ := cmd.Flags().GetString("kubeconfig")
		return runTownClusterRollback(cmd.Context(), realRunFunc(), cmd.OutOrStdout(), planDir, kubeconfig)
	},
}

func init() {
	townClusterRollbackCmd.Flags().String("plan", "", "the plan directory holding plan.json and its files (required)")
	townClusterRollbackCmd.Flags().String("kubeconfig", "", "the deployer-apply certificate's kubeconfig (required)")
	_ = townClusterRollbackCmd.MarkFlagRequired("plan")
	_ = townClusterRollbackCmd.MarkFlagRequired("kubeconfig")
	townClusterCmd.AddCommand(townClusterRollbackCmd)
}

// rollbackLedgerName is the ledger a rollback appends one line to, beside plan.json.
const rollbackLedgerName = "rollback-ledger.jsonl"

// The results a rollback ledger line records: undone, refused (nothing undone) and
// partial (some Deployments undone before the run stopped).
const (
	rollbackResultUndone  = "undone"
	rollbackResultPartial = "partial"
	rollbackResultRefused = "refused"
)

// runTownClusterRollback undoes each Deployment the plan holds to its recorded
// revision, through kubeconfig, in the reverse of the apply's order. The plan's
// files must match their hashes, and every revision is checked before the first
// kubectl call, so a bad plan refuses with nothing undone. A failed undo or wait
// stops the run. A refusal of the flags or the kubeconfig path returns before any
// record. Past those, the run appends one ledger line beside plan.json on every
// outcome; a line that cannot be written is an error, so a cluster change is never unrecorded.
func runTownClusterRollback(ctx context.Context, kube update.RunFunc, out io.Writer, planDir, kubeconfig string) (err error) {
	// An empty plan path would read the working directory, and an empty kubeconfig
	// would fall back to the operator's default cluster; both refuse before any read.
	if planDir == "" || kubeconfig == "" {
		return errors.New("gt town cluster rollback: --plan and --kubeconfig must both name a file, so nothing was rolled back")
	}
	if err := checkClusterKubeconfig(kubeconfig); err != nil {
		return err
	}
	planID, undone := "", []string{}
	defer func() {
		result := rollbackResultRefused
		switch {
		case err == nil:
			result = rollbackResultUndone
		case len(undone) > 0:
			result = rollbackResultPartial
		}
		entry := applyLedgerEntry{TS: time.Now().UTC(), Plan: planID, Objects: undone, Result: result}
		if ledgerErr := (applyLedger{path: filepath.Join(planDir, rollbackLedgerName), open: openApplyLedger}).append(entry); ledgerErr != nil {
			err = errors.Join(err, fmt.Errorf("gt town cluster rollback: the run's outcome is not recorded, and its ledger line could not be written: %w", ledgerErr))
		}
	}()
	data, err := os.ReadFile(filepath.Join(planDir, planFileName))
	if err != nil {
		return fmt.Errorf("gt town cluster rollback: the plan record cannot be read, so nothing was rolled back: %w", err)
	}
	var record planRecord
	if err := json.Unmarshal(data, &record); err != nil {
		return errors.New("gt town cluster rollback: the plan record is not a plan record, so nothing was rolled back")
	}
	planID = record.ID
	files, err := checkApplyPlanFiles(planDir, record)
	if err != nil {
		return err
	}
	ordered, err := applySteps(files)
	if err != nil {
		return err
	}
	// Walking the apply's order backwards undoes the bd gateway last. A Deployment
	// the plan marks absent was never applied, so there is nothing to undo.
	var steps []applyStep
	for i := len(ordered) - 1; i >= 0; i-- {
		step := ordered[i]
		if step.deployment == "" || step.file.file.Absent {
			continue
		}
		if !planRevisionIsNumber(step.file.file.Revision) {
			return fmt.Errorf("gt town cluster rollback: plan.json records no revision for the plan file %s, so the undo of deployment/%s would name none, and nothing was rolled back", step.file.file.Name, step.deployment)
		}
		steps = append(steps, step)
	}
	runner := applyKubeconfigRunner(kube, kubeconfig)
	for _, step := range steps {
		resource := "deployment/" + step.deployment
		if _, exit, err := runner(ctx, planDir, nil, "kubectl", "rollout", "undo", resource, "-n", step.namespace, "--to-revision="+step.file.file.Revision); err != nil || exit != 0 {
			return fmt.Errorf("gt town cluster rollback: the undo of %s failed, so the rollback stopped with %d of %d Deployments undone", resource, len(undone), len(steps))
		}
		undone = append(undone, "deployment/"+step.namespace+"/"+step.deployment)
		if err := waitDeploymentRollout(ctx, runner, out, step.namespace, resource, applyRolloutWait); err != nil {
			return fmt.Errorf("gt town cluster rollback: %s was undone but did not come back ready, so the rollback stopped: %w", resource, err)
		}
	}
	return nil
}
