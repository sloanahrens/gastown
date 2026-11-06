// Package daemonimage builds the gastown daemon's container artifacts on the
// host: the linux/arm64 gt, bd and om binaries the image runs (gt-qpeei.9), the
// image's cgo-built mango (gt-wznahy), the pinned claude CLI om's reviewer runs
// (gt-qpeei.35), and the image built from them (gt-qpeei.16). Every command it
// runs and every download it makes is one injected call, so the gate proves what
// a build runs without Docker, a running daemon, a cluster or the network.
package daemonimage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/buildstamp"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
	"github.com/steveyegge/gastown/internal/templates"
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

// The -X stamps the gt and bd binaries are built with. gt reports the short form
// of the commit it was built from, the form make install stamps and make
// polecat-image tags the image with: the tier sweep refuses an installed gt that
// records the full sha (gt-c3sue). It falls back to the VCS stamp of the tree it
// was built in (internal/cmd.resolveCommitHash), so the stamp names the short
// commit outright. bd reports the two halves of its build id the way bdbuild
// stamps them: main.Build, git's abbreviation of the commit in the worktree, then
// main.Commit, the full sha, so both images' bd report one build id (gt-glsx8).
const (
	gtCommitFlag = "-X github.com/steveyegge/gastown/internal/cmd.Commit="
	bdBuildFlag  = "-X main.Build="
	bdCommitFlag = "-X main.Commit="
)

// planCommitStamp returns the stamp Plan prints for the commit the caller
// named. That name may be abbreviated already; checkBuildInputs has refused
// anything that is not 7-40 lowercase hex, so a name shorter than the stamp is
// recorded whole rather than padded (gt-gg9hv.20) and a longer one is cut to
// the width buildstamp records.
func planCommitStamp(commit string) string {
	if len(commit) <= buildstamp.Len {
		return commit
	}
	return commit[:buildstamp.Len]
}

// Runner runs one argv in dir with env as the whole environment it runs with; it
// is the package's only way to run anything.
type Runner func(ctx context.Context, dir string, env, argv []string) error

// Fetcher downloads url and returns its body, which the caller closes; it is the
// package's only way to reach the network, as Runner is the only way to run a
// command. A test stands a fake in for it.
type Fetcher func(ctx context.Context, url string) (io.ReadCloser, error)

// Builder builds the daemon image's binaries.
type Builder struct {
	// Run runs every build.
	Run Runner
	// Git resolves the commits the binaries are built from, and makes the clean
	// worktrees they are built in.
	Git bdbuild.Git
	// Go runs go version -m on the release-reporter binary BuildTools builds, to
	// read the version the go command stamped into it.
	Go bdbuild.Go
	// Environ is the host environment the builds start from; nil reads the
	// host's. bdbuild.Env keeps the variables that locate the toolchain and drops
	// the ones that would change what is compiled.
	Environ []string
	// Out is the absolute directory the binaries are written to, and the claude
	// CLI FetchClaudeCLI places there. Git and go resolve a relative path against
	// the worktree a build runs in, not the caller's directory, so BuildBinaries
	// refuses a relative Out. Out must already exist: the staging directory is
	// made inside it.
	Out string
	// GTSrc is the gt checkout gt is built from, at rev. The build runs in a clean
	// worktree of rev, so uncommitted changes in GTSrc never reach the binary.
	GTSrc string
	// BDSrc is the beads checkout the image's bd is built from: the BD_SRC the
	// polecat image takes. An empty BDSrc (and an empty OMSrc, HMSrc or
	// DevopsSrc) fetches that source from its pin in Pins instead, so a build
	// needs no local checkout of it.
	BDSrc string
	// BDCommit is the beads commit the image's bd is built from and stamped with:
	// the commit bdbuild.Commit picks for the polecat image.
	BDCommit string
	// OMSrc is the om checkout the image's om is built from: the clone the
	// rebuild_om job keeps at <town>/.runtime/om-src. om is built at OMCommit in
	// a clean worktree of it, never at the checkout's HEAD.
	OMSrc string
	// OMCommit is the om commit the image's om is built from: the om pin.
	OMCommit string
	// HMSrc and HMCommit are the hm checkout BuildTools builds hm from, and the
	// commit it must be at. DevopsSrc and DevopsCommit are the devops checkout the
	// two reporters are built from, and its pinned commit.
	HMSrc, HMCommit         string
	DevopsSrc, DevopsCommit string
	// MangoSrc is the mango checkout the image's mango is built from, and
	// MangoCommit the commit it is built at. mango is compiled in a container of
	// mangoBuildImage, never cross-compiled on the host, because its go-api
	// module links DuckDB through cgo.
	MangoSrc, MangoCommit string
	// Pins and PinsPath are the image-pins file Plan names: the four pins the
	// commit fields above came from, and the file's path for the operator to edit.
	Pins     []Pin
	PinsPath string
	// Dockerfile is the file BuildImage builds the image from: DockerfilePath in
	// the gt checkout, as an absolute path. It must be absolute for the same
	// reason Out must: docker resolves -f against the directory it runs in.
	Dockerfile string
	// Fetch downloads the claude CLI FetchClaudeCLI places in Out; nil refuses
	// that fetch.
	Fetch Fetcher

	// The fields below are test seams for the filesystem calls the plugin and
	// mango moves and their rollbacks make; nil means the os function of the same
	// shape.
	rename func(oldpath, newpath string) error
	remove func(path string) error
	lstat  func(path string) (os.FileInfo, error)
}

