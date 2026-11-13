package daemon

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	updatejob "github.com/steveyegge/gastown/deploy/k8s/update-job"
	"github.com/steveyegge/gastown/internal/attention"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
)

// kubernetesServiceHostEnv is the variable the kubelet sets in every pod. A
// daemon that reads it runs as a pod, and a roll it starts must outlive the pod
// it replaces, so the roll runs as a Job in the cluster rather than as a child
// of this process.
const kubernetesServiceHostEnv = "KUBERNETES_SERVICE_HOST"

// updateJobNamePrefix names the Job a pod roll creates: gt-update-<sha12> for
// the green commit it rolls to. The same commit names the same Job, so a second
// launch of one commit finds the first.
const updateJobNamePrefix = "gt-update-"

// errUpdateJobExists is what a cluster client returns when the Job it was asked
// to create is already there: the roll for that commit was launched already.
var errUpdateJobExists = errors.New("the update Job already exists")

// updateJobClient creates the roll's Job in the cluster from its rendered
// manifest. Production is kubectlUpdateJob; a test injects a fake.
type updateJobClient interface {
	CreateJob(ctx context.Context, name string, manifest []byte) error
}

// updateJobEncoder writes a rendered object as YAML. Production is
// updatejob.YAML; a test injects one that fails, since no rendered Job can fail
// to encode.
type updateJobEncoder func(w io.Writer, v any) error

// kubectlUpdateJob creates the Job with kubectl. It names no context, so in a pod
// kubectl uses the pod's service account. The manifest is written under dir first,
// because update.RunFunc has no stdin.
type kubectlUpdateJob struct {
	dir string
	run update.RunFunc
}

// newKubectlUpdateJob is the production client for a daemon in townRoot.
func newKubectlUpdateJob(townRoot string) updateJobClient {
	return kubectlUpdateJob{dir: update.Dir(townRoot), run: update.ExecRun}
}

// CreateJob applies manifest as the Job name. kubectl's AlreadyExists answer is
// errUpdateJobExists; any other refusal names the exit status only, since the
// output of kubectl is not echoed.
func (k kubectlUpdateJob) CreateJob(ctx context.Context, name string, manifest []byte) error {
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		return fmt.Errorf("create the update directory %s: %w", k.dir, err)
	}
	path := filepath.Join(k.dir, name+".yaml")
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		return fmt.Errorf("write the manifest of %s: %w", name, err)
	}
	out, exit, err := k.run(ctx, "", nil, "kubectl", "create", "-f", path)
	if err != nil {
		return fmt.Errorf("running kubectl create for %s: %w", name, err)
	}
	if exit == 0 {
		return nil
	}
	if strings.Contains(out, "AlreadyExists") {
		return errUpdateJobExists
	}
	return fmt.Errorf("kubectl create for %s exited %d", name, exit)
}

// inClusterFromEnv reports whether the environment getenv reads names a pod.
func inClusterFromEnv(getenv func(string) string) bool {
	return getenv(kubernetesServiceHostEnv) != ""
}

// updateJobName is the Job name for the green commit sha.
func updateJobName(sha string) string {
	return updateJobNamePrefix + greenRefShortSHA(sha)
}

// launchUpdateJob creates the roll Job for the green commit sha. It is the UR
// template rendered for that commit, with the running daemon image, renamed to
// updateJobName(sha). A Job that already exists is errUpdateJobExists and is not
// created twice.
func (d *Daemon) launchUpdateJob(ctx context.Context, settings config.UpdateSettings, sha string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if d.updateJob == nil {
		return errors.New("no cluster client is wired into this daemon")
	}
	running, ok, err := imagevalues.LoadIn(filepath.Join(d.config.TownRoot, "gastown"))
	if err != nil {
		return fmt.Errorf("read the running images: %w", err)
	}
	if !ok {
		return errors.New("no values file names the running daemon image")
	}
	m, err := updatejob.Build(updatejob.Values{
		Image:       running.Daemon,
		GreenCommit: sha,
		MaxRuntime:  settings.MaxRuntime,
	})
	if err != nil {
		return fmt.Errorf("render the update Job: %w", err)
	}
	name := updateJobName(sha)
	m.Job.Metadata.Name = name
	encode := d.updateJobEncode
	if encode == nil {
		encode = updatejob.YAML
	}
	var buf bytes.Buffer
	if err := encode(&buf, m.Job); err != nil {
		return fmt.Errorf("encode the update Job: %w", err)
	}
	return d.updateJob.CreateJob(ctx, name, buf.Bytes())
}

