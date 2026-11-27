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
	"github.com/steveyegge/gastown/internal/constants"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/update"
	"github.com/steveyegge/gastown/internal/workspace"
)

const (
	// verifyImage is the sandbox image the installer publishes to.
	verifyImage = "gastown-polecat:dev"

	// verifyImageCommitLabel is the image label that carries the build's
	// commit.
	verifyImageCommitLabel = "org.gastown.gt-commit"

	// verifyImageLabelFormat is docker's --format template for that label.
	verifyImageLabelFormat = `{{index .Config.Labels "` + verifyImageCommitLabel + `"}}`
)

// verifyProbeEnv is the real state the verify probes read and the seams that
// let a test replace it. A nil field takes its production value in
// newVerifyProbes.
type verifyProbeEnv struct {
	// Exe is the path of the installed gt.
	Exe func() (string, error)
	// Run runs gt and docker.
	Run update.RunFunc
	// DaemonCommit is the running daemon's build commit, from the daemon
	// state.
	DaemonCommit func() (string, error)
	// ValuesDir is the directory the CI-built values file sits under. A file
	// there names the sandbox image ImageLabel inspects; nil means no values
	// file, the probe's behavior before the file existed.
	ValuesDir func() string
}

// newVerifyProbes returns the verify step's probes over env, filling each nil
// seam with the real one. The installed gt is read by running the file at
// Exe's path: after an install the running process is still the old binary,
// so its own build info would answer for the build that was replaced.
func newVerifyProbes(env verifyProbeEnv) update.VerifyProbes {
	if env.Exe == nil {
		env.Exe = os.Executable
	}
	if env.Run == nil {
		env.Run = update.ExecRun
	}
	if env.DaemonCommit == nil {
		env.DaemonCommit = daemonStateCommit
	}
	return &verifyProbes{env: env}
}

// daemonStateCommit is the running daemon's commit from the town's daemon
// state. An unreadable state is an error: it says nothing about which build
// the daemon runs.
func daemonStateCommit() (string, error) {
	return daemonCommitFrom(workspace.FindFromCwdOrError, daemon.LoadState)
}

// daemonCommitFrom reads the daemon's commit through the town root and state
// loader it is given.
func daemonCommitFrom(findTownRoot func() (string, error), loadState func(string) (*daemon.State, error)) (string, error) {
	townRoot, err := findTownRoot()
	if err != nil {
		return "", err
	}
	state, err := loadState(townRoot)
	if err != nil {
		return "", err
	}
	return state.Commit, nil
}

// verifyProbes answers the verify step's questions through env.
type verifyProbes struct {
	env verifyProbeEnv
}

var _ update.ReadyProbes = (*verifyProbes)(nil)

// GTVersion is the commit of the gt file at the executable's path.
func (p *verifyProbes) GTVersion(ctx context.Context) (string, error) {
	exe, err := p.env.Exe()
	if err != nil {
		return "", fmt.Errorf("finding the installed gt: %w", err)
	}
	out, exit, err := p.env.Run(ctx, "", nil, exe, "version")
	if err != nil {
		return "", fmt.Errorf("running %s version: %w", exe, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("%s version exited %d: %s", exe, exit, out)
	}
	line, _, _ := strings.Cut(out, "\n")
	fields := strings.Fields(line)
	if len(fields) < 3 || fields[0] != "gt" || fields[1] != "version" {
		return "", fmt.Errorf("%s version named no commit: %s", exe, out)
	}
	return fields[2], nil
}

// DaemonVersion is the running daemon's commit. A state with no commit cannot
// prove which build is running.
func (p *verifyProbes) DaemonVersion(context.Context) (string, error) {
	commit, err := p.env.DaemonCommit()
	if err != nil {
		return "", fmt.Errorf("reading the daemon state: %w", err)
	}
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return "", errors.New("the daemon state named no commit")
	}
	return commit, nil
}