// fileOps is the filesystem the plugin and mango moves publish and roll back
// with: the os functions of the same shape, or the test seams standing in for
// them.
type fileOps struct {
	rename func(oldpath, newpath string) error
	remove func(path string) error
	lstat  func(path string) (os.FileInfo, error)
}

// fileOps returns b's filesystem, with the os functions filling any seam b
// leaves nil.
func (b Builder) fileOps() fileOps {
	ops := fileOps{rename: os.Rename, remove: os.Remove, lstat: os.Lstat}
	if b.rename != nil {
		ops.rename = b.rename
	}
	if b.remove != nil {
		ops.remove = b.remove
	}
	if b.lstat != nil {
		ops.lstat = b.lstat
	}
	return ops
}

// ImageTag is the tag the daemon image for rev is built with: gt-daemon:<rev>.
// It is a local tag only; nothing pulls this image by name.
func ImageTag(rev string) string { return "gt-daemon:" + rev }

// binary is one of the three builds: the checkout it is built from, the commit
// of that checkout it is built at, and the package and stamp it is built with.
type binary struct {
	name, src, commit, pkg, ldflags string
	// stampsBuildID marks a build whose main.Build is stamped beside main.Commit
	// with git's abbreviation of the commit in its worktree, which only exists
	// once the build has made it.
	stampsBuildID bool
}

// plugin is one plugin the image carries: its directory name in the town's
// plugins/ and the package under the gt checkout it is built from.
type plugin struct{ name, pkg string }

// imagePlugins are the plugins the container daemon runs itself, in the order
// BuildPlugins builds them. The town's plugin directories hold macOS builds the
// container cannot exec, so the image carries its own linux/arm64 builds.
// polecat-image, tool-updater and hm-sync run only on the host and are not
// built here (gt-gg9hv.9).
var imagePlugins = []plugin{
	{name: "app-promote", pkg: "./plugins/app-promote"},
	{name: "town-metrics", pkg: "./plugins/town-metrics"},
}

