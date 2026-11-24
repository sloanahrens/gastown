package cmd

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/deploy/k8s/sbx"
)

// sbxRenderInputs is what the gt-sbx render reads: the directory its files are
// written to, the pinned agent-sandbox manifest, the pulled chart directory, the
// values file and the helm binary ("" is "helm" on PATH).
type sbxRenderInputs struct {
	OutDir, Manifests, Chart, Values, Helm string
}

// runSbxRender composes the gt-sbx desired set with sbx.Render and writes it to
// in.OutDir, one numbered file per object in the set's apply order. A file is
// named as the control render names its own, so the two apply sets read alike.
// After the files it prints one expected Secret line per name in the set's
// Secrets, in their order, and never a value; nothing is applied and no cluster
// is read. Every refusal happens before helm runs and before the first write.
func runSbxRender(d clusterRenderDeps, in sbxRenderInputs) error {
	if err := checkSbxRenderInputs(in); err != nil {
		return err
	}
	set, err := d.renderSbx(context.Background(), sbx.Inputs{
		Read:             d.readFile,
		Run:              sbxHelmRunner(d.runHelm, in.Helm),
		AgentSandboxPath: in.Manifests,
		ChartDir:         in.Chart,
		ValuesPath:       in.Values,
	})
	if err != nil {
		return fmt.Errorf("cannot render the gt-sbx desired set: %w", err)
	}
	for i, object := range set.Objects {
		name := fmt.Sprintf("%02d-%s-%s.yaml", i+1, strings.ToLower(object.Kind), object.Name)
		path := filepath.Join(in.OutDir, name)
		if err := d.writeFile(path, object.Doc); err != nil {
			return fmt.Errorf("cannot write %s: %w", path, err)
		}
	}
	for _, name := range set.Secrets {
		fmt.Fprintf(d.out, "expected Secret: %s\n", name)
	}
	return nil
}

// checkSbxRenderInputs refuses an unnamed input and an out directory that is
// absent, unreadable or not empty. The render writes into a directory that
// exists and is empty and never creates one, so a refusal leaves the tree as it
// was.
func checkSbxRenderInputs(in sbxRenderInputs) error {
	for _, input := range []struct{ what, value, why string }{
		{"manifests", in.Manifests, "so the agent-sandbox manifest has no path to read"},
		{"chart", in.Chart, "so there is no chart to template"},
		{"values", in.Values, "so there are no values to render the chart with"},
	} {
		if input.value == "" {
			return fmt.Errorf("cannot render the gt-sbx desired set: no %s input, %s; no file was written", input.what, input.why)
		}
	}
	if in.OutDir == "" {
		return errors.New("cannot render the gt-sbx desired set: no out directory, so the set has nowhere to be written; no file was written")
	}
	entries, err := os.ReadDir(in.OutDir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return fmt.Errorf("cannot render the gt-sbx desired set: out directory %s does not exist, and the render creates none; no file was written", in.OutDir)
	case err != nil:
		return fmt.Errorf("cannot render the gt-sbx desired set: out directory %s cannot be read as a directory: %w; no file was written", in.OutDir, err)
	case len(entries) > 0:
		return fmt.Errorf("cannot render the gt-sbx desired set: out directory %s is not empty, and the render refuses to mix its files with another apply set; no file was written", in.OutDir)
	}
	return nil
}

// execHelm is the host's helm runner: it execs the binary named by in.Helm,
// with the arguments sbx.Render gives.
func execHelm(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output() //nolint:gosec // name is the helm binary the operator named
}

// sbxHelmRunner is the runner runSbxRender hands sbx.Render: sbx.Render names
// helm, and this runs the binary in.Helm instead, so the operator can point the
// render at a helm that is not the one on PATH.
func sbxHelmRunner(run func(ctx context.Context, name string, args ...string) ([]byte, error), helm string) sbx.Runner {
	if helm == "" {
		helm = "helm"
	}
	return func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		return run(ctx, helm, args...)
	}
}
