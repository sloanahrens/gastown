package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/deploy/k8s"
	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

// planFileName is the record a plan writes beside its files, last: the full id
// and each file's name and SHA-256 in apply order. Its presence means the plan is
// whole; a plan that failed before it was written leaves none.
const planFileName = "plan.json"

// planRevisionAnnotation is the annotation the Deployment controller stamps with a
// Deployment's revision number.
const planRevisionAnnotation = "deployment.kubernetes.io/revision"

// planRecord is what plan.json holds: the full plan id, and every file the id
// names, in the order the plan applies them.
type planRecord struct {
	ID    string           `json:"id"`
	Files []planRecordFile `json:"files"`
}

// planPhaseStatic and planPhaseWorkloads name a plan's two phases, in the order
// they apply: P holds the render's static control-cluster set, W the workloads.
const (
	planPhaseStatic    = "P"
	planPhaseWorkloads = "W"
)

// planRecordFile is one file of a plan record: its name, its phase (P or W), the
// hex SHA-256 of its bytes, and the output of its server-side dry run. A rendered
// Deployment also records the revision the cluster holds for it, as the annotation
// reads, and the object's resourceVersion, beside that hash. A Deployment the
// cluster does not hold is marked absent with neither. The dry-run output is
// recorded only for a run that passed: a failed dry run ends the plan before
// plan.json is written (gt-f1p2fw).
type planRecordFile struct {
	Name            string `json:"name"`
	Phase           string `json:"phase"`
	SHA256          string `json:"sha256"`
	Revision        string `json:"revision,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Absent          bool   `json:"absent,omitempty"`
	DryRun          string `json:"dry_run"`
}

// planFlags holds the plan's own flags: policyKubeconfig is phase P's (the
// deployer-policy certificate), applyKubeconfig phase W's (the deployer-apply one).
type planFlags struct {
	policyKubeconfig string
	applyKubeconfig  string
}

// phaseKubeconfig is the kubeconfig a phase reads: phase P's is
// --policy-kubeconfig and phase W's --apply-kubeconfig; no other phase exists.
// A phase whose flag is missing is refused naming the flag, since the command
// hard-codes no identity and reads no operator login or default kubeconfig. A
// phase with no files is never asked, so it needs none.
func phaseKubeconfig(phase string, f planFlags) (string, error) {
	flag, kubeconfig := "--policy-kubeconfig", f.policyKubeconfig
	switch phase {
	case planPhaseStatic:
	case planPhaseWorkloads:
		flag, kubeconfig = "--apply-kubeconfig", f.applyKubeconfig
	default:
		return "", fmt.Errorf("gt town cluster plan: %q is not a plan phase, so it has no kubeconfig", phase)
	}
	if kubeconfig == "" {
		return "", fmt.Errorf("gt town cluster plan: phase %s has files but no %s, and the plan takes no other cluster identity", phase, flag)
	}
	return kubeconfig, nil
}

// planDryRunResult is one file's server-side dry run and why it failed, if it did.
type planDryRunResult struct {
	Name   string
	Output string
	Err    error
}

// planDryRuns sends every file to the cluster as `kubectl apply --dry-run=server`
// under the kubeconfig phaseKubeconfig gives its phase, returning one result per
// file. Kubeconfigs that are not paths, and phases without one, refuse before any
// file is sent; a failed dry run is that file's result, not an error (gt-f1p2fw).
func planDryRuns(ctx context.Context, kube update.RunFunc, dir string, files []planRecordFile, kubeconfigs planFlags) ([]planDryRunResult, error) {
	byPhase := map[string]string{}
	for _, file := range files {
		if _, ok := byPhase[file.Phase]; !ok {
			kubeconfig, err := phaseKubeconfig(file.Phase, kubeconfigs)
			if err != nil {
				return nil, err
			}
			if err := checkClusterKubeconfig(kubeconfig); err != nil {
				return nil, err
			}
			byPhase[file.Phase] = kubeconfig
		}
	}
	results := make([]planDryRunResult, 0, len(files))
	for _, file := range files {
		out, exit, err := kube(ctx, "", nil, "kubectl", "--kubeconfig", byPhase[file.Phase], "apply", "--dry-run=server", "-f", filepath.Join(dir, file.Name))
		result := planDryRunResult{Name: file.Name, Output: out}
		switch {
		case err != nil:
			result.Err = fmt.Errorf("gt town cluster plan: the server-side dry run of %s could not run: %w", file.Name, err)
		case exit != 0:
			result.Err = fmt.Errorf("gt town cluster plan: the server-side dry run of %s failed: kubectl exited %d", file.Name, exit)
		}
		results = append(results, result)
	}
	return results, nil
}

// planRecordedFlags is the render flags a plan replays: the flags the last live
// render recorded under the town, with the static control-cluster set included
// and the plan's own --out directory in place of the recorded one. It returns
// the town root beside them, so the caller reads and writes under the same town.
//
// Each refusal returns no flags: a plan with no record, or with a record it
// cannot read, writes nothing rather than a plan built on guessed defaults.
// It is unexported and called by the plan command in the next bead of this
// chain (gt-1bj7zf); nothing else calls it yet.
func planRecordedFlags(findTown func() (string, error), out string) (clusterRenderFlags, string, error) {
	root, err := findTown()
	if err != nil {
		return clusterRenderFlags{}, "", fmt.Errorf("gt town cluster plan: town not found: %w", err)
	}
	path := renderFlagsPathIn(root)
	rec, ok, err := loadRenderFlags(path)
	if err != nil {
		return clusterRenderFlags{}, "", fmt.Errorf("gt town cluster plan: %w", err)
	}
	if !ok {
		return clusterRenderFlags{}, "", fmt.Errorf("gt town cluster plan: no render has recorded its flags at %s; run a live gt town cluster render with --record-flags first, and no plan was written", path)
	}
	rec.includeStatic = true
	rec.out = out
	return rec, root, nil
}

// townClusterPlanCmd renders the desired set into --out and prints its plan id,
// so a shape change can be reviewed before it is applied.
var townClusterPlanCmd = &cobra.Command{
	Use:   "plan",
	Short: "Render the desired set into a directory and print its plan id",
	Long: `Render the desired set into --out from the flags the last live render recorded,
with --include-static, and print the short form of the plan's id. Nothing is
applied. The plan reads the cluster's Deployments and the static objects with
get only and writes nothing to any cluster, and sends each file as a server-side
dry run. A Secret is compared on the fields its document declares and never on
its data, which the cluster fills. After the phases, the plan prints, in the
drift command's form, the compare's lines for the Deployments and static objects
that differ from the live cluster.

Only --out is written: unlike a replay of the recorded flags, the plan does not
settle the town's running values file, which a live render writes. Last, the
plan writes plan.json into --out with the full id, each file's name, phase,
SHA-256 and server-side dry-run output, and the revision the cluster holds for
each Deployment; a Deployment the cluster does not hold is recorded absent, with
no revision. A dry run that fails for any file ends the plan non-zero, and no
plan.json is written. A plan holding a Secret document deploy/k8s does not review
is refused the same way, since Secrets stay operator-created. A phase P file is
listed only when a static object differs from the cluster's, and then the id names
it too. A plan that fails before that leaves no plan.json.

Each phase takes its kubeconfig from its own flag: phase P --policy-kubeconfig,
the deployer-policy certificate, and phase W --apply-kubeconfig, the
deployer-apply certificate; the plan hard-codes no phase identity. A phase with
files whose flag is missing is refused, naming the flag. The command does not
run inside the daemon pod.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTownClusterPlan(cmd.Context(), realClusterRenderDeps(io.Discard), realRunFunc(), cmd.OutOrStdout(), clusterPlanOut, clusterPlanKubeconfigs, atomicfile.WriteJSON)
	},
}