// BuildBinaries writes gt, bd and om to b.Out and returns their paths in that
// order. Every refusal comes before the first build runs, so a refused call
// writes nothing. Each binary is built in a clean worktree of its commit and is
// staged under b.Out until all three have built, so a build that fails returns
// its error and no paths, and leaves nothing in b.Out. A bd or om checkout that
// is not named is fetched from its pin into b.Out first, and the fetched tree is
// removed when the call returns.
func (b Builder) BuildBinaries(ctx context.Context, rev string) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	rev, bdCommit, omCommit, err := b.checkBuildInputs(rev)
	if err != nil {
		return nil, err
	}
	gtCommit, err := b.resolve(b.GTSrc, rev)
	if err != nil {
		return nil, err
	}
	bdSrc, bdCleanup, err := b.source(ctx, "bd", b.BDSrc)
	if err != nil {
		return nil, err
	}
	defer bdCleanup()
	omSrc, omCleanup, err := b.source(ctx, "om", b.OMSrc)
	if err != nil {
		return nil, err
	}
	defer omCleanup()
	bdCommit, err = b.resolve(bdSrc, bdCommit)
	if err != nil {
		return nil, err
	}
	omCommit, err = b.resolve(omSrc, omCommit)
	if err != nil {
		return nil, err
	}
	gtStamp, err := buildstamp.Short(gtCommit)
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stamp gt with the commit resolved in %s: %w", b.GTSrc, err)
	}
	builds := []binary{
		{name: "gt", src: b.GTSrc, commit: gtCommit, pkg: GTPackage, ldflags: gtCommitFlag + gtStamp},
		{name: "bd", src: bdSrc, commit: bdCommit, pkg: BDPackage, ldflags: bdCommitFlag + bdCommit, stampsBuildID: true},
		{name: "om", src: omSrc, commit: omCommit, pkg: OMPackage},
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

// checkBuildInputs refuses the inputs BuildBinaries cannot build from, before
// anything runs, and returns rev and the bd and om commits trimmed. It reads
// nothing from the host, which is what lets Plan refuse exactly what a build
// would. A bd or om checkout that is absent is not refused: the build fetches
// that source from its pin.
func (b Builder) checkBuildInputs(rev string) (string, string, string, error) {
	rev, err := b.checkOutAndRev(rev, "the binaries")
	if err != nil {
		return "", "", "", err
	}
	if b.GTSrc == "" {
		return "", "", "", errors.New("daemonimage: no gt source checkout named")
	}
	for _, p := range []struct{ name, dir string }{{"output directory", b.Out}, {"gt source", b.GTSrc}, {"bd source", b.BDSrc}, {"om source", b.OMSrc}} {
		if p.dir == "" {
			continue // fetched from its pin; the fetch refuses a pin that cannot be used
		}
		if err := refuseCanonicalClone(p.name, p.dir); err != nil {
			return "", "", "", err
		}
	}
	bdCommit, err := checkPinnedCommit("bd", b.BDCommit, "to stamp bd with")
	if err != nil {
		return "", "", "", err
	}
	omCommit, err := checkPinnedCommit("om", b.OMCommit, "to build om at")
	if err != nil {
		return "", "", "", err
	}
	return rev, bdCommit, omCommit, nil
}

// checkPinnedCommit refuses a binary's commit when it is missing or not a
// commit, and returns it trimmed. what says why the build needs it, so the
// refusal reads as the fix.
func checkPinnedCommit(name, commit, what string) (string, error) {
	commit = strings.TrimSpace(commit)
	if commit == "" {
		return "", fmt.Errorf("daemonimage: no %s commit %s", name, what)
	}
	if !isCommit(commit) {
		return "", fmt.Errorf("daemonimage: %s commit %q is not a 7+ character hex commit", name, commit)
	}
	return commit, nil
}

// checkPluginInputs refuses the inputs BuildPlugins cannot build from, before
// anything runs, and returns rev trimmed. It reads nothing from the host.
func (b Builder) checkPluginInputs(rev string) (string, error) {
	rev, err := b.checkOutAndRev(rev, "the plugins")
	if err != nil {
		return "", err
	}
	if b.GTSrc == "" {
		return "", errors.New("daemonimage: no gt source checkout named")
	}
	for _, p := range []struct{ name, dir string }{{"output directory", b.Out}, {"gt source", b.GTSrc}} {
		if err := refuseCanonicalClone(p.name, p.dir); err != nil {
			return "", err
		}
	}
	return rev, nil
}

// checkOutAndRev refuses the output directory and rev both BuildBinaries and
// BuildPlugins build from, and returns rev trimmed. what names the build in the
// refusal: the binaries or the plugins.
func (b Builder) checkOutAndRev(rev, what string) (string, error) {
	if b.Out == "" {
		return "", fmt.Errorf("daemonimage: no output directory for %s", what)
	}
	if !filepath.IsAbs(b.Out) {
		return "", fmt.Errorf("daemonimage: output directory %q is not absolute; a build runs in a worktree, not in the caller's directory", b.Out)
	}
	if rev = strings.TrimSpace(rev); rev == "" {
		return "", errors.New("daemonimage: no rev to build from")
	}
	if !isCommit(rev) {
		return "", fmt.Errorf("daemonimage: rev %q is not a 7+ character hex commit", rev)
	}
	return rev, nil
}

// source returns the directory a source is built from, and a cleanup to run once
// the build is done. A checkout the caller named is used as it is and cleanup
// does nothing; an absent one is fetched from its pin into a fresh directory
// under Out, and cleanup removes the fetched tree, so a full checkout never stays
// in the build context.
func (b Builder) source(ctx context.Context, name, src string) (string, func(), error) {
	if src != "" {
		return src, func() {}, nil
	}
	pin, err := b.sourcePin(name)
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp(b.Out, ".daemonimage-src-"+name+"-")
	if err != nil {
		return "", nil, fmt.Errorf("daemonimage: cannot make a fetch directory for %s in %s: %w", name, b.Out, err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	if err := FetchSource(ctx, b.Run, b.Git, dir, b.Environ, pin); err != nil {
		cleanup()
		return "", nil, err
	}
	return dir, cleanup, nil
}

// sourcePin returns the pin a source with no checkout is fetched from, and
// refuses one it cannot fetch: no pin named at all, or a pin whose URL or commit
// a fetch cannot use. The pin is checked here, before Plan prints it, so a
// credential never reaches a plan line or an argv.
func (b Builder) sourcePin(name string) (Pin, error) {
	for _, p := range b.Pins {
		if p.Name == name {
			if err := checkFetchable(name, p); err != nil {
				return Pin{}, err
			}
			return p, nil
		}
	}
	return Pin{}, fmt.Errorf("daemonimage: no --%s-src and no %s pin in %s: name the checkout or pin the source", name, name, b.pinsFile())
}

// BuildPlugins writes the image's plugin binaries to b.Out/plugins/<name>/<name>
// and returns their paths in imagePlugins order. Each is built from a clean
// worktree of b.GTSrc at rev, so it runs the code the image's gt does. The
// staging and cleanup rules are BuildBinaries': every refusal comes before the
// first build, a failed build returns its error and no paths, and nothing is
// moved into b.Out until all of them have built. The move is all or nothing too:
// a plugin that cannot be placed takes every plugin this call already placed
// back out of b.Out and puts back each file it replaced, so a failed call leaves
// no partial plugin set behind.
func (b Builder) BuildPlugins(ctx context.Context, rev string) ([]string, error) {
	if b.Run == nil {
		return nil, errors.New("daemonimage: no runner to build the plugins with")
	}
	if b.Git == nil {
		return nil, errors.New("daemonimage: no git to resolve the commits with")
	}
	rev, err := b.checkPluginInputs(rev)
	if err != nil {
		return nil, err
	}
	gtCommit, err := b.resolve(b.GTSrc, rev)
	if err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(b.Out, ".daemonimage-plugins-")
	if err != nil {
		return nil, fmt.Errorf("daemonimage: cannot stage the plugins in %s: %w", b.Out, err)
	}
	defer os.RemoveAll(stage)
	env := bdbuild.Env(b.Environ)
	builds := make([]binary, 0, len(imagePlugins))
	for _, p := range imagePlugins {
		builds = append(builds, binary{name: p.name, src: b.GTSrc, commit: gtCommit, pkg: p.pkg})
	}
	for _, build := range builds {
		if err := b.build(ctx, build, stage, env); err != nil {
			return nil, err
		}
	}
	return b.movePlugins(builds, stage)
}

// pluginPlacement is one plugin binary's move: where it goes under b.Out, where
// the file that was already there was moved aside (empty when nothing was), and
// whether the staged binary reached the path.
type pluginPlacement struct {
	path, replaced string
	moved          bool
}

// pluginPlacements is what the move phase has put into b.Out so far: one entry
// per plugin it started, including the one that failed, and the directories it
// made. It exists so a move that fails part way can put b.Out back as it found
// it.
type pluginPlacements struct {
	items []pluginPlacement
	dirs  []string
}

// movePlugins moves each built plugin binary from stage to the directory under
// b.Out the image copies it from and returns their paths in builds order. A
// placement that fails takes back every placement before it, so a failed call
// leaves no partial plugin set in b.Out.
func (b Builder) movePlugins(builds []binary, stage string) ([]string, error) {
	ops := b.fileOps()
	var placed pluginPlacements
	for _, build := range builds {
		if err := b.placePlugin(build, stage, ops, &placed); err != nil {
			if undoErr := placed.undo(ops); undoErr != nil {
				return nil, fmt.Errorf("%w; %w", err, undoErr)
			}
			return nil, err
		}
	}
	paths := make([]string, 0, len(placed.items))
	for _, item := range placed.items {
		paths = append(paths, item.path)
	}
	return paths, nil
}

// placePlugin moves build's staged binary to b.Out/plugins/<name>/<name> and
// records the placement in placed. A regular file already at the path, a
// previous build's binary, is moved aside into stage first so undo can put it
// back; anything else at the path makes the binary's rename itself fail. The
// placement is recorded before either rename, so undo knows the path even when
// the move stops half way.
func (b Builder) placePlugin(build binary, stage string, ops fileOps, placed *pluginPlacements) error {
	dir := filepath.Join(b.Out, "plugins", build.name)
	for _, d := range []string{filepath.Join(b.Out, "plugins"), dir} {
		if _, err := os.Lstat(d); errors.Is(err, fs.ErrNotExist) {
			placed.dirs = append(placed.dirs, d)
		}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("daemonimage: cannot make the plugin directory %s: %w", dir, err)
	}
	idx := len(placed.items)
	path := filepath.Join(dir, build.name)
	placed.items = append(placed.items, pluginPlacement{path: path})
	if info, err := os.Lstat(path); err == nil && info.Mode().IsRegular() {
		replaced := filepath.Join(stage, fmt.Sprintf("%d-replaced-%s", idx, build.name))
		if err := ops.rename(path, replaced); err != nil {
			return fmt.Errorf("daemonimage: cannot move the %s already in %s aside: %w", build.name, dir, err)
		}
		placed.items[idx].replaced = replaced
	}
	if err := ops.rename(filepath.Join(stage, build.name), path); err != nil {
		return fmt.Errorf("daemonimage: cannot move %s into %s: %w", build.name, dir, err)
	}
	placed.items[idx].moved = true
	return nil
}

// undo takes every plugin binary movePlugins placed back out of b.Out, puts
// back each file it replaced, and removes the directories it made: a directory
// that is not empty by then holds something this call did not put there, so it
// stays and is reported. It returns an error naming what it could not undo: a
// caller must never report a clean refusal over a build context this call left
// changed.
func (p *pluginPlacements) undo(ops fileOps) error {
	var failed []string
	for i := len(p.items) - 1; i >= 0; i-- {
		item := p.items[i]
		if item.moved {
			if err := ops.remove(item.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				failed = append(failed, err.Error())
				continue
			}
		}
		if item.replaced == "" {
			continue
		}
		if err := ops.rename(item.replaced, item.path); err != nil {
			failed = append(failed, err.Error())
		}
	}
	for i := len(p.dirs) - 1; i >= 0; i-- {
		if err := ops.remove(p.dirs[i]); err != nil && !errors.Is(err, fs.ErrNotExist) {
			failed = append(failed, err.Error())
		}
	}
	if len(failed) != 0 {
		return fmt.Errorf("daemonimage: cannot undo the plugin move: %s", strings.Join(failed, "; "))
	}
	return nil
}

// IsCanonicalClone reports whether path is a rig's canonical clone,
// <town>/<rig>/mayor/rig, or lies under one. It reads the name only: a caller
// that must catch a symlink to the clone resolves path first.
func IsCanonicalClone(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if filepath.Base(p) == "rig" && filepath.Base(filepath.Dir(p)) == "mayor" {
			return true
		}
		if parent := filepath.Dir(p); parent == p {
			return false
		}
	}
}

// refuseCanonicalClone refuses dir when it is a canonical clone or lies inside
// one, as written or with its symlinks resolved. A build adds worktrees to its
// sources and never touches the canonical clone. A path that does not resolve is
// judged as written: a link to the clone resolves, so one that does not is not.
func refuseCanonicalClone(what, dir string) error {
	paths := []string{dir}
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		paths = append(paths, resolved)
	}
	for _, p := range paths {
		if IsCanonicalClone(p) {
			return fmt.Errorf("daemonimage: %s %q is or lies inside a canonical clone (.../mayor/rig): the build adds worktrees to its sources and the canonical clone is never touched", what, dir)
		}
	}
	return nil
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
	ldflags := build.ldflags
	if build.stampsBuildID {
		short, err := bdBuildID(b.Git, dir, build.commit)
		if err != nil {
			return err
		}
		ldflags = bdBuildFlag + short + " " + ldflags
	}
	argv := goBuildArgv(filepath.Join(stage, build.name), build.pkg, ldflags)
	if err := b.Run(ctx, dir, env, argv); err != nil {
		return fmt.Errorf("daemonimage: building %s from %s failed: %w", build.name, build.pkg, err)
	}
	return nil
}

// goBuildArgv is the compile of one binary to out: the same argv a build runs and
// Plan prints.
func goBuildArgv(out, pkg, ldflags string) []string {
	argv := []string{"go", "build", "-trimpath"}
	if ldflags != "" {
		argv = append(argv, "-ldflags", ldflags)
	}
	return append(argv, "-o", out, pkg)
}

// imageArgv is the docker build of the image tagged tag from the context dir
// context with dockerfile: the same argv BuildImage runs and Plan prints.
func imageArgv(dockerfile, tag, context string) []string {
	return []string{"docker", "--context", DockerContext, "build", "-f", dockerfile, "-t", tag, context}
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

// bdBuildID returns the value bd's main.Build is stamped with: the abbreviation
// git prints for commit in dir, the worktree the build runs in. git picks its
// length from the checkout's object count, the rule the host's own bd build
// answers under. An answer that is not a commit is refused rather than stamped,
// because a bd with an empty main.Build reports a build id no host build has
// (gt-glsx8).
func bdBuildID(git bdbuild.Git, dir, commit string) (string, error) {
	out, err := git(dir, "rev-parse", "--short", commit)
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot read bd's build id of %s in its worktree %s: %w", commit, dir, err)
	}
	short := strings.TrimSpace(string(out))
	if !isCommit(short) {
		return "", fmt.Errorf("daemonimage: git in the bd worktree %s printed %q as the build id of %s, which is not a 7+ character hex commit", dir, short, commit)
	}
	return short, nil
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

// Plan returns the steps a full build of rev would run, one line each, and runs
// none of them: it makes no worktree, resolves no commit, writes no file and
// reaches no network. It names the pins file and each pin first. It refuses the
// inputs a build refuses before it runs anything (an empty or relative Out, a
// rev, bd or om commit that is not a commit, an unnamed source with no pin to
// fetch it from, a pin whose URL is not a credential-free http(s) repository URL
// or whose commit is not a full sha, a mango source or commit that is missing or
// not a commit, an hm or devops commit that is missing or not a commit, a
// canonical clone as any input, no pins, a Dockerfile that is not an absolute
// regular file). It does not
// check the claude CLI pin, and it does not check that a commit exists in its
// checkout, that a checkout is at its pin and clean, or that a pin is fresh:
// CheckPins and a build check those when they run, so a plan that prints can
// still fail there. The commits in it are the ones named, not resolved: the gt
// line stamps buildstamp.Len characters of the named commit, and the bd line's
// main.Build is planBuildID, since the build id is git's abbreviation of the
// commit in the worktree a build makes and Plan makes none.
func (b Builder) Plan(rev string) ([]string, error) { return b.plan(rev, false) }

// PlanContext returns the steps a context-only build of rev would run: the same
// steps as Plan up to the staged build context, and neither the mango build (a
// container of its own) nor the image build, so no line of it runs the container
// engine. Its last line is ContextLine, naming the absolute directory of the
// context the build stages.
func (b Builder) PlanContext(rev string) ([]string, error) { return b.plan(rev, true) }

// plan is Plan and PlanContext: the steps of the build, without the two engine
// steps when contextOnly is set.
func (b Builder) plan(rev string, contextOnly bool) ([]string, error) {
	rev, bdCommit, omCommit, err := b.checkBuildInputs(rev)
	if err != nil {
		return nil, err
	}
	if b.Dockerfile == "" {
		return nil, fmt.Errorf("daemonimage: no Dockerfile named; set Dockerfile to %s in the gt checkout", DockerfilePath)
	}
	if !filepath.IsAbs(b.Dockerfile) {
		return nil, fmt.Errorf("daemonimage: Dockerfile %q is not absolute; docker resolves -f against the directory it runs in, not the caller's", b.Dockerfile)
	}
	if info, err := os.Stat(b.Dockerfile); err != nil || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("daemonimage: Dockerfile %s is not a file", b.Dockerfile)
	}
	if b.PinsPath == "" {
		return nil, fmt.Errorf("daemonimage: no pins file named; set PinsPath to %s in the gt checkout", PinsPath)
	}
	if len(b.Pins) != len(PinNames) {
		return nil, fmt.Errorf("daemonimage: %d pins named, want the four (%s) from %s", len(b.Pins), strings.Join(PinNames, ", "), b.PinsPath)
	}
	// Every pin is checked before any line is built, so the plan never prints a
	// URL a fetch would refuse, a credential among them.
	for _, p := range b.Pins {
		if err := checkFetchable(p.Name, p); err != nil {
			return nil, err
		}
	}
	if err := b.checkToolInputs(); err != nil {
		return nil, err
	}
	// Each source is planned once, in the order the build reads it, so a source
	// with no checkout and no usable pin is refused before any line is built.
	sources := make(map[string]plannedSource, len(PinNames))
	for _, s := range []struct{ name, src string }{
		{"bd", b.BDSrc}, {"om", b.OMSrc}, {"hm", b.HMSrc}, {"devops", b.DevopsSrc},
	} {
		p, err := b.sourcePlan(s.name, s.src)
		if err != nil {
			return nil, err
		}
		sources[s.name] = p
	}
	bd, om := sources["bd"], sources["om"]
	devops := sources["devops"]
	stamp := planCommitStamp(rev)
	tag := ImageTag(rev)
	lines := []string{fmt.Sprintf("pins: %s", b.PinsPath)}
	for _, p := range b.Pins {
		lines = append(lines, fmt.Sprintf("pin %s: %s at %s", p.Name, p.URL, p.Commit))
	}
	lines = append(lines,
		fmt.Sprintf("build gt: %s   (clean worktree of %s at %s)", strings.Join(goBuildArgv(filepath.Join(b.Out, "gt"), GTPackage, gtCommitFlag+stamp), " "), b.GTSrc, rev),
	)
	lines = append(lines, bd.fetch...)
	lines = append(lines, fmt.Sprintf("build bd: %s   (clean worktree of %s at %s)", strings.Join(goBuildArgv(filepath.Join(b.Out, "bd"), BDPackage, bdBuildFlag+planBuildID+" "+bdCommitFlag+bdCommit), " "), bd.dir, bdCommit))
	lines = append(lines, om.fetch...)
	lines = append(lines, fmt.Sprintf("build om: %s   (clean worktree of %s at %s)", strings.Join(goBuildArgv(filepath.Join(b.Out, "om"), OMPackage, ""), " "), om.dir, omCommit))
	for _, p := range imagePlugins {
		lines = append(lines, fmt.Sprintf("build %s: %s   (clean worktree of %s at %s)", p.name, strings.Join(goBuildArgv(filepath.Join(b.Out, "plugins", p.name, p.name), p.pkg, ""), " "), b.GTSrc, rev))
	}
	hm := sources["hm"]
	lines = append(lines, hm.fetch...)
	lines = append(lines, devops.fetch...)
	lines = append(lines, b.toolPlanLines(hm.dir, devops.dir)...)
	if !contextOnly {
		mangoCommit, err := b.checkMangoInputs()
		if err != nil {
			return nil, err
		}
		lines = append(lines, fmt.Sprintf("build mango: %s   (clean worktree of %s at %s)",
			strings.Join(mangoArgv(planMangoWorktree, planMangoStage), " "), b.MangoSrc, mangoCommit))
	}
	lines = append(lines,
		fmt.Sprintf("fetch claude: GET %s -> %s   (sha256 %s)", fmt.Sprintf(templates.ClaudeCLIURLFormat, templates.ClaudeCLIVersion), filepath.Join(b.Out, claudeCLIName), templates.ClaudeCLISHA256),
	)
	for _, t := range templates.DaemonClientTools {
		lines = append(lines, fmt.Sprintf("fetch %s: GET %s -> %s   (sha256 %s)", t.Name, t.URL, filepath.Join(b.Out, t.Name), t.SHA256))
	}
	if contextOnly {
		lines = append(lines, ContextLine(b.Out))
	} else {
		lines = append(lines, fmt.Sprintf("image: %s", strings.Join(imageArgv(b.Dockerfile, tag, b.Out), " ")))
	}
	return lines, nil
}

// ContextLine is the line a context-only build ends with: the absolute directory
// holding the staged build context, ready for the engine step that builds the
// image from it.
func ContextLine(dir string) string { return "context: " + dir }

// plannedSource is one source's place in the plan: the directory it is built from
// and, for a source with no checkout, the line that fetches it there.
type plannedSource struct {
	dir   string
	fetch []string
}

// sourcePlan returns the directory a source is built from in a plan, and the
// line that fetches it when it has no checkout flag. Nothing runs: a source with
// neither a checkout nor a pin a fetch can use is refused exactly as a build
// would refuse it. A fetched source's directory is a placeholder, planFetchedDir:
// the build makes its own under Out and Plan cannot know the name.
func (b Builder) sourcePlan(name, src string) (plannedSource, error) {
	if src != "" {
		return plannedSource{dir: src}, nil
	}
	pin, err := b.sourcePin(name)
	if err != nil {
		return plannedSource{}, err
	}
	argv := make([]string, 0, len(fetchArgv(pin)))
	for _, cmd := range fetchArgv(pin) {
		argv = append(argv, strings.Join(cmd, " "))
	}
	dir := filepath.Join(b.Out, planFetchedDir(name))
	return plannedSource{
		dir:   dir,
		fetch: []string{fmt.Sprintf("fetch %s: %s   (in %s, made fresh)", name, strings.Join(argv, "; "), dir)},
	}, nil
}

// planFetchedDir is what Plan prints in place of a fetched source's directory:
// the build makes a fresh one under Out with a name Plan cannot know. It is one
// shell-safe token, because Plan prints it inside a command line.
func planFetchedDir(name string) string { return "fetched_" + name + "_dir" }

// planBuildID is what Plan prints in place of bd's build id: git answers it
// inside the worktree BuildBinaries makes, and Plan makes none. It is one
// shell-safe token, letters, digits and underscores, because Plan prints it
// inside a command line: a space there would split the printed argv into two
// words, and a shell reads angle brackets as redirection.
const planBuildID = "short_sha_read_in_worktree"

// claudeCLIName is the file the claude CLI is written to in the build context;
// the Dockerfile's COPY reads it by this name.
const claudeCLIName = "claude"

// claudeVersion is an exact dotted version such as 2.1.314: digits and dots, so
// a floating tag such as latest or stable cannot match.
var claudeVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// sha256Pin is the form a sha256 sum prints in: 64 lower-case hex digits. The
// claude CLI pin and every client tool pin are checked against it.
var sha256Pin = regexp.MustCompile(`^[0-9a-f]{64}$`)

// FetchClaudeCLI downloads the pinned linux/arm64 claude CLI, verifies its sha256
// against templates.ClaudeCLISHA256, and writes it to b.Out/claude, the build
// context the Dockerfile COPYs from. It returns that path. The bytes are verified
// before the file takes its name, so a fetch that fails or does not match returns
// an error and leaves no file in b.Out.
func (b Builder) FetchClaudeCLI(ctx context.Context) (string, error) {
	version := templates.ClaudeCLIVersion
	return b.fetchClaudeCLI(ctx, fmt.Sprintf(templates.ClaudeCLIURLFormat, version), version, templates.ClaudeCLISHA256, claudeCLIMaxBytes)
}

// claudeCLIMaxBytes bounds the claude CLI download. The pinned binary is far
// smaller, so a stream over the bound is refused rather than written to the disk.
const claudeCLIMaxBytes = 1 << 30

// claudeTemp is the temporary file the download is written to. *os.File is one;
// a test stands in a file whose Close fails, to drive that failure.
type claudeTemp interface {
	io.Writer
	Close() error
	Name() string
}

// createClaudeTemp makes the temporary file fetchClaudeCLIWith writes through.
func createClaudeTemp(dir, pattern string) (claudeTemp, error) {
	f, err := os.CreateTemp(dir, pattern)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// fetchClaudeCLI is FetchClaudeCLI with the URL, the pin and the size bound
// injected, so a test can hand it bytes whose sha256 it has computed.
func (b Builder) fetchClaudeCLI(ctx context.Context, url, version, want string, max int64) (string, error) {
	return b.fetchClaudeCLIWith(ctx, url, version, want, max, createClaudeTemp)
}

// fetchClaudeCLIWith is fetchClaudeCLI with the temporary file's source injected,
// so each step's failure can be driven.
func (b Builder) fetchClaudeCLIWith(ctx context.Context, url, version, want string, max int64, create func(dir, pattern string) (claudeTemp, error)) (string, error) {
	if err := checkClaudePin(version, want); err != nil {
		return "", err
	}
	return b.fetchPinned(ctx, pinnedFile{what: "the claude CLI", name: claudeCLIName, url: url, sha256: want, max: max}, create)
}

// pinnedFile is one download the deployer verifies and places in the build
// context: the claude CLI and every client tool go through fetchPinned. A member
// names the one file to take out of a .tar.gz download; empty means the
// download is the file itself.
type pinnedFile struct {
	what, name, url, sha256, member string
	max                             int64
}

// fetchPinned downloads p.url, refuses it unless it is at most p.max bytes and
// its sha256 is p.sha256, and places the file (or the archive's member) in b.Out
// under p.name. The download is written under a temporary name and takes its
// own only once it verifies; any other exit removes the temporary file, so a
// refused download leaves nothing in b.Out.
func (b Builder) fetchPinned(ctx context.Context, p pinnedFile, create func(dir, pattern string) (claudeTemp, error)) (string, error) {
	if b.Fetch == nil {
		return "", fmt.Errorf("daemonimage: no fetcher to download %s with", p.what)
	}
	if b.Out == "" {
		return "", fmt.Errorf("daemonimage: no build context for %s; Out is the directory the binaries are in", p.what)
	}
	if !filepath.IsAbs(b.Out) {
		return "", fmt.Errorf("daemonimage: build context %q is not absolute; %s is written there for the Dockerfile's COPY", b.Out, p.what)
	}
	body, err := b.Fetch(ctx, p.url)
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot download %s from %s: %w", p.what, p.url, err)
	}
	defer func() { _ = body.Close() }()
	tmp, err := create(b.Out, "."+p.name+"-*")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot stage %s in %s: %w", p.what, b.Out, err)
	}
	placed := false
	defer func() {
		if !placed {
			_ = os.Remove(tmp.Name())
		}
	}()
	sum := sha256.New()
	// An archive's member is taken from memory once the download verifies; a
	// plain file is only ever on disk.
	var archive bytes.Buffer
	sinks := []io.Writer{tmp, sum}
	if p.member != "" {
		sinks = append(sinks, &archive)
	}
	// The copy reads at most max+1 bytes: a stream that reaches max+1 is over the
	// bound, and nothing past max+1 is read.
	n, err := io.Copy(io.MultiWriter(sinks...), io.LimitReader(body, p.max+1))
	if err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("daemonimage: cannot download %s from %s: %w", p.what, p.url, err)
	}
	if n > p.max {
		_ = tmp.Close()
		return "", errOverBound(p.what, p.url, p.max, b.Out)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("daemonimage: cannot write %s in %s: %w", p.what, b.Out, err)
	}
	if err := verifySum(p.what, p.url, hex.EncodeToString(sum.Sum(nil)), p.sha256, b.Out); err != nil {
		return "", err
	}
	path := filepath.Join(b.Out, p.name)
	if p.member != "" {
		data, err := extractMember(archive.Bytes(), p.member, p.max)
		if err != nil {
			return "", fmt.Errorf("daemonimage: %s from %s: %w", p.what, p.url, err)
		}
		if err := atomicfile.WriteFile(path, data, 0o755); err != nil {
			return "", fmt.Errorf("daemonimage: cannot write %s: %w", path, err)
		}
		return path, nil
	}
	// The Dockerfile's COPY sets the binary's mode, so the file keeps the
	// temporary file's.
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("daemonimage: cannot move %s into %s: %w", p.what, b.Out, err)
	}
	placed = true
	return path, nil
}

