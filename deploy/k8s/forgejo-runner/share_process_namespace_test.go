package forgejorunner

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestRunnerDeploymentSharesProcessNamespace holds the rendered runner pod, and the
// golden an operator applies, to a shared process namespace: the pod's pause
// container is then PID 1 and reaps the children a job leaves behind, instead of
// the zombies accumulating under forgejo-runner (gt-p97r7p).
func TestRunnerDeploymentSharesProcessNamespace(t *testing.T) {
	t.Parallel()
	pod := mustBuild(t, goldenValues()).Deployment.Spec.Template.Spec
	if pod.ShareProcessNamespace == nil {
		t.Error("the runner pod sets no shareProcessNamespace, want true")
	} else if !*pod.ShareProcessNamespace {
		t.Errorf("the runner pod sets shareProcessNamespace to %v, want true", *pod.ShareProcessNamespace)
	}

	path := filepath.Join("testdata", "deployment.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var golden Deployment
	if err := yaml.Unmarshal(raw, &golden); err != nil {
		t.Fatalf("decoding %s: %v", path, err)
	}
	if got := golden.Spec.Template.Spec.ShareProcessNamespace; got == nil || !*got {
		t.Errorf("%s sets shareProcessNamespace to %v, want true", path, got)
	}
}
