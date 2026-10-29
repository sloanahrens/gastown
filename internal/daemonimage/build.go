// Package daemonimage builds the gastown daemon's container artifacts on the
// host: the linux/arm64 gt, bd and om binaries the image runs (gt-qpeei.9), and
// the image built from them (gt-qpeei.16). Every command it runs is one injected
// call, so the gate proves what a build runs without Docker, a running daemon or
// a cluster.
package daemonimage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// DockerfilePath is the image's Dockerfile in the gt checkout, relative to the
// repo root. BuildImage takes it as an absolute path; this is the one it names
// for a caller reading the tree.
const DockerfilePath = "deploy/daemon/Dockerfile"

// DockerContext is the Docker CLI context every image build runs in: Docker
// Desktop's VM on this Mac. The flag is explicit so the operator's shell cannot
// swap the engine a build reaches by changing the global context.
const DockerContext = "desktop-linux"

// The package each binary is built from, inside its source tree. gt and bd are
// pinned to the polecat image flow's own sources: a test keeps GTPackage equal
// to the package polecat-image builds, and BDPackage to bdbuild.Package.
const (
	GTPackage = "./cmd/gt"
	BDPackage = "./cmd/bd"
	OMPackage = "./cmd/om"
)

// The -X stamps the gt and bd binaries are built with. gt reports rev, the commit
// its image tag names: it falls back to the VCS stamp of the tree it was built
// in (internal/cmd.resolveCommitHash), so the stamp names rev outright. bd reports
// its commit the way bdbuild stamps it.
const (
	gtCommitFlag = "-X github.com/steveyegge/gastown/internal/cmd.Commit="
	bdCommitFlag = "-X main.Commit="
)

// Runner runs one argv in dir with env as the whole environment it runs with; it
// is the package's only way to run anything.
type Runner func(ctx context.Context, dir string, env, argv []string) error

// Builder builds the daemon image's binaries.
type Builder struct {
	// Run runs every build.
	Run Runner
	// Git resolves the commits the binaries are built from, and makes the clean
	// worktrees they are built in.
	Git bdbuild.Git
	// Environ is the host environment the builds start from; nil reads the
	// host's. bdbuild.Env keeps the variables that locate the toolchain and drops
	// the ones that would change what is compiled.
	Environ []string
	// Out is the absolute directory the three binaries are written to. git and go
	// resolve a relative path against the worktree a build runs in, not the
	// caller's directory, so BuildBinaries refuses a relative Out. Out must
	// already exist: the staging directory is made inside it.
	Out string
	// GTSrc is the gt checkout gt is built from, at rev. The build runs in a clean
	// worktree of rev, so uncommitted changes in GTSrc never reach the binary.
	GTSrc string
	// BDSrc is the beads checkout the image's bd is built from: the BD_SRC the
	// polecat image takes.
	BDSrc string
	// BDCommit is the beads commit the image's bd is built from and stamped with:
	// the commit bdbuild.Commit picks for the polecat image.
	BDCommit string
	// OMSrc is the om checkout the image's om is built from: the clone the
	// rebuild_om job keeps at <town>/.runtime/om-src. om is built from its HEAD,
	// in a clean worktree of it.
	OMSrc string
	// Dockerfile is the file BuildImage builds the image from: DockerfilePath in
	// the gt checkout, as an absolute path. It must be absolute for the same
	// reason Out must: docker resolves -f against the directory it runs in.
	Dockerfile string
}

// ImageTag is the tag the daemon image for rev is built with: gt-daemon:<rev>.
// It is a local tag only; nothing pulls this image by name.
func ImageTag(rev string) string { return "gt-daemon:" + rev }

// binary is one of the three builds: the checkout it is built from, the commit
// of that checkout it is built at, and the package and stamp it is built with.
type binary struct {
	name, src, commit, pkg, ldflags string
}

