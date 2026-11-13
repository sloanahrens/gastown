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

const (
	// updateJobCreateTimeout bounds one create of the roll's Job, on the daemon's
	// clock. The create runs on the heartbeat, so a cluster that does not answer
	// costs this much and no more (gt-hlo2ac is the same class).
	updateJobCreateTimeout = 30 * time.Second

	// updateJobRequestSlack is how much shorter kubectl's own --request-timeout is
	// than updateJobCreateTimeout, so kubectl gives up before the daemon abandons
	// the call and the two bounds agree.
	updateJobRequestSlack = 5 * time.Second

	// updateJobBackoffBase is the wait after the first failed launch. Each further
	// consecutive failure doubles it, up to updateLaunchSuppression.
	updateJobBackoffBase = time.Minute
)

// errUpdateJobExists is what a cluster client returns when the Job it was asked
// to create is already there: the roll for that commit was launched already.
var errUpdateJobExists = errors.New("the update Job already exists")

// errUpdateJobNoAnswer is the create's outcome when the cluster did not answer
// within the bound. The create may still land on the server; the next attempt
// finds it by name.
var errUpdateJobNoAnswer = errors.New("the cluster did not answer the create within its bound")

// errUpdateJobOutstanding refuses an attempt while an earlier abandoned create
// has not returned, so a hung cluster holds one goroutine, not one per attempt.
var errUpdateJobOutstanding = errors.New("an earlier create of the update Job has not returned")