// pairReadySHAForRoll is the green commit a pod roll goes to: the pair-ready item
// that is ahead of the running commit. clone returns the rig's local clone, which
// judges ancestry with no fetch, the same read the pair check makes. Among several
// candidates the one every other descends from is the roll. It returns the sha, or
// "" and the reason no roll starts. A candidate the clone cannot judge refuses the
// whole decision, so an unjudged pair never stands in for a judged one.
func pairReadySHAForRoll(items []attention.Item, running string, clone func(rig string) pairReadyGit) (string, string) {
	type candidate struct {
		sha string
		git pairReadyGit
	}
	var cands []candidate
	for _, it := range items {
		if it.Kind != attention.KindDeployPairReady || it.SHA == running {
			continue
		}
		git := clone(it.Rig)
		if git == nil {
			return "", fmt.Sprintf("the local clone of %s is missing, so pair-ready %s cannot be judged", it.Rig, greenRefShortSHA(it.SHA))
		}
		ahead, err := git.IsAncestor(running, it.SHA)
		if err != nil {
			return "", fmt.Sprintf("judging pair-ready %s against the running commit in the local clone: %v", greenRefShortSHA(it.SHA), err)
		}
		if ahead {
			cands = append(cands, candidate{sha: it.SHA, git: git})
		}
	}
	switch len(cands) {
	case 0:
		return "", "no pair-ready green commit is ahead of the running commit"
	case 1:
		return cands[0].sha, ""
	}
	for _, c := range cands {
		newest := true
		for _, o := range cands {
			if o.sha == c.sha {
				continue
			}
			below, err := c.git.IsAncestor(o.sha, c.sha)
			if err != nil {
				return "", fmt.Sprintf("judging pair-ready %s against %s in the local clone: %v", greenRefShortSHA(o.sha), greenRefShortSHA(c.sha), err)
			}
			if !below {
				newest = false
				break
			}
		}
		if newest {
			return c.sha, ""
		}
	}
	return "", "the pair-ready green commits do not form one line; not launching"
}

// triggerPodUpdate is the due decision for a daemon that runs as a pod. The roll
// starts only when a pair-ready green commit is ahead of the running one, and it
// starts as the Job the cluster runs, never as a child this process would lose
// when the roll replaces it.
func (d *Daemon) triggerPodUpdate(now time.Time, settings config.UpdateSettings, reason string) {
	running, err := pairReadyRunningCommit(d.config.TownRoot)
	if err != nil {
		d.noteUpdateNotDue("cannot read the running commit: " + err.Error())
		return
	}
	sha, why := pairReadySHAForRoll(d.pairReady.observed(), running, d.pairReadyBareClone)
	if sha == "" {
		d.noteUpdateNotDue(why)
		return
	}
	d.clearUpdateNotDue()
	if d.updateRefusedRecently(now) {
		return
	}
	d.logger.Printf("update: due (%s); pair-ready %s; starting the update Job", reason, greenRefShortSHA(sha))
	err = d.launchUpdateJob(context.Background(), settings, sha)
	switch {
	case errors.Is(err, errUpdateJobExists):
		// The roll for this commit is in the cluster already: wait out the
		// suppression as for a launch, rather than create it again.
		d.noteUpdateLaunch(now)
		d.logger.Printf("update: Job %s is already launched", updateJobName(sha))
	case err != nil:
		d.logger.Printf("update: launch of the update Job failed: %v; not retrying for %s", err, updateLaunchSuppression)
		d.noteUpdateRefusal(now)
	default:
		d.noteUpdateLaunch(now)
		d.logger.Printf("update: created Job %s", updateJobName(sha))
	}
}
