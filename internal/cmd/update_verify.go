package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

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

// podImageLabelProbes is the run's verify probes with the image label of a pod
// target read from the registry. The update Job runs in a pod with no docker, so
// the docker inspect ImageLabel makes cannot answer there.
type podImageLabelProbes struct {
	update.VerifyProbes
	label func(context.Context) (string, error)
}

// ImageLabel is the commit label of the target pair's polecat image, from the
// registry.
func (p podImageLabelProbes) ImageLabel(ctx context.Context) (string, error) {
	return p.label(ctx)
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