// ImageLabel is the commit label of the sandbox image. With a values file, the
// image is the one the file names, and its label must be the file's commit: a
// label that differs is an error naming both. A values file that exists but
// cannot be read is an error too, not a fall back to verifyImage.
func (p *verifyProbes) ImageLabel(ctx context.Context) (string, error) {
	image := verifyImage
	var values imagevalues.Values
	haveValues := false
	if p.env.ValuesDir != nil {
		v, ok, err := imagevalues.LoadIn(p.env.ValuesDir())
		if err != nil {
			return "", fmt.Errorf("reading the values file: %w", err)
		}
		if ok {
			values, haveValues, image = v, true, v.Polecat
		}
	}
	out, exit, err := p.env.Run(ctx, "", nil, "docker", "image", "inspect", image, "--format", verifyImageLabelFormat)
	if err != nil {
		return "", fmt.Errorf("running docker image inspect %s: %w", image, err)
	}
	if exit != 0 {
		return "", fmt.Errorf("docker image inspect %s exited %d: %s", image, exit, out)
	}
	label := strings.TrimSpace(out)
	if haveValues && label != values.Commit {
		return "", fmt.Errorf("image %s is labeled commit %s, but the values file names commit %s", image, label, values.Commit)
	}
	return label, nil
}

// podImageLabelProbes is the run's verify probes for a pod target: the image
// label of the target pair read from the registry, the pair extension's two
// checks, and the sweep-launch check read from the daemon pod's record. The
// update Job runs in a pod with no docker, so the docker inspect ImageLabel
// makes cannot answer there.
type podImageLabelProbes struct {
	update.VerifyProbes
	label       func(context.Context) (string, error)
	wantPolecat string
	annotation  func(context.Context) (string, error)
	devTag      func(context.Context) (string, error)
	// sweep reads the daemon pod's launch-smoke record, sweepStart is the running
	// daemon's start the record is judged against, and sweepWait waits one poll
	// between reads of a missing record, returning ctx's error once ctx ends.
	sweep      func(context.Context) (daemon.SweepSmokeRecord, bool, error)
	sweepStart func() (time.Time, error)
	sweepWait  func(context.Context) error
}

var _ update.PairProbes = podImageLabelProbes{}
var _ update.SweepProbes = podImageLabelProbes{}

// ImageLabel is the commit label of the target pair's polecat image, from the
// registry. A run with no registry read wired is an error, never a pass.
func (p podImageLabelProbes) ImageLabel(ctx context.Context) (string, error) {
	if p.label == nil {
		return "", errors.New("the run has no image label reader")
	}
	return p.label(ctx)
}

// WantPolecat is the polecat digest the --images file names, and empty for a run
// that names no file, where both pair checks fail.
func (p podImageLabelProbes) WantPolecat() string { return p.wantPolecat }

// PolecatAnnotation is the digest of the Deployment's polecat-image annotation.
// A run with no reader wired is an error, never a pass.
func (p podImageLabelProbes) PolecatAnnotation(ctx context.Context) (string, error) {
	if p.annotation == nil {
		return "", errors.New("the run has no Deployment annotation reader")
	}
	return p.annotation(ctx)
}

// DevTagDigest is the digest the sandbox registry's dev tag names. A run with no
// reader wired is an error, never a pass.
func (p podImageLabelProbes) DevTagDigest(ctx context.Context) (string, error) {
	if p.devTag == nil {
		return "", errors.New("the run has no dev tag reader")
	}
	return p.devTag(ctx)
}

// podTargetAnnotation reads the Deployment's polecat-image annotation through the
// swap's running pair read, so verify and the install that restores a pair read
// the one command.
func podTargetAnnotation(target podDaemonTarget, run update.RunFunc) func(context.Context) (string, error) {
	swap := podDaemonSwap{
		Context:    target.Context,
		Namespace:  target.Namespace,
		InCluster:  target.InCluster,
		Deployment: target.Deployment,
		Run:        run,
	}
	return func(ctx context.Context) (string, error) {
		pair, err := swap.RunningPair(ctx)
		if err != nil {
			return "", err
		}
		return podRefDigest(pair.Polecat), nil
	}
}