var (
	clusterPlanOut         string
	clusterPlanKubeconfigs planFlags
)

func init() {
	townClusterPlanCmd.Flags().StringVar(&clusterPlanOut, "out", "",
		"the directory the rendered files are written to; it must be empty or absent (required)")
	townClusterPlanCmd.Flags().StringVar(&clusterPlanKubeconfigs.policyKubeconfig, "policy-kubeconfig", "",
		"phase P's kubeconfig, the deployer-policy certificate's: required when phase P has files")
	townClusterPlanCmd.Flags().StringVar(&clusterPlanKubeconfigs.applyKubeconfig, "apply-kubeconfig", "",
		"phase W's kubeconfig, the deployer-apply certificate's: required when phase W has files")
	townClusterCmd.AddCommand(townClusterPlanCmd)
}

// planRevisionIsNumber reports whether a revision annotation is a positive whole
// number. The controller stamps 1, 2, 3 and so on; zero, a sign, a fraction or any
// other text is not a revision the plan can trust.
func planRevisionIsNumber(revision string) bool {
	n, err := strconv.ParseUint(revision, 10, 64)
	return err == nil && n > 0
}

// planLiveVersions reads the Deployment a rendered file names with one get
// through the drift command's read: the revision and resourceVersion the cluster
// holds for it, as the annotation and the metadata read, beside the drift
// compare's lines (objectDrift) for the rendered fields that differ, each
// prefixed by the resource. A Deployment the cluster does not hold is a normal
// plan: absent, with no revision and no lines. A failed get, a held Deployment
// with no revision annotation, a revision that is not a positive whole number, no
// resourceVersion, or an answer that is no object at all, is an error: an empty
// or unreadable value would let a later apply pass as unchanged.
func planLiveVersions(ctx context.Context, kube update.RunFunc, body []byte) (revision, resourceVersion string, absent bool, lines []string, err error) {
	// The file's own fields are the rendered side of the compare, decoded as the
	// drift compare decodes a rendered object, so the lines name the fields the
	// apply would change.
	var rendered map[string]any
	if err := yaml.Unmarshal(body, &rendered); err != nil {
		return "", "", false, nil, fmt.Errorf("gt town cluster plan: cannot read a rendered file's kind, so no plan was written: %w", err)
	}
	kind, ok := rendered["kind"].(string)
	if !ok {
		return "", "", false, nil, fmt.Errorf("gt town cluster plan: a rendered file has no kind to read, so no plan was written")
	}
	if kind != "Deployment" {
		return "", "", false, nil, nil
	}
	metadata, _ := rendered["metadata"].(map[string]any)
	name, _ := metadata["name"].(string)
	namespace, _ := metadata["namespace"].(string)
	resource := "deployment/" + name
	live, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: kube}, resource, namespace, false)
	if err != nil {
		return "", "", false, nil, fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	if absent {
		return "", "", true, nil, nil
	}
	diff, err := objectDrift(kind, rendered, live)
	if err != nil {
		return "", "", false, nil, fmt.Errorf("gt town cluster plan: cannot compare %s with its live object, so no plan was written: %w", resource, err)
	}
	liveMetadata, _ := live["metadata"].(map[string]any)
	annotations, _ := liveMetadata["annotations"].(map[string]any)
	revision, _ = annotations[planRevisionAnnotation].(string)
	if revision == "" {
		return "", "", false, nil, fmt.Errorf("gt town cluster plan: %s in %s has no revision annotation, so no plan was written", resource, namespace)
	}
	if !planRevisionIsNumber(revision) {
		return "", "", false, nil, fmt.Errorf("gt town cluster plan: %s in %s has revision %q, which is not a positive whole number, so no plan was written", resource, namespace, revision)
	}
	resourceVersion, _ = liveMetadata["resourceVersion"].(string)
	if resourceVersion == "" {
		return "", "", false, nil, fmt.Errorf("gt town cluster plan: %s in %s has no resourceVersion, so no plan was written", resource, namespace)
	}
	lines = make([]string, len(diff))
	for i, line := range diff {
		lines[i] = planDiffLine(resource, line)
	}
	return revision, resourceVersion, false, lines, nil
}

