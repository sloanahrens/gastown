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