// podTargetDevTag reads the sandbox registry's dev tag through the pair check's
// read, gt daemon polecat-devtag in the daemon pod: only that pod reaches the
// registry.
func podTargetDevTag(target podDaemonTarget, run update.RunFunc) func(context.Context) (string, error) {
	check := podPairCheck{Run: run, Target: target}
	return func(ctx context.Context) (string, error) {
		read, err := check.readPodDevTag(ctx)
		if err != nil {
			return "", err
		}
		if !read.Present {
			return "", errors.New("the sandbox registry holds no dev tag for the polecat image")
		}
		return read.Digest, nil
	}
}

const (
	// podSweepSmokeReadScript prints the record when its file exists and exits
	// podSweepSmokeAbsent when not. The path is $1, never spliced in.
	podSweepSmokeReadScript = `if [ -f "$1" ]; then cat "$1"; else exit 3; fi`
	podSweepSmokeAbsent     = 3
)

// podSweepSmokeRecord reads and decodes the daemon's launch-smoke record in the
// daemon pod through run (gt-r2497d). A missing record is not an error. No error
// names the output, which can hold a secret.
func podSweepSmokeRecord(target podDaemonTarget, run update.RunFunc) func(context.Context) (daemon.SweepSmokeRecord, bool, error) {
	path := filepath.Join(constants.TownRuntimePath(k8sdaemon.TownRootPath), "tier-sweep", "launch-smoke.json")
	return func(ctx context.Context) (daemon.SweepSmokeRecord, bool, error) {
		ctx, cancel := context.WithTimeout(ctx, podProbeBound)
		defer cancel()
		argv := append(podKubectlPrefix(target.Context, target.Namespace, target.InCluster),
			"exec", "deployment/"+target.Deployment, "-c", k8sdaemon.Name, "--", "sh", "-c", podSweepSmokeReadScript, "sh", path)
		out, exit, err := run(ctx, "", nil, "kubectl", argv...)
		if err != nil {
			return daemon.SweepSmokeRecord{}, false, fmt.Errorf("pod sweep smoke record: running kubectl exec in deployment %s: %w", target.Deployment, err)
		}
		if exit == podSweepSmokeAbsent {
			return daemon.SweepSmokeRecord{}, false, nil
		}
		if exit != 0 {
			return daemon.SweepSmokeRecord{}, false, fmt.Errorf("pod sweep smoke record: kubectl exec in deployment %s exited %d", target.Deployment, exit)
		}
		var rec daemon.SweepSmokeRecord
		if err := json.Unmarshal([]byte(out), &rec); err != nil {
			return daemon.SweepSmokeRecord{}, false, fmt.Errorf("pod sweep smoke record: the record in deployment %s is not JSON", target.Deployment)
		}
		return rec, true, nil
	}
}

// podSweepRecordWait bounds the wait for the daemon's launch-smoke record: it
// writes it from its start goroutine, so one still missing after the bound is not written.
const podSweepRecordWait = 5 * time.Minute

// podSweepRecordPoll is the interval between reads of a missing record.
const podSweepRecordPoll = 10 * time.Second

