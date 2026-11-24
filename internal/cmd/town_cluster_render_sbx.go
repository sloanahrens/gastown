package cmd

import (
	"context"
	"fmt"
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
// named as the control render names its own, so the two apply sets read alike;
// nothing is applied and no cluster is read.
func runSbxRender(d clusterRenderDeps, in sbxRenderInputs) error {
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
