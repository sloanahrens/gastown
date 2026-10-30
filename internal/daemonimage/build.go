// Package daemonimage builds the gastown daemon's container artifacts on the
// host: the linux/arm64 gt, bd and om binaries the image runs (gt-qpeei.9), the
// pinned claude CLI om's reviewer runs (gt-qpeei.35), and the image built from
// them (gt-qpeei.16). Every command it runs and every download it makes is one
// injected call, so the gate proves what a build runs without Docker, a running
// daemon, a cluster or the network.
package daemonimage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

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
	// Fetch downloads the claude CLI FetchClaudeCLI places in Out; nil refuses
	// that fetch.
	Fetch Fetcher
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
	rev, bdCommit, err := b.checkBuildInputs(rev)
	if err != nil {
		return nil, err
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

// checkBuildInputs refuses the inputs BuildBinaries cannot build from, before
// anything runs, and returns rev and the bd commit trimmed. It reads nothing
// from the host, which is what lets Plan refuse exactly what a build would.
func (b Builder) checkBuildInputs(rev string) (string, string, error) {
	if b.Out == "" {
		return "", "", errors.New("daemonimage: no output directory for the binaries")
	}
	if !filepath.IsAbs(b.Out) {
		return "", "", fmt.Errorf("daemonimage: output directory %q is not absolute; a build runs in a worktree, not in the caller's directory", b.Out)
	}
	if rev = strings.TrimSpace(rev); rev == "" {
		return "", "", errors.New("daemonimage: no rev to stamp gt with")
	}
	if !isCommit(rev) {
		return "", "", fmt.Errorf("daemonimage: rev %q is not a 7+ character hex commit", rev)
	}
	for _, src := range []struct{ name, dir string }{{"gt", b.GTSrc}, {"bd", b.BDSrc}, {"om", b.OMSrc}} {
		if src.dir == "" {
			return "", "", fmt.Errorf("daemonimage: no %s source checkout named", src.name)
		}
	}
	for _, p := range []struct{ name, dir string }{{"output directory", b.Out}, {"gt source", b.GTSrc}, {"bd source", b.BDSrc}, {"om source", b.OMSrc}} {
		if err := refuseCanonicalClone(p.name, p.dir); err != nil {
			return "", "", err
		}
	}
	bdCommit := strings.TrimSpace(b.BDCommit)
	if bdCommit == "" {
		return "", "", errors.New("daemonimage: no bd commit to stamp bd with")
	}
	if !isCommit(bdCommit) {
		return "", "", fmt.Errorf("daemonimage: bd commit %q is not a 7+ character hex commit", bdCommit)
	}
	return rev, bdCommit, nil
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
	argv := goBuildArgv(filepath.Join(stage, build.name), build.pkg, build.ldflags)
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
// reaches no network. It refuses the inputs a build refuses before it runs
// anything (an empty or relative Out, a rev or bd commit that is not a commit,
// an unnamed source, a canonical clone as any input, a Dockerfile that is not an
// absolute regular file). It does
// not check the claude CLI pin, and it does not check that a commit exists in its
// checkout: a build checks both when it runs, so a plan that prints can still
// fail there. The commits in it are the ones named, not resolved.
func (b Builder) Plan(rev string) ([]string, error) {
	rev, bdCommit, err := b.checkBuildInputs(rev)
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
	tag := ImageTag(rev)
	return []string{
		fmt.Sprintf("build gt: %s   (clean worktree of %s at %s)", strings.Join(goBuildArgv(filepath.Join(b.Out, "gt"), GTPackage, gtCommitFlag+rev), " "), b.GTSrc, rev),
		fmt.Sprintf("build bd: %s   (clean worktree of %s at %s)", strings.Join(goBuildArgv(filepath.Join(b.Out, "bd"), BDPackage, bdCommitFlag+bdCommit), " "), b.BDSrc, bdCommit),
		fmt.Sprintf("build om: %s   (clean worktree of %s at HEAD)", strings.Join(goBuildArgv(filepath.Join(b.Out, "om"), OMPackage, ""), " "), b.OMSrc),
		fmt.Sprintf("fetch claude: GET %s -> %s   (sha256 %s)", fmt.Sprintf(templates.ClaudeCLIURLFormat, templates.ClaudeCLIVersion), filepath.Join(b.Out, claudeCLIName), templates.ClaudeCLISHA256),
		fmt.Sprintf("image: %s", strings.Join(imageArgv(b.Dockerfile, tag, b.Out), " ")),
	}, nil
}

// claudeCLIName is the file the claude CLI is written to in the build context;
// the Dockerfile's COPY reads it by this name.
const claudeCLIName = "claude"

// claudeVersion is an exact dotted version such as 2.1.314: digits and dots, so
// a floating tag such as latest or stable cannot match.
var claudeVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+)+$`)

// claudeSHA256 is the form a sha256 sum prints in: 64 lower-case hex digits.
var claudeSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	if b.Fetch == nil {
		return "", errors.New("daemonimage: no fetcher to download the claude CLI with")
	}
	if b.Out == "" {
		return "", errors.New("daemonimage: no build context for the claude CLI; Out is the directory the binaries are in")
	}
	if !filepath.IsAbs(b.Out) {
		return "", fmt.Errorf("daemonimage: build context %q is not absolute; the claude CLI is written there for the Dockerfile's COPY", b.Out)
	}
	if err := checkClaudePin(version, want); err != nil {
		return "", err
	}
	body, err := b.Fetch(ctx, url)
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot download the claude CLI %s from %s: %w", version, url, err)
	}
	defer func() { _ = body.Close() }()
	tmp, err := create(b.Out, "."+claudeCLIName+"-*")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot stage the claude CLI in %s: %w", b.Out, err)
	}
	// The download is written under a temporary name and takes the claude name
	// only once it verifies; any other exit removes the temporary file.
	placed := false
	defer func() {
		if !placed {
			_ = os.Remove(tmp.Name())
		}
	}()
	sum := sha256.New()
	// The copy reads at most max+1 bytes: a stream that reaches max+1 is over the
	// bound, and nothing past max+1 is read.
	n, err := io.Copy(io.MultiWriter(tmp, sum), io.LimitReader(body, max+1))
	if err != nil {
		_ = tmp.Close()
		return "", fmt.Errorf("daemonimage: cannot download the claude CLI from %s: %w", url, err)
	}
	if n > max {
		_ = tmp.Close()
		return "", fmt.Errorf("daemonimage: the claude CLI from %s is over the %d-byte bound; nothing was written to %s", url, max, b.Out)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("daemonimage: cannot write the claude CLI in %s: %w", b.Out, err)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != want {
		return "", fmt.Errorf("daemonimage: the claude CLI from %s has sha256 %s, not the pinned %s; nothing was written to %s", url, got, want, b.Out)
	}
	// The Dockerfile's COPY sets the binary's mode, so the file keeps the
	// temporary file's.
	path := filepath.Join(b.Out, claudeCLIName)
	if err := os.Rename(tmp.Name(), path); err != nil {
		return "", fmt.Errorf("daemonimage: cannot move the claude CLI into %s: %w", b.Out, err)
	}
	placed = true
	return path, nil
}

// checkClaudePin refuses a claude CLI pin that is not an exact version and a
// sha256 in the form a sum prints, so a build never fetches a moving target or
// checks against a value that cannot match.
func checkClaudePin(version, sha string) error {
	if !claudeVersion.MatchString(version) {
		return fmt.Errorf("daemonimage: claude CLI version %q is not an exact dotted version such as 2.1.314; a floating tag would fetch a binary the pin does not name", version)
	}
	if !claudeSHA256.MatchString(sha) {
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
	tag := ImageTag(rev)
	argv := imageArgv(b.Dockerfile, tag, b.Out)
	if err := b.Run(ctx, b.Out, dockerEnv(b.Environ), argv); err != nil {
		return "", fmt.Errorf("daemonimage: building image %s failed: %w", tag, err)
	}
	return tag, nil
}

// checkImageBinaries refuses a set of binaries the Dockerfile's COPY lines could
// not read: gt, bd, om and claude once each, as files in the build context. Docker
// would report a missing or misplaced one only after the package install ran, so
// it is caught here first, against what the caller passed.
func (b Builder) checkImageBinaries(bins []string) error {
	want := []string{"gt", "bd", "om", claudeCLIName}
	seen := make(map[string]bool, len(want))
	for _, p := range bins {
		name := filepath.Base(p)
		if !slices.Contains(want, name) {
			return fmt.Errorf("daemonimage: %q is not one of the image's binaries gt, bd, om and claude", p)
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
			return fmt.Errorf("daemonimage: no %s binary among the %d given; the image copies gt, bd, om and claude", name, len(bins))
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