// planDiffLine is one line the plan prints after the phases: the resource the
// drift compare read, then the compare's line as `gt town cluster drift` prints
// it, value or fingerprint as that command decides.
func planDiffLine(resource, line string) string {
	return resource + ": " + line
}

// planStaticTargets is the drift target of every static object of the render, in
// the render's order, each built by planStaticTarget. A target that cannot be
// built refuses the plan. A resource and namespace the set holds twice also
// refuses it: the plan would read one object twice and compare the second copy
// against nothing that could differ.
func planStaticTargets(objects []k8s.StaticObject) ([]driftTarget, error) {
	targets := make([]driftTarget, 0, len(objects))
	seen := make(map[string]bool, len(objects))
	for _, object := range objects {
		target, err := planStaticTarget(object)
		if err != nil {
			return nil, err
		}
		key := target.resource + " " + target.namespace
		if seen[key] {
			return nil, fmt.Errorf("the static %s is compared twice, so the set holds it twice", staticObjectLabel(object))
		}
		seen[key] = true
		targets = append(targets, target)
	}
	return targets, nil
}

// planStaticTarget is the drift target of one static object: a Secret's declared
// fields, read by planSecretTarget, and any other kind's by the drift walk's own
// builder, so no rule is copied here.
func planStaticTarget(object k8s.StaticObject) (driftTarget, error) {
	if object.Kind == secretKind {
		return planSecretTarget(object)
	}
	return staticObjectTarget(object)
}

