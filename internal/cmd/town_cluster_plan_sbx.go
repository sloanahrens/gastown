package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/deploy/k8s"
	"github.com/steveyegge/gastown/internal/clusterapproval"
	"github.com/steveyegge/gastown/internal/update"
)

// planClusterFor names the cluster a plan is for, empty reading as control. An
// unknown name, and the sbx inputs given to control, refuse.
func planClusterFor(f planFlags) (string, error) {
	cluster := f.cluster
	if cluster == "" {
		cluster = clusterControl
	}
	if !clusterapproval.ValidCluster(cluster) {
		return "", fmt.Errorf("gt town cluster plan: --cluster %q is not a cluster the plan knows: want %s or %s; no plan was written", f.cluster, clusterControl, clusterSbx)
	}
	if cluster == clusterControl && f.sbx != (planSbxInputs{}) {
		return "", fmt.Errorf("gt town cluster plan: the --sbx-* and --helm flags are for --cluster %s only; no plan was written", clusterSbx)
	}
	return cluster, nil
}

// runSbxPlan writes the gt-sbx set into outDir and plans it as the control set is
// planned: cluster-scoped objects in phase P and namespaced ones in phase W, each
// read under its phase's kubeconfig, with plan.json written last.
func runSbxPlan(ctx context.Context, d clusterRenderDeps, kube update.RunFunc, out io.Writer, outDir string, f planFlags, writeJSON func(path string, v any) error) error {
	names, bodies, phases, targets, err := planSbxSet(d, f.sbx, outDir)
	if err != nil {
		return err
	}
	runnerFor := func(phase string) (update.RunFunc, error) { return planSbxRunner(kube, phase, f) }
	differences, differs, err := planSbxDifferences(ctx, d.encode, runnerFor, phases, targets)
	if err != nil {
		return fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	record := planRecord{Cluster: clusterSbx}
	var listed []planFile
	var kept [][]byte
	counts := map[string]int{}
	var deployments, comparable, rolls int
	for i, phase := range phases {
		if phase == planPhaseStatic && !differs {
			continue
		}
		listed = append(listed, planFile{Name: names[i], Data: bodies[i]})
		kept = append(kept, bodies[i])
		sum := sha256.Sum256(bodies[i])
		readKube, err := runnerFor(phase)
		if err != nil {
			return err
		}
		read, err := planLiveVersions(ctx, readKube, bodies[i])
		if err != nil {
			return err
		}
		differences = append(differences, read.Lines...)
		counts[phase]++
		record.Files = append(record.Files, planRecordFile{Name: names[i], Phase: phase, SHA256: hex.EncodeToString(sum[:]), Revision: read.Revision, ResourceVersion: read.ResourceVersion, Absent: read.Absent})
		if phase != planPhaseWorkloads || read.Kind != "Deployment" {
			continue
		}
		deployments++
		if read.Absent || len(read.Lines) > 0 {
			continue
		}
		comparable++
		roll, err := planImageRollDiffers(read.Rendered, read.Live)
		if err != nil {
			return fmt.Errorf("gt town cluster plan: cannot compare the images %s names with the cluster's, so no plan.json was written: %w", names[i], err)
		}
		if roll {
			rolls++
		}
	}
	if !differs && rolls > 0 && comparable == deployments {
		return planImageRollRefusal()
	}
	full, short := planIDFor(clusterSbx, listed)
	for _, phase := range []string{planPhaseStatic, planPhaseWorkloads} {
		if counts[phase] == 0 {
			continue
		}
		if _, err := phaseKubeconfig(phase, f); err != nil {
			return err
		}
	}
	record.ID = full
	mounted, err := planMountedSecrets(kept)
	if err != nil {
		return fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	if err := planSecretPreflight(ctx, kube, f.applyKubeconfig, mounted); err != nil {
		return fmt.Errorf("gt town cluster plan: %w", err)
	}
	results, err := planDryRuns(ctx, kube, outDir, record.Files, f)
	if err != nil {
		return err
	}
	for i := range results {
		if results[i].Err != nil {
			return fmt.Errorf("%w, so no plan.json was written", results[i].Err)
		}
		record.Files[i].DryRun = results[i].Output
	}
	if err := writeJSON(filepath.Join(outDir, planFileName), record); err != nil {
		return fmt.Errorf("gt town cluster plan: cannot write %s, so the plan is not whole and no id was printed: %w", filepath.Join(outDir, planFileName), err)
	}
	var held []string
	if counts[planPhaseStatic] > 0 {
		held = append(held, fmt.Sprintf("%s (static, %d files)", planPhaseStatic, counts[planPhaseStatic]))
	}
	if counts[planPhaseWorkloads] > 0 {
		held = append(held, fmt.Sprintf("%s (workloads, %d files)", planPhaseWorkloads, counts[planPhaseWorkloads]))
	}
	fmt.Fprintf(out, "Phases in apply order: %s\n", strings.Join(held, ", then "))
	for _, line := range differences {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, short)
	return nil
}

// planSbxSet renders the gt-sbx set into outDir, which must exist and be empty,
// and returns each file with its phase and each object's drift target. A Secret
// document refuses the plan, so no plan.json is written; objects before it may
// already be in outDir, and a rerun needs an empty directory again.
func planSbxSet(d clusterRenderDeps, in planSbxInputs, outDir string) ([]string, [][]byte, []string, []driftTarget, error) {
	rin := sbxRenderInputs{OutDir: outDir, Manifests: in.manifests, Chart: in.chart, Values: in.values, Helm: in.helm}
	if err := checkSbxRenderInputs(rin); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("gt town cluster plan: %w", err)
	}
	set, err := renderSbxSet(d, rin)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	names := make([]string, 0, len(set.Objects))
	bodies := make([][]byte, 0, len(set.Objects))
	phases := make([]string, 0, len(set.Objects))
	targets := make([]driftTarget, 0, len(set.Objects))
	seen := make(map[string]bool, len(set.Objects))
	for i, object := range set.Objects {
		if object.Kind == secretKind {
			return nil, nil, nil, nil, fmt.Errorf("gt town cluster plan: the gt-sbx set holds the %s, and Secrets stay operator-created, so no plan.json was written", staticObjectLabel(object))
		}
		phase := planPhaseWorkloads
		if object.Cluster {
			phase = planPhaseStatic
		}
		target, err := planSbxObjectTarget(object)
		if err != nil {
			return nil, nil, nil, nil, fmt.Errorf("gt town cluster plan: %w, so no plan.json was written", err)
		}
		key := target.resource + " " + target.namespace
		if seen[key] {
			return nil, nil, nil, nil, fmt.Errorf("gt town cluster plan: the static %s is compared twice, so no plan.json was written", staticObjectLabel(object))
		}
		seen[key] = true
		name := sbxObjectFileName(i, object)
		if err := d.writeFile(filepath.Join(outDir, name), object.Doc); err != nil {
			return nil, nil, nil, nil, fmt.Errorf("gt town cluster plan: cannot write %s, so no plan.json was written: %w", name, err)
		}
		names = append(names, name)
		bodies = append(bodies, object.Doc)
		phases = append(phases, phase)
		targets = append(targets, target)
	}
	return names, bodies, phases, targets, nil
}

// planSbxObjectTarget is one gt-sbx object's drift target: a cluster-scoped object
// as a static object's, a namespaced one by its whole document, since the static
// kind list holds no Service or claim.
func planSbxObjectTarget(object k8s.StaticObject) (driftTarget, error) {
	if object.Cluster {
		return planStaticTarget(object)
	}
	if object.Namespace == "" {
		return driftTarget{}, fmt.Errorf("the namespaced %s names no namespace, so it was not read", staticObjectLabel(object))
	}
	doc, err := staticObjectDoc(object)
	if err != nil {
		return driftTarget{}, err
	}
	target := namespacedDriftTarget(object.Kind, object.Name, object.Namespace, doc.body)
	target.absentIsDrift = true
	return target, nil
}

// planSbxDifferences is the compare's lines for the objects, each read through its
// phase's runner, and whether a phase P object differs.
func planSbxDifferences(ctx context.Context, encode func(io.Writer, any) error, runnerFor func(string) (update.RunFunc, error), phases []string, targets []driftTarget) ([]string, bool, error) {
	var lines []string
	var differs bool
	for i, target := range targets {
		kube, err := runnerFor(phases[i])
		if err != nil {
			return nil, false, err
		}
		diff, err := target.differences(ctx, clusterDriftDeps{render: clusterRenderDeps{encode: encode}, kubectl: kube})
		if err != nil {
			return nil, false, err
		}
		for _, line := range diff {
			lines = append(lines, planDiffLine(target.resource, line))
			differs = differs || phases[i] == planPhaseStatic
		}
	}
	return lines, differs, nil
}

// planSbxRunner binds a phase's reads to that phase's kubeconfig, so a sandbox
// object is never read through the operator's default cluster.
func planSbxRunner(kube update.RunFunc, phase string, f planFlags) (update.RunFunc, error) {
	kubeconfig, err := phaseKubeconfig(phase, f)
	if err != nil {
		return nil, err
	}
	if err := checkClusterKubeconfig(kubeconfig); err != nil {
		return nil, err
	}
	return applyKubeconfigRunner(kube, kubeconfig), nil
}