// errOverBound is the refusal for a download that reaches past its size bound,
// shared by the claude CLI and the client tools so the two read alike.
func errOverBound(what, url string, max int64, out string) error {
	return fmt.Errorf("daemonimage: %s from %s is over the %d-byte bound; nothing was written to %s", what, url, max, out)
}

// verifySum is the refusal for a download whose sha256 is not its pin, shared
// by the claude CLI and the client tools.
func verifySum(what, url, got, want, out string) error {
	if got != want {
		return fmt.Errorf("daemonimage: %s from %s has sha256 %s, not the pinned %s; nothing was written to %s", what, url, got, want, out)
	}
	return nil
}

// checkClaudePin refuses a claude CLI pin that is not an exact version and a
// sha256 in the form a sum prints, so a build never fetches a moving target or
// checks against a value that cannot match.
func checkClaudePin(version, sha string) error {
	if !claudeVersion.MatchString(version) {
		return fmt.Errorf("daemonimage: claude CLI version %q is not an exact dotted version such as 2.1.314; a floating tag would fetch a binary the pin does not name", version)
	}
	if !sha256Pin.MatchString(sha) {
		return fmt.Errorf("daemonimage: claude CLI sha256 %q is not 64 lower-case hex digits", sha)
	}
	return nil
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
	if err := refuseCanonicalClone("build context", b.Out); err != nil {
		return "", err
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
	if err := b.stageKnownHosts(); err != nil {
		return "", err
	}
	tag := ImageTag(rev)
	argv := imageArgv(b.Dockerfile, tag, b.Out)
	if err := b.Run(ctx, b.Out, dockerEnv(b.Environ), argv); err != nil {
		return "", fmt.Errorf("daemonimage: building image %s failed: %w", tag, err)
	}
	return tag, nil
}

// KnownHostsName is the file the image's known_hosts COPY reads from the build
// context. It sits beside the Dockerfile in the gt checkout.
const KnownHostsName = "ssh_known_hosts"

// stageKnownHosts copies the known_hosts file beside b.Dockerfile into the build
// context, so the Dockerfile's COPY of it reads the same bytes the repo holds. A
// missing or unreadable file is a refusal: docker would report it only after the
// base image pulled.
func (b Builder) stageKnownHosts() error {
	src := filepath.Join(filepath.Dir(b.Dockerfile), KnownHostsName)
	body, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("daemonimage: %s is not readable; the image's COPY of it reads only the build context: %w", src, err)
	}
	if err := os.WriteFile(filepath.Join(b.Out, KnownHostsName), body, 0o644); err != nil {
		return fmt.Errorf("daemonimage: staging %s in the build context %s: %w", KnownHostsName, b.Out, err)
	}
	return nil
}