// planSecretTarget is the drift target of a static Secret: its document without
// its data and stringData, which the cluster owns and fills, so a Secret is
// compared on the fields its document declares and never on its data. A Secret
// that carries data refuses the plan: the plan compares declared fields only, so
// it would not compare what the cluster fills. The document is validated by
// staticObjectDoc, so one that names another object refuses the plan. A Secret
// the cluster does not hold is drift.
func planSecretTarget(object k8s.StaticObject) (driftTarget, error) {
	if !object.SecretNoData {
		return driftTarget{}, fmt.Errorf("the static %s carries data and was not read, so the plan would not compare what the cluster fills", staticObjectLabel(object))
	}
	doc, err := staticObjectDoc(object)
	if err != nil {
		return driftTarget{}, err
	}
	declared := make(map[string]any, len(doc.body))
	for key, value := range doc.body {
		switch key {
		case "data", "stringData":
			continue
		}
		declared[key] = value
	}
	target := namespacedDriftTarget(secretKind, object.Name, object.Namespace, declared)
	target.absentIsDrift = true
	return target, nil
}

// planStaticDifferences is the drift compare's lines for the static objects, in
// the render's order, each prefixed by the resource as the drift command prints
// it. Every target is read even after one differs, so a failed read refuses the
// plan. runTownClusterPlan lists phase P only when it returns at least one line.
func planStaticDifferences(ctx context.Context, encode func(io.Writer, any) error, kube update.RunFunc, targets []driftTarget) ([]string, error) {
	d := clusterDriftDeps{render: clusterRenderDeps{encode: encode}, kubectl: kube}
	var lines []string
	for _, target := range targets {
		diff, err := target.differences(ctx, d)
		if err != nil {
			return nil, err
		}
		for _, line := range diff {
			lines = append(lines, planDiffLine(target.resource, line))
		}
	}
	return lines, nil
}

