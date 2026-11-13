package cmd

import (
	"context"
	"encoding/json"
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

// checkDaemonImage judges every daemon pod on its own: a pod being deleted is
// skipped, any other pod must be Running and report an image id whose digest is
// the values file's daemon digest. The read is per pod, so a pod that reports no
// id cannot pass beside one that does. A list with no pod left to judge fails.
func (c podPairCheck) checkDaemonImage(ctx context.Context, daemon string) error {
	out, err := c.kubectl(ctx, "read of the daemon pods", nil, "get", "pods", "-l", podDaemonSelector, "-o", "json")
	if err != nil {
		return err
	}
	var list podListing
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return errors.New("the daemon pods' read is not a pod list")
	}
	want := podRefDigest(daemon)
	judged := 0
	for _, pod := range list.Items {
		if pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		judged++
		name := pod.Metadata.Name
		if pod.Status.Phase != "Running" {
			return fmt.Errorf("pod %s is in phase %q, not Running, so it cannot run the pair", name, pod.Status.Phase)
		}
		id := daemonImageID(pod)
		if id == "" {
			return fmt.Errorf("pod %s reports no image id", name)
		}
		got := podRefDigest(id)
		if got == "" {
			return fmt.Errorf("pod %s reports an image id that names no digest", name)
		}
		if got != want {
			return fmt.Errorf("pod %s runs daemon digest %s, not the values file's %s", name, got, want)
		}
	}
	if judged == 0 {
		return errors.New("no daemon pod to verify")
	}
	return nil
}

// podListing is the part of `kubectl get pods -o json` the pair check reads.
type podListing struct {
	Items []podItem `json:"items"`
}

// podItem is one pod: its name and deletion mark, its phase, and its containers'
// statuses.
type podItem struct {
	Metadata struct {
		Name              string  `json:"name"`
		DeletionTimestamp *string `json:"deletionTimestamp"`
	} `json:"metadata"`
	Status struct {
		Phase             string            `json:"phase"`
		ContainerStatuses []containerStatus `json:"containerStatuses"`
	} `json:"status"`
}

// containerStatus is one container's status; ImageID is what the runtime ran.
type containerStatus struct {
	Name    string `json:"name"`
	ImageID string `json:"imageID"`
}

// daemonImageID is the image id a pod reports for its daemon container, or "" when
// the pod reports none for it.
func daemonImageID(pod podItem) string {
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == k8sdaemon.Name {
			return status.ImageID
		}
	}
	return ""
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

// podRefDigest is the digest after the last '@' of a reference. A pod's image id
// may carry a runtime prefix before the reference, so the last '@' is the one that
// starts the digest. A reference with no '@' has no digest: "".
func podRefDigest(ref string) string {
	i := strings.LastIndex(ref, "@")
	if i < 0 {
		return ""
	}
	return ref[i+1:]
}
