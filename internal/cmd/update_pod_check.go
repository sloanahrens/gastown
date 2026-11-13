package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	k8sdaemon "github.com/steveyegge/gastown/deploy/k8s/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

const (
	// podPolecatTag is the registry tag the polecat image's dev build sits at. The
	// pod's pair must name the same polecat digest.
	podPolecatTag = "dev"
	// podImagesValuesName is the values file's name in the pod's update directory.
	podImagesValuesName = "images-values.yaml"
	// podDaemonSelector selects the daemon's pods by the label the Deployment's
	// selector carries.
	podDaemonSelector = "app.kubernetes.io/name=" + k8sdaemon.Name
)

// podPairCheck is the check a digest install runs once a daemon that runs as a
// pod reports the new pair. The daemon pods must run the values file's daemon
// digest, the registry's dev tag of the polecat image must name the values file's
// polecat digest, and then the values file is copied into the pod, so the pod
// holds the file for the pair it runs. A failure is returned to the
// install, which restores the previous pair. Errors name the step and the digests,
// never a command's output.
type podPairCheck struct {
	// Run runs kubectl. Every command the check makes goes through it.
	Run update.RunFunc
	// Target is the Deployment the pair moved.
	Target podDaemonTarget
	// Images is the registry reader and viewer credential the dev tag is read
	// through: the same seams gt update images reads with.
	Images updateImagesSeams
	// TownRoot is the local town whose registry Images resolves.
	TownRoot string
	// ValuesPath is the values file the pair came from.
	ValuesPath string
}

// Check runs the three checks in order and stops at the first that fails. Nothing
// is written into the pod unless both digests hold.
func (c podPairCheck) Check(ctx context.Context, pair update.ImagePair) error {
	data, err := os.ReadFile(c.ValuesPath)
	if err != nil {
		return fmt.Errorf("reading the values file for the pod check: %w", err)
	}
	values, err := imagevalues.Parse(data)
	if err != nil {
		return fmt.Errorf("the values file for the pod check: %w", err)
	}
	if values.Daemon != pair.Daemon || values.Polecat != pair.Polecat || values.Commit != pair.Commit {
		return errors.New("the values file no longer holds the pair the swap put in force")
	}
	if err := c.checkDaemonImage(ctx, values.Daemon); err != nil {
		return err
	}
	if err := c.checkPolecatTag(ctx, values.Polecat); err != nil {
		return err
	}
	return c.writeValues(ctx, data)
}

// checkDaemonImage holds the daemon's pods to the values file's daemon digest:
// there must be a pod that reports an image id, and every such id must carry the
// digest.
func (c podPairCheck) checkDaemonImage(ctx context.Context, daemon string) error {
	digest := podRefDigest(daemon)
	jsonpath := fmt.Sprintf("{.items[*].status.containerStatuses[?(@.name==%q)].imageID}", k8sdaemon.Name)
	out, err := c.kubectl(ctx, "read of the daemon pods' image ids", nil, "get", "pods", "-l", podDaemonSelector, "-o", "jsonpath="+jsonpath)
	if err != nil {
		return err
	}
	ids := strings.Fields(out)
	if len(ids) == 0 {
		return errors.New("no daemon pod reports an image id for the daemon container")
	}
	for _, id := range ids {
		if !strings.Contains(id, digest) {
			return fmt.Errorf("a daemon pod does not run the values file's daemon digest %s", digest)
		}
	}
	return nil
}

// checkPolecatTag reads the registry's dev tag of the polecat image through the
// viewer credential and holds its digest to the values file's polecat digest. A
// missing tag is a failure: the pair cannot be shown to be the dev build.
func (c podPairCheck) checkPolecatTag(ctx context.Context, polecat string) error {
	seams := c.Images
	if seams.Registry == nil || seams.Credentials == nil || seams.HTTPClient == nil {
		return errors.New("no registry reader is wired for the pod's dev tag check")
	}
	reg, err := seams.Registry(c.TownRoot)
	if err != nil {
		return fmt.Errorf("reading the registry for the pod's dev tag check: %w", err)
	}
	user, token, err := seams.Credentials()
	if err != nil {
		return fmt.Errorf("reading the viewer credential for the pod's dev tag check: %w", err)
	}
	digest, found, err := imagevalues.TagDigest(ctx, seams.HTTPClient, reg, plaintextHostOf(reg), user, token, imagevalues.PolecatImage, podPolecatTag)
	if err != nil {
		return fmt.Errorf("reading the %s tag of the polecat image: %w", podPolecatTag, err)
	}
	if !found {
		return fmt.Errorf("the registry holds no %s tag for the polecat image", podPolecatTag)
	}
	if want := podRefDigest(polecat); digest != want {
		return fmt.Errorf("the registry's %s tag for the polecat image is digest %s, not the values file's %s", podPolecatTag, digest, want)
	}
	return nil
}

// writeValues copies the values file's bytes into the update directory under the
// pod's own town root, TownRootPath, not the job's local path. tee does not make
// the directory, so it is made first; the bytes go in on stdin.
func (c podPairCheck) writeValues(ctx context.Context, data []byte) error {
	dir := update.Dir(k8sdaemon.TownRootPath)
	deployment := "deployment/" + c.Target.Deployment
	if _, err := c.kubectl(ctx, "creation of the update directory", nil, "exec", deployment, "--", "mkdir", "-p", dir); err != nil {
		return err
	}
	_, err := c.kubectl(ctx, "copy of the values file", data, "exec", "-i", deployment, "--", "tee", filepath.Join(dir, podImagesValuesName))
	return err
}

// kubectl runs one kubectl command against the pod's target, under podProbeBound.
// stdin, when not nil, is what the command reads. A nonzero exit or a Run error is
// named by step; the command's output is not.
func (c podPairCheck) kubectl(ctx context.Context, step string, stdin []byte, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(withRunStdin(ctx, stdin), podProbeBound)
	defer cancel()
	argv := append(podKubectlPrefix(c.Target.Context, c.Target.Namespace, c.Target.InCluster), args...)
	out, exit, err := c.Run(ctx, "", nil, "kubectl", argv...)
	if err != nil {
		return "", fmt.Errorf("running kubectl for the pod's %s: %w", step, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("kubectl for the pod's %s exited %d", step, exit)
	}
	return out, nil
}

// podRefDigest is the digest part of a digest reference. The values file has
// parsed, so the reference holds the '@' it is cut at.
func podRefDigest(ref string) string {
	_, digest, _ := strings.Cut(ref, "@")
	return digest
}
