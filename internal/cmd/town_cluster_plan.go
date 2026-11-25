package cmd

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

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

// planRecordFile is one file of a plan record: its name and the hex SHA-256 of
// its bytes. A rendered Deployment also records the revision the cluster holds
// for it, as the annotation reads, and the object's resourceVersion, beside that
// hash. A Deployment the cluster does not hold is marked absent with neither.
type planRecordFile struct {
	Name            string `json:"name"`
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
applied. The plan reads the cluster's Deployments with get only and writes nothing
to any cluster.

Only --out is written: unlike a replay of the recorded flags, the plan does not
settle the town's running values file, which a live render writes. Last, the
plan writes plan.json into --out with the full id, each file's name and SHA-256,
and the revision the cluster holds for each Deployment; a Deployment the cluster
does not hold is recorded absent, with no revision. A plan that fails before that
leaves no plan.json.`,
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

// runTownClusterPlan renders the recorded flags' desired set with
// runClusterRender, reads each rendered Deployment's revision and resourceVersion
// through kube, writes plan.json into outDir with writeJSON, and prints the short
// plan id to out. The plan takes the file set, its apply order and its bytes from
// the render's own writes, so the id names exactly what the render wrote; the
// render's own output is dropped, so the id is all that is printed. plan.json is
// written only after every read succeeds, and the id only after it is written,
// so a refusal, a render error, a failed read or a failed write prints no id.
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
	d.findTown = func() (string, error) { return root, nil }
	// The town's running values file is the live render's to settle: a plan is a
	// review, so it leaves the file the pair-ready check reads as it was.
	d.writeTownValues = func(string, imagevalues.Values) error { return nil }
	d.out = io.Discard
	if err := runClusterRender(d, f); err != nil {
		return err
	}
	files := make([]planFile, len(names))
	record := planRecord{Files: make([]planRecordFile, len(names))}
	for i, name := range names {
		files[i] = planFile{Name: name, Data: bodies[i]}
		sum := sha256.Sum256(bodies[i])
		revision, resourceVersion, absent, err := planLiveVersions(ctx, kube, bodies[i])
		if err != nil {
			return err
		}
		record.Files[i] = planRecordFile{Name: name, SHA256: hex.EncodeToString(sum[:]), Revision: revision, ResourceVersion: resourceVersion, Absent: absent}
	}
	full, short := planID(files)
	record.ID = full
	if err := writeJSON(filepath.Join(outDir, planFileName), record); err != nil {
		return fmt.Errorf("gt town cluster plan: cannot write %s, so the plan is not whole and no id was printed: %w", filepath.Join(outDir, planFileName), err)
	}
	fmt.Fprintln(out, short)
	return nil
}