// runTownClusterPlan renders the recorded flags' desired set with
// runClusterRender, reads each rendered Deployment and each static object through
// kube, writes plan.json into outDir with writeJSON, and prints the phases, the
// drift compare's differences and the short plan id to out. The plan takes the file set, its apply order and its bytes
// from the render's own writes, so the id names exactly what the render wrote; the
// render's own output is dropped. Phase P is listed only when a static object
// differs from the cluster's: when none does, plan.json and the id name the
// workloads alone, and the phases line names only W. A phase with files whose
// kubeconfig flag is missing (phaseKubeconfig) is refused. Each file is then sent
// as a server-side dry run under its phase's kubeconfig, its output recorded on
// the file in plan.json; a dry run that fails for any file ends the plan non-zero
// and writes no plan.json (gt-f1p2fw). A plan whose rendered static set holds a
// Secret document deploy/k8s does not review is refused before the static objects
// are read, since Secrets stay operator-created (gt-8bck5o). The differences
// print after the phases, the static objects' then each Deployment's in apply
// order, and the short id is the last line. plan.json is written only after every
// read succeeds and every dry run passed, and the output only after it is
// written, so a refusal, a render error, a failed read, a failed dry run or a
// failed write prints nothing.
func runTownClusterPlan(ctx context.Context, d clusterRenderDeps, kube update.RunFunc, out io.Writer, outDir string, kubeconfigs planFlags, writeJSON func(path string, v any) error) error {
	f, root, err := planRecordedFlags(d.findTown, outDir)
	if err != nil {
		return err
	}
	var names []string
	var bodies [][]byte
	write := d.writeFile
	d.writeFile = func(path string, data []byte) error {
		if err := write(path, data); err != nil {
			return err
		}
		names = append(names, filepath.Base(path))
		bodies = append(bodies, data)
		return nil
	}
	// The static documents are the render's static set, which the render writes as
	// their reviewed bytes, so a file is phase P exactly when its bytes are one of them.
	staticDocs := map[string]bool{}
	var staticObjects []k8s.StaticObject
	build := d.build
	d.build = func(v k8sdaemon.Values) (k8sdaemon.Manifests, error) {
		m, err := build(v)
		if err != nil {
			return m, err
		}
		staticObjects = m.Static
		for _, object := range m.Static {
			staticDocs[string(object.Doc)] = true
		}
		return m, nil
	}
	d.findTown = func() (string, error) { return root, nil }
	// The town's running values file is the live render's to settle: a plan is a
	// review, so it leaves the file the pair-ready check reads as it was.
	d.writeTownValues = func(string, imagevalues.Values) error { return nil }
	d.out = io.Discard
	if err := runClusterRender(d, f); err != nil {
		return err
	}
	// Every static object builds its target through the drift walk's builder, so a
	// static document that names another object refuses the plan here, before
	// plan.json is written. Each target is then read, and a read that fails refuses
	// the plan the same way; the lines a target returns are the difference that
	// decides whether phase P is listed.
	targets, err := planStaticTargets(staticObjects)
	if err != nil {
		return fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	// The plan may hold only the static Secret documents deploy/k8s reviews, and
	// it refuses before the static objects are read, so a read's cause cannot
	// stand in for the guard's. Secrets stay operator-created.
	unreviewed, err := planUnreviewedStaticSecret(staticObjects, d.reviewedStatic)
	if err != nil {
		return fmt.Errorf("gt town cluster plan: %w, so no plan.json was written", err)
	}
	if unreviewed != "" {
		return fmt.Errorf("gt town cluster plan: the plan holds %s, which deploy/k8s does not review, and Secrets stay operator-created, so no plan.json was written", unreviewed)
	}
	differences, err := planStaticDifferences(ctx, d.encode, kube, targets)
	if err != nil {
		return fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	differs := len(differences) > 0
	files := make([]planFile, 0, len(names))
	record := planRecord{Files: make([]planRecordFile, 0, len(names))}
	counts := map[string]int{}
	for i, name := range names {
		phase := planPhaseWorkloads
		if staticDocs[string(bodies[i])] {
			phase = planPhaseStatic
		}
		// With no static object differing, the static documents change nothing, so
		// phase P is dropped from plan.json and from the id together.
		if phase == planPhaseStatic && !differs {
			continue
		}
		files = append(files, planFile{Name: name, Data: bodies[i]})
		sum := sha256.Sum256(bodies[i])
		revision, resourceVersion, absent, lines, err := planLiveVersions(ctx, kube, bodies[i])
		if err != nil {
			return err
		}
		differences = append(differences, lines...)
		counts[phase]++
		record.Files = append(record.Files, planRecordFile{Name: name, Phase: phase, SHA256: hex.EncodeToString(sum[:]), Revision: revision, ResourceVersion: resourceVersion, Absent: absent})
	}
	full, short := planID(files)
	// A phase with files but no kubeconfig flag is refused: no identity is left to fall back on.
	for _, phase := range []string{planPhaseStatic, planPhaseWorkloads} {
		if counts[phase] == 0 {
			continue
		}
		if _, err := phaseKubeconfig(phase, kubeconfigs); err != nil {
			return err
		}
	}
	record.ID = full
	// Each file's server-side dry run runs before plan.json is written, and a run
	// that fails for any file ends the plan here: plan.json is written last, so its
	// presence means every file it names passed its dry run (gt-f1p2fw).
	results, err := planDryRuns(ctx, kube, outDir, record.Files, kubeconfigs)
	if err != nil {
		return err
	}
	for i := range results {
		if results[i].Err != nil {
			return fmt.Errorf("%w, so no plan.json was written", results[i].Err)
		}
		// planDryRuns returns one result per file in files order, so result i is the
		// dry run of record.Files[i].
		record.Files[i].DryRun = results[i].Output
	}
	if err := writeJSON(filepath.Join(outDir, planFileName), record); err != nil {
		return fmt.Errorf("gt town cluster plan: cannot write %s, so the plan is not whole and no id was printed: %w", filepath.Join(outDir, planFileName), err)
	}
	// The phases line names only the phases the plan holds, each with its file count.
	var held []string
	if counts[planPhaseStatic] > 0 {
		held = append(held, fmt.Sprintf("%s (static, %d files)", planPhaseStatic, counts[planPhaseStatic]))
	}
	if counts[planPhaseWorkloads] > 0 {
		held = append(held, fmt.Sprintf("%s (workloads, %d files)", planPhaseWorkloads, counts[planPhaseWorkloads]))
	}
	fmt.Fprintf(out, "Phases in apply order: %s\n", strings.Join(held, ", then "))
	// The differences follow the phases, and the short id stays the last line.
	for _, line := range differences {
		fmt.Fprintln(out, line)
	}
	fmt.Fprintln(out, short)
	return nil
}
