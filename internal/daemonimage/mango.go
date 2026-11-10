package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
)

// The one builder image the mango compile runs in, the platform the daemon runs
// on, and the module directory and package inside the mango checkout. mango's
// go-api links DuckDB through cgo, so the image's mango is compiled on linux in
// a container rather than cross-compiled on the host as gt, bd and om are.
const (
	mangoBuildImage = "golang:1.27-bookworm"
	mangoPlatform   = "linux/arm64"
	mangoGoAPI      = "go-api"
	mangoPackage    = "./cmd/mango"
)

// mangoCompile is what the builder container runs: gcc and g++ for cgo, then the
// linux build of mango into /out. -buildvcs=false keeps the go command from
// reading the worktree's .git, which names a gitdir the container does not
// mount; the commit reaches the image through the caller and the mango.sha
// stamp instead of VCS stamping.
const mangoCompile = mangoAptInstall + ` && ` + mangoGoBuild

// mangoAptInstall and mangoGoBuild are the two commands of mangoCompile. The
// images workflow builds the same mango from deploy/daemon/mango.Dockerfile, whose
// two RUN lines are these strings; TestMangoDockerfile holds them equal.
const (
	mangoAptInstall = `apt-get update && apt-get install -y --no-install-recommends gcc g++`
	mangoGoBuild    = `CGO_ENABLED=1 GOOS=linux go build -trimpath -buildvcs=false -ldflags="-w -s" -o /out/mango ` + mangoPackage
)

// mangoDir is the directory under the build context the image's COPY reads mango
// and its commit stamp from.
const mangoDir = "mango"

// mangoNames are the two files BuildMango publishes in b.Out/mango, in the order
// it returns them: the binary and the commit it was built from.
var mangoNames = []string{"mango", "mango.sha"}

// mangoArgv is the docker run that compiles mango, mounting worktree at /src and
// stage at /out: the same argv BuildMango runs and Plan prints.
func mangoArgv(worktree, stage string) []string {
	return []string{
		"docker", "--context", DockerContext, "run", "--rm",
		"--platform", mangoPlatform,
		"-v", worktree + ":/src",
		"-v", stage + ":/out",
		"-w", "/src/" + mangoGoAPI,
		mangoBuildImage,
		"sh", "-c", mangoCompile,
	}
}

// planMangoWorktree and planMangoStage stand in for the two paths Plan cannot
// know because BuildMango makes them fresh. Each is one shell-safe token, since
// Plan prints it inside a command line.
const (
	planMangoWorktree = "mango_worktree"
	planMangoStage    = "mango_stage"
)

// checkMangoInputs refuses the inputs BuildMango cannot build from, before
// anything runs, and returns MangoCommit trimmed.
func (b Builder) checkMangoInputs() (string, error) {
	if b.Out == "" {
		return "", errors.New("daemonimage: no output directory for mango")
	}
	if !filepath.IsAbs(b.Out) {
		return "", fmt.Errorf("daemonimage: output directory %q is not absolute; a build runs in a worktree, not in the caller's directory", b.Out)
	}
	if strings.TrimSpace(b.MangoSrc) == "" {
		return "", errors.New("daemonimage: --mango-src is required: mango is built from that checkout")
	}
	commit := strings.TrimSpace(b.MangoCommit)
	if commit == "" {
		return "", errors.New("daemonimage: no mango commit to build at: pass --mango-commit")
	}
	if !isCommit(commit) {
		return "", fmt.Errorf("daemonimage: mango commit %q is not a 7+ character hex commit", commit)
	}
	for _, p := range []struct{ name, dir string }{{"output directory", b.Out}, {"mango source", b.MangoSrc}} {
		if err := refuseCanonicalClone(p.name, p.dir); err != nil {
			return "", err
		}
	}
	return commit, nil
}

// BuildMango builds the image's mango and its commit stamp into b.Out/mango and
// returns their two paths.
func (b Builder) BuildMango(ctx context.Context) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build mango with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the mango commit with")
	}
	commit, err := b.checkMangoInputs()
	if err != nil {
		return nil, err
	}
	commit, err = b.resolve(b.MangoSrc, commit)
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(b.Out, ".daemonimage-mango-")
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stage mango in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	worktree := filepath.Join(stage, "mango-src")
	if _, err := b.Git(b.MangoSrc, "worktree", "add", "--detach", worktree, commit); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot check commit %s out of %s: %w", commit, b.MangoSrc, err)
	}
	defer func() { _, _ = b.Git(b.MangoSrc, "worktree", "remove", "--force", worktree) }()
	if err := b.Run(ctx, worktree, dockerEnv(b.Environ), mangoArgv(worktree, stage)); err != nil {
		return nil, fmt.Errorf("daemonimage: building mango from %s at %s failed: %w", mangoPackage, commit, err)
	}
	built := filepath.Join(stage, "mango")
	info, err := os.Stat(built)
	if err != nil {
		return nil, fmt.Errorf("daemonimage: the mango build wrote no binary to %s: %w", built, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("daemonimage: the mango build wrote %s, which is not a regular file", built)
	}
	return b.publishMango(commit, built, stage)
}

// publishMango moves the built mango and its commit stamp into b.Out/mango and
// puts back the directory a previous build published when the final move fails,
// so a caller never sees a partial b.Out/mango. The directory is assembled in
// the staging directory first, so b.Out/mango does not change until it is whole.
func (b Builder) publishMango(commit, built, stage string) ([]string, error) {
	ops := b.fileOps()
	out := filepath.Join(stage, "mango-out")
	if err := os.Mkdir(out, 0o755); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot make %s: %w", out, err)
	}
	if err := ops.rename(built, filepath.Join(out, "mango")); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot move mango into %s: %w", out, err)
	}
	if err := atomicfile.WriteFile(filepath.Join(out, "mango.sha"), []byte(commit+"\n"), 0o644); err != nil {
		return nil, fmt.Errorf("daemonimage: cannot write the mango commit stamp in %s: %w", out, err)
	}
	target := filepath.Join(b.Out, mangoDir)
	previous := filepath.Join(stage, "previous-mango")
	hadPrevious := false
	if _, err := ops.lstat(target); err == nil {
		if err := ops.rename(target, previous); err != nil {
			return nil, fmt.Errorf("daemonimage: cannot move the mango directory already in %s aside: %w", b.Out, err)
		}
		hadPrevious = true
	} else if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("daemonimage: cannot read %s: %w", target, err)
	}
	if err := ops.rename(out, target); err != nil {
		if !hadPrevious {
			return nil, fmt.Errorf("daemonimage: cannot move mango into %s: %w", b.Out, err)
		}
		if undoErr := ops.rename(previous, target); undoErr != nil {
			return nil, fmt.Errorf("daemonimage: cannot move mango into %s: %w; cannot put the previous mango directory back: %w", b.Out, err, undoErr)
		}
		return nil, fmt.Errorf("daemonimage: cannot move mango into %s: %w", b.Out, err)
	}
	return []string{filepath.Join(target, "mango"), filepath.Join(target, "mango.sha")}, nil
}