// BuildBinaries writes gt, bd and om to b.Out and returns their paths in that
// order. Every refusal comes before the first build runs, so a refused call
// writes nothing. Each binary is built in a clean worktree of its commit and is
// staged under b.Out until all three have built, so a build that fails returns
// its error and no paths, and leaves nothing in b.Out.
func (b Builder) BuildBinaries(ctx context.Context, rev string) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	if b.Out == "" {
		return nil, errors.New("daemonimage: no output directory for the binaries")
	}
	if !filepath.IsAbs(b.Out) {
		return nil, fmt.Errorf("daemonimage: output directory %q is not absolute; a build runs in a worktree, not in the caller's directory", b.Out)
	}
	if rev = strings.TrimSpace(rev); rev == "" {
		return nil, errors.New("daemonimage: no rev to stamp gt with")
	}
	if !isCommit(rev) {
		return nil, fmt.Errorf("daemonimage: rev %q is not a 7+ character hex commit", rev)
	}
	for _, src := range []struct{ name, dir string }{{"gt", b.GTSrc}, {"bd", b.BDSrc}, {"om", b.OMSrc}} {
		if src.dir == "" {
			return nil, fmt.Errorf("daemonimage: no %s source checkout named", src.name)
		}
	}
	bdCommit := strings.TrimSpace(b.BDCommit)
	if bdCommit == "" {
		return nil, errors.New("daemonimage: no bd commit to stamp bd with")
	}
	if !isCommit(bdCommit) {
		return nil, fmt.Errorf("daemonimage: bd commit %q is not a 7+ character hex commit", bdCommit)
	}
	gtCommit, err := b.resolve(b.GTSrc, rev)
	if err != nil {
		return nil, err
	}
	bdCommit, err = b.resolve(b.BDSrc, bdCommit)
	if err != nil {
		return nil, err
	}
	omCommit, err := b.resolve(b.OMSrc, "HEAD")
	if err != nil {
		return nil, err
	}
	builds := []binary{
		{name: "gt", src: b.GTSrc, commit: gtCommit, pkg: GTPackage, ldflags: gtCommitFlag + gtCommit},
		{name: "bd", src: b.BDSrc, commit: bdCommit, pkg: BDPackage, ldflags: bdCommitFlag + bdCommit},
		{name: "om", src: b.OMSrc, commit: omCommit, pkg: OMPackage},
	}
	stage, err := os.MkdirTemp(b.Out, ".daemonimage-")
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stage the binaries in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	env := bdbuild.Env(b.Environ)
	for _, build := range builds {
		if err := b.build(ctx, build, stage, env); err != nil {
			return nil, err
		}
	}
	paths := make([]string, 0, len(builds))
	for _, build := range builds {
		path := filepath.Join(b.Out, build.name)
		if err := os.Rename(filepath.Join(stage, build.name), path); err != nil {
			return nil, fmt.Errorf("daemonimage: cannot move %s into %s: %w", build.name, b.Out, err)
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// build compiles one binary to stage/<name> at build.commit. The clean worktree
// keeps uncommitted changes out of the binary, and it is removed whether or not
// the compile succeeds.
func (b Builder) build(ctx context.Context, build binary, stage string, env []string) error {
	dir := filepath.Join(stage, build.name+"-src")
	if _, err := b.Git(build.src, "worktree", "add", "--detach", dir, build.commit); err != nil {
		return fmt.Errorf("daemonimage: cannot check commit %s out of %s: %w", build.commit, build.src, err)
	}
	defer func() { _, _ = b.Git(build.src, "worktree", "remove", "--force", dir) }()
	argv := []string{"go", "build", "-trimpath"}
	if build.ldflags != "" {
		argv = append(argv, "-ldflags", build.ldflags)
	}
	argv = append(argv, "-o", filepath.Join(stage, build.name), build.pkg)
	if err := b.Run(ctx, dir, env, argv); err != nil {
		return fmt.Errorf("daemonimage: building %s from %s failed: %w", build.name, build.pkg, err)
	}
	return nil
}

// resolve returns the full sha commit names in src, and refuses one src does
// not hold, so a stamp never names a commit the build did not read.
func (b Builder) resolve(src, commit string) (string, error) {
	out, err := b.Git(src, "rev-parse", "--verify", commit+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("daemonimage: %s does not hold commit %s: %w", src, commit, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// isCommit reports whether s has the shape of an abbreviated or full commit, so
// git never reads it as an option or a revision expression.
func isCommit(s string) bool {
	if len(s) < 7 || len(s) > 40 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// BuildImage builds the daemon image from the binaries in b.Out and returns its
// tag, ImageTag(rev). The build context is b.Out itself, so each COPY in
// b.Dockerfile reads the binary BuildBinaries wrote there, and the image runs
// exactly the code rev names.
//
// Every refusal comes before docker runs, so a refused call starts no build. A
// build that fails returns an empty tag: a caller that logs and goes on never
// deploys an image this call did not make.
func (b Builder) BuildImage(ctx context.Context, rev string, bins []string) (string, error) {
	if b.Run == nil {
		return "", errors.New("daemonimage: no runner to build the image with")
	}
	rev = strings.TrimSpace(rev)
	if rev == "" {
		return "", errors.New("daemonimage: no rev to tag the image with")
	}
	if !isCommit(rev) {
		return "", fmt.Errorf("daemonimage: rev %q is not a 7+ character hex commit", rev)
	}
	if b.Out == "" {
		return "", errors.New("daemonimage: no build context for the image; Out is the directory the binaries are in")
	}
	if !filepath.IsAbs(b.Out) {
		return "", fmt.Errorf("daemonimage: build context %q is not absolute; docker resolves the context and -f against the directory it runs in", b.Out)
	}
	if b.Dockerfile == "" {
		return "", fmt.Errorf("daemonimage: no Dockerfile named; set Dockerfile to %s in the gt checkout", DockerfilePath)
	}
	if !filepath.IsAbs(b.Dockerfile) {
		return "", fmt.Errorf("daemonimage: Dockerfile %q is not absolute; docker resolves -f against the directory it runs in, not the caller's", b.Dockerfile)
	}
	if info, err := os.Stat(b.Dockerfile); err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("daemonimage: Dockerfile %s is not a file; docker would report it only after the base image pulled", b.Dockerfile)
	}
	if err := b.checkImageBinaries(bins); err != nil {
		return "", err
	}
	tag := ImageTag(rev)
	argv := []string{"docker", "--context", DockerContext, "build", "-f", b.Dockerfile, "-t", tag, b.Out}
	if err := b.Run(ctx, b.Out, dockerEnv(b.Environ), argv); err != nil {
		return "", fmt.Errorf("daemonimage: building image %s failed: %w", tag, err)
	}
	return tag, nil
}

// checkImageBinaries refuses a set of binaries the Dockerfile's COPY lines could
// not read: gt, bd and om once each, as files in the build context. Docker would
// report a missing or misplaced one only after the package install ran, so it is
// caught here first, against what the caller passed.
func (b Builder) checkImageBinaries(bins []string) error {
	want := []string{"gt", "bd", "om"}
	seen := make(map[string]bool, len(want))
	for _, p := range bins {
		name := filepath.Base(p)
		if !slices.Contains(want, name) {
			return fmt.Errorf("daemonimage: %q is not one of the image's binaries gt, bd and om", p)
		}
		if filepath.Dir(filepath.Clean(p)) != filepath.Clean(b.Out) {
			return fmt.Errorf("daemonimage: %q is outside the build context %s, so the Dockerfile's COPY %s reads nothing", p, b.Out, name)
		}
		if seen[name] {
			return fmt.Errorf("daemonimage: %s is listed twice among the binaries", name)
		}
		if info, err := os.Stat(p); err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("daemonimage: %s is not a file in %s; build the binaries there first", p, b.Out)
		}
		seen[name] = true
	}
	for _, name := range want {
		if !seen[name] {
			return fmt.Errorf("daemonimage: no %s binary among the %d given; the image copies gt, bd and om", name, len(bins))
		}
	}
	return nil
}

// dockerEnvKeys names the host variables the image build runs with: those that
// find the docker CLI and read its config, plus the proxy and CA settings a pull
// through a proxy needs. bdbuild.Env's compile settings stay out, and so do
// DOCKER_CONTEXT and DOCKER_HOST: the build names its context in argv, so a
// shell's choice of engine cannot reach it.
var dockerEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
}

// dockerEnv is the environment the image build runs with, taken from environ in
// its own order. A nil environ reads the host's.
func dockerEnv(environ []string) []string {
	if environ == nil {
		environ = os.Environ()
	}
	env := make([]string, 0, len(environ))
	for _, kv := range environ {
		if key, _, ok := strings.Cut(kv, "="); ok && dockerEnvKeys[key] {
			env = append(env, kv)
		}
	}
	return env
}
