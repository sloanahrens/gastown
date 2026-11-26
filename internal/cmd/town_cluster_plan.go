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

// planRecordFile is one file of a plan record: its name, its phase (P or W) and
// the hex SHA-256 of its bytes. A rendered Deployment also records the revision
// the cluster holds for it, as the annotation reads, and the object's
// resourceVersion, beside that hash. A Deployment the cluster does not hold is
// marked absent with neither.
type planRecordFile struct {
	Name            string `json:"name"`
	Phase           string `json:"phase"`
	SHA256          string `json:"sha256"`
	Revision        string `json:"revision,omitempty"`
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Absent          bool   `json:"absent,omitempty"`
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
applied. The plan reads the cluster's Deployments and the static objects with get
only and writes nothing to any cluster. A Secret is compared on the fields its
document declares and never on its data, which the cluster fills.

Only --out is written: unlike a replay of the recorded flags, the plan does not
settle the town's running values file, which a live render writes. Last, the
plan writes plan.json into --out with the full id, each file's name, phase and
SHA-256, and the revision the cluster holds for each Deployment; a Deployment the
cluster does not hold is recorded absent, with no revision. A phase P file is
listed only when a static object differs from the cluster's, and then the id
names it too. A plan that fails before that leaves no plan.json.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTownClusterPlan(cmd.Context(), realClusterRenderDeps(io.Discard), realRunFunc(), cmd.OutOrStdout(), clusterPlanOut, atomicfile.WriteJSON)
	},
}

var clusterPlanOut string

func init() {
	townClusterPlanCmd.Flags().StringVar(&clusterPlanOut, "out", "",
		"the directory the rendered files are written to; it must be empty or absent (required)")
	townClusterCmd.AddCommand(townClusterPlanCmd)
}

// planDocument is the part of a rendered file the plan reads back: the kind and
// the name and namespace a Deployment is read by.
type planDocument struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name      string `yaml:"name"`
		Namespace string `yaml:"namespace"`
	} `yaml:"metadata"`
}

// planRevisionIsNumber reports whether a revision annotation is a positive whole
// number. The controller stamps 1, 2, 3 and so on; zero, a sign, a fraction or any
// other text is not a revision the plan can trust.
func planRevisionIsNumber(revision string) bool {
	n, err := strconv.ParseUint(revision, 10, 64)
	return err == nil && n > 0
}

// planLiveVersions returns the revision and the resourceVersion the cluster holds
// for the Deployment a rendered file names, read with one get through the drift
// command's read, as the annotation and the metadata read. A file that is not a
// Deployment reads nothing. A Deployment the cluster does not hold is a normal
// plan: it returns absent with no revision and no resourceVersion. A Deployment
// that is held with no revision annotation, a revision that is not a positive
// whole number, or no resourceVersion, or a get that failed, is an error: an empty
// or unreadable value would let a later apply pass as unchanged.
func planLiveVersions(ctx context.Context, kube update.RunFunc, body []byte) (string, string, bool, error) {
	var doc planDocument
	if err := yaml.Unmarshal(body, &doc); err != nil {
		return "", "", false, fmt.Errorf("gt town cluster plan: cannot read a rendered file's kind, so no plan was written: %w", err)
	}
	if doc.Kind != "Deployment" {
		return "", "", false, nil
	}
	resource := "deployment/" + doc.Metadata.Name
	live, absent, err := readLiveObject(ctx, clusterDriftDeps{kubectl: kube}, resource, doc.Metadata.Namespace, false)
	if err != nil {
		return "", "", false, fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	if absent {
		return "", "", true, nil
	}
	metadata, _ := live["metadata"].(map[string]any)
	annotations, _ := metadata["annotations"].(map[string]any)
	revision, _ := annotations[planRevisionAnnotation].(string)
	if revision == "" {
		return "", "", false, fmt.Errorf("gt town cluster plan: %s in %s has no revision annotation, so no plan was written", resource, doc.Metadata.Namespace)
	}
	if !planRevisionIsNumber(revision) {
		return "", "", false, fmt.Errorf("gt town cluster plan: %s in %s has revision %q, which is not a positive whole number, so no plan was written", resource, doc.Metadata.Namespace, revision)
	}
	resourceVersion, _ := metadata["resourceVersion"].(string)
	if resourceVersion == "" {
		return "", "", false, fmt.Errorf("gt town cluster plan: %s in %s has no resourceVersion, so no plan was written", resource, doc.Metadata.Namespace)
	}
	return revision, resourceVersion, false, nil
}

// planStaticTargets is the drift target of every static object of the render, in
// the render's order, each built by planStaticTarget. A target that cannot be
// built refuses the plan.
func planStaticTargets(objects []k8s.StaticObject) ([]driftTarget, error) {
	targets := make([]driftTarget, 0, len(objects))
	for _, object := range objects {
		target, err := planStaticTarget(object)
		if err != nil {
			return nil, err
		}
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

// planStaticDiffers reports whether any target differs from the cluster's live
// object, compared by driftTarget.differences. Every target is read even after one
// differs, so a failed read refuses the plan. runTownClusterPlan lists phase P only
// when this reports true.
func planStaticDiffers(ctx context.Context, encode func(io.Writer, any) error, kube update.RunFunc, targets []driftTarget) (bool, error) {
	d := clusterDriftDeps{render: clusterRenderDeps{encode: encode}, kubectl: kube}
	differs := false
	for _, target := range targets {
		lines, err := target.differences(ctx, d)
		if err != nil {
			return false, err
		}
		differs = differs || len(lines) > 0
	}
	return differs, nil
}

// runTownClusterPlan renders the recorded flags' desired set with
// runClusterRender, reads each rendered Deployment's revision and resourceVersion
// through kube, writes plan.json into outDir with writeJSON, and prints the phases
// and the short plan id to out. The plan takes the file set, its apply order and
// its bytes from the render's own writes, so the id names exactly what the render
// wrote; the render's own output is dropped, so only the phases and the id are
// printed. Phase P is listed only when a static object differs from the cluster's:
// when none does, plan.json and the id name the workloads alone, and the phases
// line names only W. plan.json is written only after every read succeeds, and the
// output only after it is written, so a refusal, a render error, a failed read or
// a failed write prints nothing.
func runTownClusterPlan(ctx context.Context, d clusterRenderDeps, kube update.RunFunc, out io.Writer, outDir string, writeJSON func(path string, v any) error) error {
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
	// the plan the same way. Whether any target differs decides whether phase P is
	// listed.
	targets, err := planStaticTargets(staticObjects)
	if err != nil {
		return fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
	differs, err := planStaticDiffers(ctx, d.encode, kube, targets)
	if err != nil {
		return fmt.Errorf("gt town cluster plan: %w, so no plan was written", err)
	}
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
		revision, resourceVersion, absent, err := planLiveVersions(ctx, kube, bodies[i])
		if err != nil {
			return err
		}
		counts[phase]++
		record.Files = append(record.Files, planRecordFile{Name: name, Phase: phase, SHA256: hex.EncodeToString(sum[:]), Revision: revision, ResourceVersion: resourceVersion, Absent: absent})
	}
	full, short := planID(files)
	record.ID = full
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
	fmt.Fprintln(out, short)
	return nil
}