// podSweepPoll waits one poll interval between reads of a missing record, and
// returns ctx's error when ctx ends first.
func podSweepPoll(ctx context.Context) error {
	t := time.NewTimer(podSweepRecordPoll)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// SweepLaunch answers whether the daemon's sweeper started a stage, from the
// launch-smoke record in the daemon pod. A record missing at a read is read again
// after each poll until the wait ends: the context's end answers not started, and
// a wait that fails for any other reason is an error.
func (p podImageLabelProbes) SweepLaunch(ctx context.Context) (bool, string, error) {
	if p.sweep == nil || p.sweepStart == nil || p.sweepWait == nil {
		return false, "", errors.New("the run has no launch-smoke record reader")
	}
	ctx, cancel := context.WithTimeout(ctx, podSweepRecordWait)
	defer cancel()
	start, err := p.sweepStart()
	if err != nil {
		return false, "", fmt.Errorf("reading the daemon's start: %w", err)
	}
	for {
		rec, exists, err := p.sweep(ctx)
		if err != nil {
			return false, "", err
		}
		if exists {
			return sweepLaunchAnswer(rec, start)
		}
		err = p.sweepWait(ctx)
		if err != nil && !errors.Is(err, ctx.Err()) {
			return false, "", fmt.Errorf("waiting for the launch-smoke record: %w", err)
		}
		// The context check ends the loop even when the wait returns nil after the context ends.
		if err != nil || ctx.Err() != nil {
			return false, "no launch-smoke record in the daemon pod before the wait ended", nil
		}
	}
}

// daemonStateStart is the running daemon's start from the town's daemon state.
func daemonStateStart() (time.Time, error) {
	return daemonStartFrom(workspace.FindFromCwdOrError, daemon.LoadState)
}

// daemonStartFrom reads the daemon's start through the town root and state
// loader it is given. A state with no start cannot tell this daemon's record from
// an earlier one, so it is an error: a zero start would judge every record current.
func daemonStartFrom(findTownRoot func() (string, error), loadState func(string) (*daemon.State, error)) (time.Time, error) {
	townRoot, err := findTownRoot()
	if err != nil {
		return time.Time{}, err
	}
	state, err := loadState(townRoot)
	if err != nil {
		return time.Time{}, err
	}
	if state.StartedAt.IsZero() {
		return time.Time{}, errors.New("the daemon state names no start")
	}
	return state.StartedAt, nil
}

// sweepLaunchAnswer judges a record the daemon wrote. The daemon stamps its
// record a moment after it saves its state's start, so this daemon's record is
// never before that start; a record before it is from an earlier daemon. A
// busy launch names no Job: the record carries none.
func sweepLaunchAnswer(rec daemon.SweepSmokeRecord, start time.Time) (bool, string, error) {
	if rec.DaemonStart.Before(start) {
		return false, fmt.Sprintf("the launch-smoke record is from the daemon start %s, before this daemon's start %s", rec.DaemonStart.Format(time.RFC3339), start.Format(time.RFC3339)), nil
	}
	switch rec.Outcome {
	case "started":
		return true, "a sweep stage launched against main", nil
	case "busy":
		return true, "busy: a sweep Job of the rig has not ended (the record names no Job)", nil
	case "declined":
		return false, "declined by the capacity preflight", nil
	case "failed":
		if rec.Detail == "" {
			return false, "the sweep launch failed", nil
		}
		return false, "the sweep launch failed: " + rec.Detail, nil
	}
	return false, "", errors.New("the launch-smoke record names no known outcome")
}

// podTargetImageLabel reads the commit label of the polecat image the values file
// at imagesPath names, through the town's registry reader, in process: its only
// inputs are the registry seams, the town root and the images file, and no
// command runner is among them. The registry answers the manifest and config bytes, and the reader checks each
// against the digest it was asked by. The town copy of the values file is not
// read: until the Job's own checks pass it names the previous pair.
func podTargetImageLabel(seams updateImagesSeams, townRoot, imagesPath string) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		values, err := imagevalues.Load(imagesPath)
		if err != nil {
			return "", fmt.Errorf("the --images values file: %w", err)
		}
		reader, err := townRegistryReader(seams, townRoot)
		if err != nil {
			return "", err
		}
		digest := podRefDigest(values.Polecat)
		if want := reader.registry.Ref(imagevalues.PolecatImage, digest); values.Polecat != want {
			return "", fmt.Errorf("the values file's polecat image %s is not %s in the town's registry", values.Polecat, want)
		}
		polecat, err := reader.image(ctx, "polecat", imagevalues.PolecatImage, digest)
		if err != nil {
			return "", fmt.Errorf("reading the target pair's polecat image: %w", err)
		}
		if polecat.Digest != digest {
			return "", fmt.Errorf("the registry answered polecat digest %s for the digest %s it was asked for", polecat.Digest, digest)
		}
		if polecat.Commit != values.Commit {
			return "", fmt.Errorf("image %s is labeled commit %s, but the values file names commit %s", polecat.Repo, polecat.Commit, values.Commit)
		}
		return polecat.Commit, nil
	}
}

