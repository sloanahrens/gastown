package cmd

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	forgejorunner "github.com/steveyegge/gastown/deploy/k8s/forgejo-runner"
	"github.com/steveyegge/gastown/internal/update"
	"gopkg.in/yaml.v3"
)

// engineReadWait bounds one read of the runner's engine: a kubectl exec under
// the roll's deadline-less signal context, which would otherwise hold the
// landing hold open (gt-wvicts).
const engineReadWait = 30 * time.Second

// Verify checks the live runner against its render: each label whose image has
// no registry host must be held by the engine, and a label on a registry host
// is not checked (gt-wvicts).
func (a runnerApplier) Verify(ctx context.Context) ([]string, error) {
	if a.kubectl == nil {
		return nil, errors.New("no cluster client is wired, so the runner's images cannot be checked")
	}
	m, err := a.manifests()
	if err != nil {
		return nil, err
	}
	labels, err := runnerConfigLabels(m.ConfigMap)
	if err != nil {
		return nil, err
	}
	e, err := engineForDeployment(m.Deployment)
	if err != nil {
		return nil, err
	}
	lines := make([]string, 0, len(labels))
	for _, label := range labels {
		name, image, err := runnerLabelImage(label)
		if err != nil {
			return nil, err
		}
		line, err := e.checkImage(ctx, a.kubectl, name, image)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

// runnerConfigLabels is the labels the rendered ConfigMap carries, decoded from
// its one data value, so a ConfigMap that renders none refuses rather than
// checking nothing.
func runnerConfigLabels(cm forgejorunner.ConfigMap) ([]string, error) {
	if len(cm.Data) != 1 {
		return nil, fmt.Errorf("the rendered ConfigMap %s holds %d data keys; want the runner config alone", cm.Metadata.Name, len(cm.Data))
	}
	var doc struct {
		Runner struct {
			Labels []string `yaml:"labels"`
		} `yaml:"runner"`
	}
	for _, config := range cm.Data {
		if err := yaml.Unmarshal([]byte(config), &doc); err != nil {
			return nil, fmt.Errorf("reading the rendered runner config: %w", err)
		}
	}
	if len(doc.Runner.Labels) == 0 {
		return nil, errors.New("the rendered runner config names no labels, so no image can be checked")
	}
	return doc.Runner.Labels, nil
}

// runnerLabelImage splits a rendered "name:docker://image" label. A label the
// render should never produce is refused, never skipped.
func runnerLabelImage(label string) (string, string, error) {
	name, ref, ok := strings.Cut(label, ":")
	if !ok || name == "" {
		return "", "", fmt.Errorf("the runner label %q names no image; want name:docker://image", label)
	}
	image, ok := strings.CutPrefix(ref, "docker://")
	if !ok || image == "" {
		return "", "", fmt.Errorf("the runner label %q names no docker:// image; want name:docker://image", label)
	}
	return name, image, nil
}

// imageRegistryHost is the registry host an image names, or "" for none: docker
// reads the first path component as one when it holds a dot or a port, or is
// localhost.
func imageRegistryHost(image string) string {
	first, _, ok := strings.Cut(image, "/")
	if ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return first
	}
	return ""
}

// runnerEngineSidecar is the pod the checks run in.
type runnerEngineSidecar struct {
	namespace, deployment, container, cli, image string
}

// engineForDeployment reads the sidecar and the CLI its image provides from the
// render; one naming no Deployment, namespace or engine container is refused.
func engineForDeployment(d forgejorunner.Deployment) (runnerEngineSidecar, error) {
	e := runnerEngineSidecar{namespace: d.Metadata.Namespace, deployment: d.Metadata.Name, container: forgejorunner.DindContainerName}
	if e.namespace == "" || e.deployment == "" {
		return runnerEngineSidecar{}, fmt.Errorf("the rendered Deployment is named %q in namespace %q, so the engine cannot be reached", e.deployment, e.namespace)
	}
	for _, c := range d.Spec.Template.Spec.InitContainers {
		if c.Name == forgejorunner.DindContainerName {
			e.image, e.cli = c.Image, engineCLI(c.Image)
		}
	}
	if e.cli == "" {
		return runnerEngineSidecar{}, errors.New("the rendered Deployment has no engine sidecar image, so its CLI cannot be named")
	}
	return e, nil
}

// engineCLI is the command the engine's image provides: its repository's last
// path component, "docker" for data.forgejo.org/oci/docker:dind.
func engineCLI(image string) string {
	if at := strings.Index(image, "@"); at >= 0 {
		image = image[:at]
	}
	image = image[strings.LastIndex(image, "/")+1:]
	if colon := strings.LastIndex(image, ":"); colon >= 0 {
		image = image[:colon]
	}
	return image
}

// execEngine runs one engine command in the sidecar, returning its exit status
// apart from the error so a command that refused is told from one that did not
// run.
func (e runnerEngineSidecar) execEngine(ctx context.Context, kubectl update.RunFunc, args ...string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, engineReadWait)
	defer cancel()
	argv := append([]string{"exec", "-n", e.namespace, "deploy/" + e.deployment, "-c", e.container, "--", e.cli}, args...)
	_, exit, err := kubectl(ctx, "", nil, "kubectl", argv...)
	return exit, err
}

// checkImage answers for one label. An image on a registry host is pulled on
// demand, so it is skipped and never reported missing; the engine is not asked
// about it. An image with no registry host can come only from the engine, so
// the engine must hold it (gt-wvicts).
func (e runnerEngineSidecar) checkImage(ctx context.Context, kubectl update.RunFunc, label, image string) (string, error) {
	if host := imageRegistryHost(image); host != "" {
		return fmt.Sprintf("label %s: image %s is pulled on demand from %s, not checked", label, image, host), nil
	}
	exit, err := e.execEngine(ctx, kubectl, "image", "inspect", image)
	if err != nil {
		return "", fmt.Errorf("asking the engine %s whether it holds image %s of label %s: %w", e.image, image, label, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("label %s names image %s, which has no registry host, and the engine %s does not hold it", label, image, e.image)
	}
	return fmt.Sprintf("label %s: the engine holds image %s", label, image), nil
}
