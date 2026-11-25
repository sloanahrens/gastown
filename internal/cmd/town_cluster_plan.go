package cmd

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/imagevalues"
)

// planFileName is the record a plan writes beside its files, last: the full id
// and each file's name and SHA-256 in apply order. Its presence means the plan is
// whole; a plan that failed before it was written leaves none.
const planFileName = "plan.json"

// planRecord is what plan.json holds: the full plan id, and every file the id
// names, in the order the plan applies them.
type planRecord struct {
	ID    string           `json:"id"`
	Files []planRecordFile `json:"files"`
}

// planRecordFile is one file of a plan record: its name and the hex SHA-256 of
// its bytes.
type planRecordFile struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
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
applied and no cluster is contacted.

Only --out is written: unlike a replay of the recorded flags, the plan does not
settle the town's running values file, which a live render writes. Last, the
plan writes plan.json into --out with the full id and each file's name and
SHA-256; a plan that fails before that leaves no plan.json.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runTownClusterPlan(realClusterRenderDeps(io.Discard), cmd.OutOrStdout(), clusterPlanOut, atomicfile.WriteJSON)
	},
}

var clusterPlanOut string

func init() {
	townClusterPlanCmd.Flags().StringVar(&clusterPlanOut, "out", "",
		"the directory the rendered files are written to; it must be empty or absent (required)")
	townClusterCmd.AddCommand(townClusterPlanCmd)
}

// runTownClusterPlan renders the recorded flags' desired set with
// runClusterRender, writes plan.json into outDir with writeJSON, and prints the
// short plan id to out. The plan takes the file set, its apply order and its
// bytes from the render's own writes, so the id names exactly what the render
// wrote; the render's own output is dropped, so the id is all that is printed.
// plan.json is written only after the render succeeds, and the id is printed only
// after it is written, so a refusal, a render error or a failed write prints no id.
func runTownClusterPlan(d clusterRenderDeps, out io.Writer, outDir string, writeJSON func(path string, v any) error) error {
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
		record.Files[i] = planRecordFile{Name: name, SHA256: hex.EncodeToString(sum[:])}
	}
	full, short := planID(files)
	record.ID = full
	if err := writeJSON(filepath.Join(outDir, planFileName), record); err != nil {
		return fmt.Errorf("gt town cluster plan: cannot write %s, so the plan is not whole and no id was printed: %w", filepath.Join(outDir, planFileName), err)
	}
	fmt.Fprintln(out, short)
	return nil
}