// updateJobClient creates the roll's Job in the cluster from its rendered
// manifest, and bounds the request by requestTimeout. Production is
// kubectlUpdateJob; a test injects a fake.
type updateJobClient interface {
	CreateJob(ctx context.Context, name string, manifest []byte, requestTimeout time.Duration) error
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

// CreateJob applies manifest as the Job name, with kubectl's own request timeout
// set to requestTimeout. kubectl reads zero as no bound, so a zero bound is
// refused before anything is written. kubectl's AlreadyExists answer is
// errUpdateJobExists; any other refusal names the exit status only, since the
// output of kubectl is not echoed.
func (k kubectlUpdateJob) CreateJob(ctx context.Context, name string, manifest []byte, requestTimeout time.Duration) error {
	if requestTimeout <= 0 {
		return fmt.Errorf("the create of %s has no request timeout to send", name)
	}
	if err := os.MkdirAll(k.dir, 0o700); err != nil {
		return fmt.Errorf("create the update directory %s: %w", k.dir, err)
	}
	path := filepath.Join(k.dir, name+".yaml")
	if err := os.WriteFile(path, manifest, 0o600); err != nil {
		return fmt.Errorf("write the manifest of %s: %w", name, err)
	}
	out, exit, err := k.run(ctx, "", nil, "kubectl", "create", "-f", path, "--request-timeout="+requestTimeout.String())
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
	return d.updateJob.CreateJob(ctx, name, buf.Bytes(), d.updateJobRequestTimeout())
}

// updateJobBound is the deadline on one create: updateJobCreateTimeout, unless a
// test shortened it through updateTriggerState.jobBound.
func (d *Daemon) updateJobBound() time.Duration {
	d.updateTrigger.mu.Lock()
	defer d.updateTrigger.mu.Unlock()
	if d.updateTrigger.jobBound > 0 {
		return d.updateTrigger.jobBound
	}
	return updateJobCreateTimeout
}

// updateJobRequestTimeout is kubectl's bound on the create: the deadline less
// updateJobRequestSlack, so kubectl gives up before the daemon abandons the call.
func (d *Daemon) updateJobRequestTimeout() time.Duration {
	return d.updateJobBound() - updateJobRequestSlack
}

// launchUpdateJobBounded runs launchUpdateJob under updateJobBound on the
// daemon's clock. The create is abandoned at the deadline whether or not the
// cluster client honors its context, so one hung cluster costs the heartbeat one
// bound (gt-hlo2ac). An abandoned create keeps its goroutine until it returns;
// until then a later attempt returns errUpdateJobOutstanding instead of starting
// a second create. A create that times out is never recorded as launched: the
// retry finds the Job by its name if the server took it.
func (d *Daemon) launchUpdateJobBounded(settings config.UpdateSettings, sha string) error {
	d.updateTrigger.mu.Lock()
	outstanding := d.updateTrigger.jobOutstanding
	d.updateTrigger.mu.Unlock()
	if outstanding != nil {
		select {
		case <-outstanding:
		default:
			return errUpdateJobOutstanding
		}
	}
	returned := make(chan struct{})
	d.updateTrigger.mu.Lock()
	d.updateTrigger.jobOutstanding = returned
	d.updateTrigger.mu.Unlock()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	deadline := d.clk().AfterFunc(d.updateJobBound(), func() { cancel(errUpdateJobNoAnswer) })
	defer deadline.Stop()

	done := make(chan error, 1)
	go func() {
		defer close(returned)
		done <- d.launchUpdateJob(ctx, settings, sha)
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return errUpdateJobNoAnswer
	}
}

// updateJobBackoff is the wait after the failures-th consecutive failed pod
// launch: updateJobBackoffBase, doubled for each further failure, capped at
// updateLaunchSuppression. The cap is the same wait a refused child launch gets.
func updateJobBackoff(failures int) time.Duration {
	wait := updateJobBackoffBase
	for i := 1; i < failures && wait < updateLaunchSuppression; i++ {
		wait *= 2
	}
	return min(wait, updateLaunchSuppression)
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
			return "", fmt.Sprintf("clone missing: the local clone of %s is missing, so pair-ready %s cannot be judged", it.Rig, greenRefShortSHA(it.SHA))
		}
		ahead, err := git.IsAncestor(running, it.SHA)
		if err != nil {
			return "", fmt.Sprintf("ancestry check failed: judging pair-ready %s against the running commit in the local clone: %v", greenRefShortSHA(it.SHA), err)
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
				return "", fmt.Sprintf("pair not judgeable: judging pair-ready %s against %s in the local clone: %v", greenRefShortSHA(o.sha), greenRefShortSHA(c.sha), err)
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
	return "", "commits not linear: the pair-ready green commits do not form one line; not launching"
}

// triggerPodUpdate is the due decision for a daemon that runs as a pod. The roll
// starts only when a pair-ready green commit is ahead of the running one, and it
// starts as the Job the cluster runs, never as a child this process would lose
// when the roll replaces it.
func (d *Daemon) triggerPodUpdate(now time.Time, settings config.UpdateSettings, reason string) {
	running, err := pairReadyRunningCommit(d.config.TownRoot)
	if err != nil {
		d.noteUpdateNotDue("running commit unreadable: " + err.Error())
		return
	}
	sha, why := pairReadySHAForRoll(d.pairReady.observed(), running, d.pairReadyBareClone)
	if sha == "" {
		d.noteUpdateNotDue(why)
		return
	}
	d.clearUpdateNotDue()
	if d.updateJobBackingOff(now) {
		return
	}
	d.logger.Printf("update: due (%s); pair-ready %s; starting the update Job", reason, greenRefShortSHA(sha))
	err = d.launchUpdateJobBounded(settings, sha)
	switch {
	case errors.Is(err, errUpdateJobExists):
		// The roll for this commit is in the cluster already: wait out the
		// suppression as for a launch, rather than create it again.
		d.noteUpdateJobSuccess()
		d.noteUpdateLaunch(now)
		d.logger.Printf("update: Job %s is already launched", updateJobName(sha))
	case errors.Is(err, errUpdateJobNoAnswer):
		wait := d.noteUpdateJobFailure(now)
		d.logger.Printf("update: launch of the update Job timed out after %s; not launched; not retrying for %s", d.updateJobBound(), wait)
	case err != nil:
		wait := d.noteUpdateJobFailure(now)
		d.logger.Printf("update: launch of the update Job failed: %v; not launched; not retrying for %s", err, wait)
	default:
		d.noteUpdateJobSuccess()
		d.noteUpdateLaunch(now)
		d.logger.Printf("update: created Job %s", updateJobName(sha))
	}
}

// updateJobBackingOff reports whether the last failed pod launch is recent
// enough that this heartbeat waits out its backoff rather than trying again.
func (d *Daemon) updateJobBackingOff(now time.Time) bool {
	d.updateTrigger.mu.Lock()
	defer d.updateTrigger.mu.Unlock()
	if d.updateTrigger.jobFailedAt.IsZero() {
		return false
	}
	return now.Sub(d.updateTrigger.jobFailedAt) < updateJobBackoff(d.updateTrigger.jobFailures)
}

// noteUpdateJobFailure records a pod launch that timed out or was refused, so the
// next attempt waits out the doubled backoff. It returns that wait for the log.
func (d *Daemon) noteUpdateJobFailure(now time.Time) time.Duration {
	d.updateTrigger.mu.Lock()
	defer d.updateTrigger.mu.Unlock()
	d.updateTrigger.jobFailures++
	d.updateTrigger.jobFailedAt = now
	return updateJobBackoff(d.updateTrigger.jobFailures)
}

// noteUpdateJobSuccess clears the pod launch failures once a Job is created or
// found, so the next failure starts the backoff again from its first step.
func (d *Daemon) noteUpdateJobSuccess() {
	d.updateTrigger.mu.Lock()
	defer d.updateTrigger.mu.Unlock()
	d.updateTrigger.jobFailures = 0
	d.updateTrigger.jobFailedAt = time.Time{}
}