// townRegistryReader is the registry reader the town's images are read with: the
// registry the town's Forgejo names and the viewer credential. A seam that is not
// wired is an error, never a read made without it.
func townRegistryReader(seams updateImagesSeams, townRoot string) (registryReader, error) {
	switch {
	case seams.Registry == nil:
		return registryReader{}, errors.New("no registry is wired")
	case seams.Credentials == nil:
		return registryReader{}, errors.New("no credential reader is wired")
	case seams.HTTPClient == nil:
		return registryReader{}, errors.New("no HTTP client is wired")
	}
	registry, err := seams.Registry(townRoot)
	if err != nil {
		return registryReader{}, err
	}
	if err := registry.Validate(); err != nil {
		return registryReader{}, err
	}
	user, token, err := seams.Credentials()
	if err != nil {
		return registryReader{}, fmt.Errorf("reading the viewer credential: %w", err)
	}
	return registryReader{
		base:          registry.BaseURL(),
		registry:      registry,
		plaintextHost: plaintextHostOf(registry),
		http:          seams.HTTPClient,
		user:          user,
		token:         token,
	}, nil
}

// Health is the town's first health line. gt status --line exits nonzero for a
// degraded town and its line still describes it, so the exit status is
// ignored.
//
// A command that prints no line says nothing about the town, so it is an error.
func (p *verifyProbes) Health(ctx context.Context) (string, error) {
	exe, err := p.env.Exe()
	if err != nil {
		return "", fmt.Errorf("finding the installed gt: %w", err)
	}
	out, _, err := p.env.Run(ctx, "", nil, exe, "status", "--line")
	if err != nil {
		return "", fmt.Errorf("running %s status --line: %w", exe, err)
	}
	line, _, _ := strings.Cut(out, "\n")
	if strings.TrimSpace(line) == "" {
		return "", fmt.Errorf("%s status --line printed no health line", exe)
	}
	return strings.TrimSpace(line), nil
}

// Ready is the town's readiness answer from the installed gt: `status --line
// --ready` exits 0 when the town can take traffic, and otherwise exits the
// verdict's code, 1 degraded, 2 red or 3 unknown. A nonzero exit is not ready,
// with the reason the command prints to stderr after the health line; when Run
// returns no text there, the reason names the health line and the exit status,
// or the exit status alone when it printed no line. A command that cannot run,
// and an exit code the town never writes, are errors naming the executable, so
// a probe that could not ask never reports the town's verdict.
func (p *verifyProbes) Ready(ctx context.Context) (bool, string, error) {
	exe, err := p.env.Exe()
	if err != nil {
		return false, "", fmt.Errorf("finding the installed gt: %w", err)
	}
	out, exit, err := p.env.Run(ctx, "", nil, exe, "status", "--line", "--ready")
	if err != nil {
		return false, "", fmt.Errorf("running %s status --line --ready: %w", exe, err)
	}
	if exit == 0 {
		return true, "", nil
	}
	if exit < 1 || exit > 3 {
		return false, "", fmt.Errorf("%s status --line --ready exited %d", exe, exit)
	}
	line, rest, _ := strings.Cut(out, "\n")
	if reason := strings.TrimSpace(rest); reason != "" {
		return false, reason, nil
	}
	if line = strings.TrimSpace(line); line == "" {
		return false, fmt.Sprintf("exit %d", exit), nil
	}
	return false, fmt.Sprintf("%s (exit %d)", line, exit), nil
}