// checkImageBinaries refuses a set of binaries the Dockerfile's COPY lines could
// not read: each of the image's binaries (gt, bd, om, claude, the toolNames and
// ClientToolNames), each of imagePlugins and each of mangoNames once, at the path
// under b.Out its COPY reads. Docker would report a missing or misplaced one only
// after the package install ran, so it is caught here first, against what the
// caller passed.
func (b Builder) checkImageBinaries(bins []string) error {
	// rels is each binary's COPY source relative to the build context; want holds
	// the same names in the order the refusal messages use.
	rels := make(map[string]string)
	var want []string
	for _, name := range append(append([]string{"gt", "bd", "om", claudeCLIName}, toolNames...), ClientToolNames()...) {
		rels[name] = name
		want = append(want, name)
	}
	for _, p := range imagePlugins {
		rels[p.name] = filepath.Join("plugins", p.name, p.name)
		want = append(want, p.name)
	}
	for _, name := range mangoNames {
		rels[name] = filepath.Join(mangoDir, name)
		want = append(want, name)
	}
	seen := make(map[string]bool, len(want))
	for _, p := range bins {
		name := filepath.Base(p)
		rel, ok := rels[name]
		if !ok {
			return fmt.Errorf("daemonimage: %q is not one of the image's binaries %s", p, strings.Join(want, ", "))
		}
		if filepath.Clean(p) != filepath.Join(filepath.Clean(b.Out), rel) {
			return fmt.Errorf("daemonimage: %q is outside the build context %s, so the Dockerfile's COPY of %s reads nothing", p, b.Out, rel)
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
			return fmt.Errorf("daemonimage: no %s binary among the %d given; the image copies %s", name, len(bins), strings.Join(want, ", "))
		}
	}
	return nil
}

// dockerEnvKeys names the host variables the image build runs with: those that
// find the docker CLI and read its config (DOCKER_CONFIG holds the registry
// logins a base-image pull needs), plus the proxy and CA settings a pull through
// a proxy needs. bdbuild.Env's compile settings stay out, and so do
// DOCKER_CONTEXT and DOCKER_HOST: the build names its context in argv, so a
// shell's choice of engine cannot reach it.
var dockerEnvKeys = map[string]bool{
	"PATH": true, "HOME": true, "TMPDIR": true, "DOCKER_CONFIG": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "NO_PROXY": true,
	"http_proxy": true, "https_proxy": true, "no_proxy": true,
}

// DockerEnv is dockerEnv for a caller that runs its own docker command with an
// explicit --context: the variables docker needs, and none of DOCKER_CONTEXT or
// DOCKER_HOST, so a shell cannot swap the engine. It is exported only because
// the scratch commands (internal/cmd) need the same allowlisted environment; it
// adds no behavior of its own.
func DockerEnv(environ []string) []string { return dockerEnv(environ) }

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
