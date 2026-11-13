package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	// podDevTagWait bounds the wait for the daemon's start-time copy of the dev
	// tag. It is over the daemon's own publish budget (two minutes), so a copy
	// still in flight is waited out and one that never lands fails the install
	// rather than the bound expiring while it is still plausible.
	podDevTagWait = 3 * time.Minute
	// podDevTagPoll is the wait between dev tag reads.
	podDevTagPoll = 5 * time.Second
)

// podPairCheck is the check a digest install runs once a daemon that runs as a
// pod reports the new pair. The daemon pods must run the values file's daemon
// digest, the sandbox registry's dev tag of the polecat image must name the
// values file's polecat digest, and then the values file is copied into the pod,
// so the pod holds the file for the pair it runs. A failure is returned to the
// install, which restores the previous pair. Errors name the step and the
// digests, never a command's output.
type podPairCheck struct {
	// Run runs kubectl. Every command the check makes goes through it.
	Run update.RunFunc
	// Target is the Deployment the pair moved.
	Target podDaemonTarget
	// ValuesPath is the values file the pair came from.
	ValuesPath string
	// Clock stamps the wait for the daemon's copy of the dev tag. Nil is the
	// real clock.
	Clock update.Clock
	// Sleep waits between dev tag reads and returns the context's error, so the
	// wait ends with a canceled install. Nil is a sleep that does the same.
	Sleep func(context.Context, time.Duration) error
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
		return update.PairCheckError{Check: update.CheckRunningDigests, Err: err}
	}
	if err := c.checkPolecatTag(ctx, values.Polecat); err != nil {
		return update.PairCheckError{Check: update.CheckRegistryTag, Err: err}
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

// checkPolecatTag reads the dev tag of the polecat image from the registry the
// daemon publishes it to, through the daemon pod: only that pod reaches the
// sandbox registry, and the tag lives there, not in the Forgejo registry the
// town's images come from (gt-5yh32c). The daemon copies the tag in a start-time
// goroutine, so a read is polled up to podDevTagWait; a bound that passes fails
// with the state it last saw.
func (c podPairCheck) checkPolecatTag(ctx context.Context, polecat string) error {
	// The values file parses only refs of the form repo@sha256:<64 hex>, so the
	// digest the pair carries is always there to compare against.
	want := podRefDigest(polecat)
	saw := ""
	deadline := c.now().Add(podDevTagWait)
	for {
		read, err := c.readPodDevTag(ctx)
		switch {
		case err != nil:
			saw = "could not be read: " + err.Error()
		case !read.Present:
			saw = "has no dev tag"
		case read.Digest == want:
			return nil
		default:
			saw = "names digest " + read.Digest
		}
		if !c.now().Before(deadline) {
			return fmt.Errorf("the sandbox registry's %s tag for the polecat image %s after waiting %s for the daemon's start-time copy", podPolecatTag, saw, podDevTagWait)
		}
		if err := c.sleep(ctx, podDevTagPoll); err != nil {
			return fmt.Errorf("waiting for the sandbox registry's %s tag for the polecat image: %w", podPolecatTag, err)
		}
	}
}

// readPodDevTag runs the dev tag read in the daemon container, the one pod with
// a route to the sandbox registry, and decodes its answer. The runner joins
// kubectl's stderr to its stdout, so the answer is taken as the last line shaped
// like the JSON object rather than as the whole output, which a kubectl warning
// can surround.
func (c podPairCheck) readPodDevTag(ctx context.Context) (polecatDevTagRead, error) {
	argv := append([]string{"exec", "deployment/" + c.Target.Deployment, "-c", k8sdaemon.Name, "--"}, polecatDevTagCommand...)
	out, err := c.kubectl(ctx, "read of the dev tag in the daemon pod", nil, argv...)
	if err != nil {
		return polecatDevTagRead{}, err
	}
	line := ""
	for _, candidate := range strings.Split(out, "\n") {
		if candidate = strings.TrimSpace(candidate); strings.HasPrefix(candidate, "{") && strings.HasSuffix(candidate, "}") {
			line = candidate
		}
	}
	if line == "" {
		return polecatDevTagRead{}, errors.New("the dev tag read in the daemon pod did not answer JSON")
	}
	var read polecatDevTagRead
	if err := json.Unmarshal([]byte(line), &read); err != nil {
		return polecatDevTagRead{}, errors.New("the dev tag read in the daemon pod did not answer JSON")
	}
	if read.Present && !imagevalues.ValidDigest(read.Digest) {
		return polecatDevTagRead{}, errors.New("the dev tag read in the daemon pod named no sha256 digest")
	}
	return read, nil
}

// now is the check's clock: the wired one, or the real clock.
func (c podPairCheck) now() time.Time {
	if c.Clock != nil {
		return c.Clock.Now()
	}
	return time.Now()
}

// sleep waits d on the wired sleep, or on the real one.
func (c podPairCheck) sleep(ctx context.Context, d time.Duration) error {
	if c.Sleep != nil {
		return c.Sleep(ctx, d)
	}
	return updateSleep(ctx, d)
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
