// Package imagectx stages the polecat image's build context from the pins
// (gt-gg9hv.33): the cross-compiled gt, polecat-image and sweep-run, the bd built
// at its pin, the seed sources at their pins and the Go floor. Every source with
// no checkout is fetched by URL, and no container engine runs here, so the CI
// job can stage the context and `make polecat-image` does only the engine build
// and the push after it.
//
// Every command the build runs goes through one of the Seams, so a test sees
// each argv and no engine can be reached from this package.
package imagectx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/mod/modfile"

	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
	"github.com/steveyegge/gastown/internal/polecat/seed"
)

// BuildRig is the rig the image is built at. It is seeded from the build commit
// and never from a pin line, so its seed-pins row is ignored.
const BuildRig = "gastown"

// runCeiling bounds one command ExecRun runs. A fetch reaches the network, and an
// unreachable source would otherwise stall the build with no message.
const runCeiling = 5 * time.Minute

// Inputs is one context build.
type Inputs struct {
	// Src is the gt checkout the gt binaries and the build rig's seed are read
	// from. It must be a clean tree whose HEAD is Rev.
	Src string
	// Rev is the full sha of the commit the image is built at.
	Rev string
	// Out is the absolute, empty directory the context is staged in.
	Out string
	// BD is the bd source: the URL and commit the image's bd is built at.
	BD daemonimage.Pin
	// Seeds are the rows of the seed-pins file, and SeedURLs maps each seeded rig
	// other than BuildRig to its repository URL.
	Seeds    []seed.Source
	SeedURLs map[string]string
}

// GitEnv runs git with env as its whole environment and returns its standard
// output. A read that reaches a repository URL goes through it, so git sees the
// build's environment and cannot prompt on a terminal for a credential.
type GitEnv func(dir string, env []string, args ...string) ([]byte, error)

// Seams are the commands a build runs: Run for a fetch, Git for a read or a
// worktree, GitEnv for a read of a URL, Go for a compile. The real ones are
// ExecRun, bdbuild.ExecGit, bdbuild.ExecGitEnv and bdbuild.ExecGo. Nothing else
// runs.
type Seams struct {
	Run    daemonimage.Runner
	Git    bdbuild.Git
	GitEnv GitEnv
	Go     bdbuild.Go
	// Environ is the host environment the builds and the git reads start from;
	// nil reads the process's own. A test sets it, so none touches the process
	// environment.
	Environ []string
}

// environ is the host environment Build starts from: Environ, or the process's
// own when it is nil.
func (s Seams) environ() []string {
	if s.Environ == nil {
		return os.Environ()
	}
	return s.Environ
}

// Result is a staged context and the labels its image build needs.
type Result struct {
	// Dir is the context directory, absolute.
	Dir string
	// Notes are the pin-drift guard's notes for the seeded rigs.
	Notes []string
	// GTCommit, BDCommit and GoFloor are the labels and build argument the image
	// build takes.
	GTCommit string
	BDCommit string
	GoFloor  string
}

// Lines is what the command prints: one note line per guard note, then the
// context directory and the labels, each on its own line.
func (r Result) Lines() []string {
	lines := make([]string, 0, len(r.Notes)+4)
	for _, n := range r.Notes {
		lines = append(lines, "note: "+n)
	}
	return append(lines,
		"context: "+r.Dir,
		"label: org.gastown.gt-commit="+r.GTCommit,
		"label: org.gastown.bd-commit="+r.BDCommit,
		"label: go-floor="+r.GoFloor,
	)
}

// fullSHA is the form a pin's commit and the --rev take: a full sha.
var fullSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// goVersion is the form of a go directive's version, as go-floor.sh accepts.
var goVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+(\.[0-9]+)?$`)

// rigName is the form of a rig's name: lower-case letters, digits and hyphens,
// so the name is one path element under the context and no separator or dot
// segment can escape it.
var rigName = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// ParseSeedURLs parses a seed-urls file: one "<rig> <url>" per line, with blank
// lines and "#" comments ignored. A rig named twice is refused. The URL is not
// checked here: FetchSource refuses a URL that carries a credential or is not
// http(s), before any command runs.
func ParseSeedURLs(data []byte) (map[string]string, error) {
	urls := map[string]string{}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("line %d: want \"<rig> <url>\"", i+1)
		}
		if _, dup := urls[fields[0]]; dup {
			return nil, fmt.Errorf("line %d: rig %s is named twice", i+1, fields[0])
		}
		urls[fields[0]] = fields[1]
	}
	return urls, nil
}

// Build stages the context in in.Out and returns its labels. It refuses before
// it builds anything when an input is wrong, the tree is dirty or not at Rev, or
// a seeded pin fails the pin-drift guard.
func Build(ctx context.Context, in Inputs, s Seams) (Result, error) {
	if s.Run == nil || s.Git == nil || s.GitEnv == nil || s.Go == nil {
		return Result{}, errors.New("imagectx: a run, git, git-env and go seam are all needed to stage the context")
	}
	if err := checkInputs(in); err != nil {
		return Result{}, err
	}
	if err := checkCleanAt(in, s.Git); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(filepath.Join(in.Out, "bin"), 0o755); err != nil {
		return Result{}, fmt.Errorf("imagectx: cannot make the bin directory in %s: %w", in.Out, err)
	}
	env := buildEnvFrom(s.environ())
	genv := gitEnv(s.environ())
	res := Result{Dir: in.Out, GTCommit: in.Rev}

	// Every tree is read and every pin guarded before anything is unpacked into
	// the context, so a refusal leaves the context without a seed directory. The
	// build rig's tree is the one at Rev, read from the checkout it runs in.
	trees := map[string][]byte{}
	gastown, err := archiveTree(in.Src, in.Rev, s.Git)
	if err != nil {
		return Result{}, err
	}
	trees[BuildRig] = gastown
	for _, src := range in.Seeds {
		if src.Rig == BuildRig {
			continue
		}
		note, tree, err := stageRig(ctx, in, s, genv, src)
		if err != nil {
			return Result{}, err
		}
		if note != "" {
			res.Notes = append(res.Notes, note)
		}
		trees[src.Rig] = tree
	}

	bdCommit, err := buildBD(ctx, in, s, genv)
	if err != nil {
		return Result{}, err
	}
	res.BDCommit = bdCommit

	for _, b := range []struct{ name, pkg string }{
		{"gt", "./cmd/gt"},
		{"polecat-image", "./plugins/polecat-image"},
		{"sweep-run", "./cmd/sweep-run"},
	} {
		out := filepath.Join(in.Out, "bin", b.name)
		if _, err := s.Go(in.Src, env, bdbuild.GoBuildArgv(out, b.pkg)...); err != nil {
			return Result{}, fmt.Errorf("imagectx: cannot build %s at %s: %w", b.name, in.Rev, err)
		}
	}

	// The seeds are unpacked only now, after the last step that writes into the
	// context has finished, so no step fails on a directory the unpack made.
	rigs := []string{BuildRig}
	for _, src := range in.Seeds {
		if src.Rig != BuildRig {
			rigs = append(rigs, src.Rig)
		}
	}
	for _, rig := range rigs {
		if err := unpackInto(in.Out, rig, trees[rig]); err != nil {
			return Result{}, err
		}
	}
	floor, err := goFloor(in.Out, rigs)
	if err != nil {
		return Result{}, err
	}
	res.GoFloor = floor

	script, err := s.Git(in.Src, "show", in.Rev+":deploy/polecat/go-floor.sh")
	if err != nil {
		return Result{}, fmt.Errorf("imagectx: cannot read deploy/polecat/go-floor.sh at %s: %w", in.Rev, err)
	}
	if err := os.WriteFile(filepath.Join(in.Out, "go-floor.sh"), script, 0o755); err != nil {
		return Result{}, fmt.Errorf("imagectx: cannot write go-floor.sh: %w", err)
	}
	if err := os.WriteFile(filepath.Join(in.Out, "bd.commit"), []byte(bdCommit+"\n"), 0o644); err != nil {
		return Result{}, fmt.Errorf("imagectx: cannot write bd.commit: %w", err)
	}
	return res, nil
}

// checkInputs refuses an input the build cannot use, before any command runs.
func checkInputs(in Inputs) error {
	if in.Src == "" {
		return errors.New("imagectx: no source checkout named")
	}
	if !fullSHA.MatchString(in.Rev) {
		return fmt.Errorf("imagectx: --rev %q is not a full 40-character sha", in.Rev)
	}
	if !filepath.IsAbs(in.Out) {
		return fmt.Errorf("imagectx: --out %q is not absolute", in.Out)
	}
	entries, err := os.ReadDir(in.Out)
	if err != nil {
		return fmt.Errorf("imagectx: cannot read the output directory %s: %w", in.Out, err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("imagectx: the output directory %s holds %d entries; the context needs an empty directory", in.Out, len(entries))
	}
	if in.BD.Name != "bd" {
		return fmt.Errorf("imagectx: the bd pin is named %q, want bd", in.BD.Name)
	}
	if !fullSHA.MatchString(in.BD.Commit) {
		return fmt.Errorf("imagectx: the bd pin commit %q is not a full 40-character sha", in.BD.Commit)
	}
	if _, ok := in.SeedURLs[BuildRig]; ok {
		return fmt.Errorf("imagectx: seed-urls names %s, the build rig, which is seeded from --rev and takes no URL", BuildRig)
	}
	seeded := map[string]bool{}
	for _, src := range in.Seeds {
		// A rig name becomes a path under the context, so it is a plain name
		// before it is joined to anything.
		if !rigName.MatchString(src.Rig) {
			return fmt.Errorf("imagectx: seed-pins names rig %q, which is not a plain rig name", src.Rig)
		}
		if src.Rig == BuildRig {
			continue
		}
		if seeded[src.Rig] {
			return fmt.Errorf("imagectx: seed-pins names rig %s twice", src.Rig)
		}
		seeded[src.Rig] = true
		if !fullSHA.MatchString(src.Commit) {
			return fmt.Errorf("imagectx: seed pin for rig %s is not a full 40-character sha", src.Rig)
		}
		if _, ok := in.SeedURLs[src.Rig]; !ok {
			return fmt.Errorf("imagectx: seed-urls names no URL for seeded rig %s", src.Rig)
		}
	}
	for rig := range in.SeedURLs {
		if !seeded[rig] {
			return fmt.Errorf("imagectx: seed-urls names rig %s, which seed-pins does not seed", rig)
		}
	}
	return nil
}

// checkCleanAt refuses a Src whose tree has changes or whose HEAD is not Rev: the
// build label names Rev, so the binaries must be the tree at Rev. The status
// ignores .beads, as the Makefile's guard always has.
func checkCleanAt(in Inputs, git bdbuild.Git) error {
	status, err := git(in.Src, "status", "--porcelain", "--", ".", ":(exclude).beads")
	if err != nil {
		return fmt.Errorf("imagectx: cannot read the status of %s: %w", in.Src, err)
	}
	if strings.TrimSpace(string(status)) != "" {
		return fmt.Errorf("imagectx: the tree at %s has uncommitted or untracked changes, so its gt is not the commit the label names (%s); commit or remove them first", in.Src, in.Rev)
	}
	head, err := git(in.Src, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return fmt.Errorf("imagectx: cannot read HEAD of %s: %w", in.Src, err)
	}
	if got := strings.TrimSpace(string(head)); got != in.Rev {
		return fmt.Errorf("imagectx: HEAD of %s is %s, not --rev %s; check out the commit the image is built at", in.Src, got, in.Rev)
	}
	return nil
}

// stageRig fetches one seeded rig at its pin by URL, reads its main by URL,
// applies the pin-drift guard to the pin against that main, and archives the
// pinned tree. It writes nothing into the context: Build unpacks the tree once
// every rig has passed. The fetch directory is removed on every path. The
// returned note is the guard's note, empty when the pin is main.
func stageRig(ctx context.Context, in Inputs, s Seams, env []string, src seed.Source) (string, []byte, error) {
	url := in.SeedURLs[src.Rig]
	tmp, err := os.MkdirTemp(in.Out, ".imagectx-"+src.Rig+"-")
	if err != nil {
		return "", nil, fmt.Errorf("imagectx: cannot make a fetch directory for rig %s in %s: %w", src.Rig, in.Out, err)
	}
	defer os.RemoveAll(tmp)

	// FetchSource refuses a URL with a credential before any command runs, so
	// the ls-remote below is only ever given a credential-free URL.
	pin := daemonimage.Pin{Name: src.Rig, URL: url, Commit: src.Commit}
	if err := daemonimage.FetchSource(ctx, s.Run, s.Git, tmp, env, pin); err != nil {
		return "", nil, err
	}
	// The read goes through GitEnv, so it runs with the build's environment:
	// GIT_TERMINAL_PROMPT=0 makes a missing credential fail the read instead of
	// waiting on a terminal.
	out, err := s.GitEnv(tmp, env, "ls-remote", url, "refs/heads/main")
	if err != nil {
		return "", nil, fmt.Errorf("imagectx: cannot read the main of rig %s from its URL: %w", src.Rig, err)
	}
	main, err := mainFromLsRemote(out)
	if err != nil {
		return "", nil, fmt.Errorf("imagectx: rig %s: %w", src.Rig, err)
	}
	if main != src.Commit {
		// The history back to the pin is fetched whole, so the guard's
		// ancestry and behind counts read the real history, not a shallow cut.
		if err := s.Run(ctx, tmp, env, []string{"git", "fetch", "--unshallow", url, "refs/heads/main"}); err != nil {
			return "", nil, fmt.Errorf("imagectx: cannot fetch the main of rig %s from its URL: %w", src.Rig, err)
		}
		fetched, err := s.Git(tmp, "rev-parse", "--verify", "FETCH_HEAD^{commit}")
		if err != nil {
			return "", nil, fmt.Errorf("imagectx: cannot read the main fetched for rig %s: %w", src.Rig, err)
		}
		if got := strings.TrimSpace(string(fetched)); got != main {
			return "", nil, fmt.Errorf("imagectx: the main of rig %s moved from %s to %s while it was fetched; rerun", src.Rig, short(main), short(got))
		}
	}
	note, err := seed.CheckAgainstMain(src.Rig, src.Commit, main, "the fetched "+src.Rig+" source", tmp, seed.Git(s.Git))
	if err != nil {
		return "", nil, err
	}
	tree, err := archiveTree(tmp, src.Commit, s.Git)
	if err != nil {
		return "", nil, err
	}
	return note, tree, nil
}

// mainFromLsRemote reads the one line `git ls-remote <url> refs/heads/main`
// prints: a full sha and that ref. Its output is never echoed in an error.
func mainFromLsRemote(out []byte) (string, error) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) != 1 {
		return "", fmt.Errorf("the source answered %d refs for main, want one", len(lines))
	}
	fields := strings.Fields(lines[0])
	if len(fields) != 2 || fields[1] != "refs/heads/main" || !fullSHA.MatchString(fields[0]) {
		return "", errors.New("the source's main ref is not a full sha")
	}
	return fields[0], nil
}

// buildBD fetches bd at its pin by URL and compiles it there. The fetch
// directory and the compile's worktree are both removed before it returns.
func buildBD(ctx context.Context, in Inputs, s Seams, env []string) (string, error) {
	tmp, err := os.MkdirTemp(in.Out, ".imagectx-bd-")
	if err != nil {
		return "", fmt.Errorf("imagectx: cannot make a fetch directory for bd in %s: %w", in.Out, err)
	}
	defer os.RemoveAll(tmp)
	if err := daemonimage.FetchSource(ctx, s.Run, s.Git, tmp, env, in.BD); err != nil {
		return "", err
	}
	full, err := bdbuild.Compile(bdbuild.Options{
		Src:      tmp,
		Out:      filepath.Join(in.Out, "bin", "bd"),
		Commit:   in.BD.Commit,
		BuildDir: in.Out,
	}, s.Git, s.Go)
	if err != nil {
		return "", err
	}
	if full != in.BD.Commit {
		return "", fmt.Errorf("imagectx: bd built at %s, not its pin %s", short(full), short(in.BD.Commit))
	}
	return full, nil
}

// archiveTree reads the tree of commit in checkout as a `git archive` stream, the
// form the seed stage reads it in. Nothing is written.
func archiveTree(checkout, commit string, git bdbuild.Git) ([]byte, error) {
	tree, err := git(checkout, "archive", "--format=tar", commit)
	if err != nil {
		return nil, fmt.Errorf("imagectx: cannot archive %s from %s: %w", short(commit), checkout, err)
	}
	return tree, nil
}

// unpackInto writes a rig's archived tree into the seed directory under out. It
// is seed.Unpack, which refuses a tree that would write outside that directory.
func unpackInto(out, rig string, tree []byte) error {
	dir := filepath.Join(out, "seed", rig)
	if err := seed.Unpack(dir, tree); err != nil {
		return fmt.Errorf("imagectx: cannot unpack the %s tree into %s: %w", rig, dir, err)
	}
	return nil
}

// goFloor is the highest `go` line of the top-level go.mod of each rig in rigs
// that has one, as go-floor.sh rig computes it. A rig with no go.mod is skipped;
// no go line at all is an error, never a floor of "".
func goFloor(out string, rigs []string) (string, error) {
	var versions []string
	for _, rig := range rigs {
		path := filepath.Join(out, "seed", rig, "go.mod")
		data, err := os.ReadFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return "", fmt.Errorf("imagectx: cannot read the go.mod of rig %s: %w", rig, err)
		}
		f, err := modfile.Parse(path, data, nil)
		if err != nil {
			return "", fmt.Errorf("imagectx: cannot parse the go.mod of rig %s: %w", rig, err)
		}
		if f.Go != nil {
			versions = append(versions, f.Go.Version)
		}
	}
	var floor string
	var floorParts []int
	for _, v := range versions {
		parts, err := parseVersion(v)
		if err != nil {
			return "", err
		}
		if floorParts == nil || compareVersions(parts, floorParts) > 0 {
			floor, floorParts = v, parts
		}
	}
	if floorParts == nil {
		return "", errors.New("imagectx: no rig go.mod has a go line, so there is no Go floor")
	}
	return floor, nil
}

// parseVersion splits a go line's version into its numbers, refusing a form
// go-floor.sh refuses too.
func parseVersion(v string) ([]int, error) {
	if !goVersion.MatchString(v) {
		return nil, fmt.Errorf("imagectx: go line %q is not a version", v)
	}
	var parts []int
	for _, p := range strings.Split(v, ".") {
		n, err := strconv.Atoi(p)
		if err != nil {
			return nil, fmt.Errorf("imagectx: go line %q is not a version", v)
		}
		parts = append(parts, n)
	}
	return parts, nil
}

// compareVersions orders two parsed versions numerically, so 1.10 is above 1.9.
func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		x, y := 0, 0
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// buildEnv is the environment the compiles start with: the host's toolchain
// variables and the cross-compile settings bdbuild.Env keeps, with git told
// never to prompt, so a fetch or a read that wants a credential fails rather
// than waiting on a terminal. The fetches and the ls-remote read use gitEnv,
// which adds the credential-helper variables a compile must not see.
func buildEnv() []string {
	return buildEnvFrom(nil)
}

// buildEnvFrom is buildEnv for the host environment environ; nil reads the
// process's own.
func buildEnvFrom(environ []string) []string {
	return append(bdbuild.Env(environ), "GIT_TERMINAL_PROMPT=0")
}

// gitCredentialVars names the host variables a git fetch needs to reach a
// private source: the config file that installs a credential helper
// (GIT_CONFIG_GLOBAL) and the two variables that helper prints. bdbuild.Env
// drops them on purpose, because a compile must not see them; git does need
// them, or the helper is gone and a private repository refuses the fetch with
// "could not read Username" (gt-y1p1np).
var gitCredentialVars = []string{"GIT_CONFIG_GLOBAL", "SOURCES_USER", "SOURCES_TOKEN"}

// gitEnv is the environment the fetches and the ls-remote read run with:
// buildEnv, plus gitCredentialVars when environ holds them with a value. Only
// the names are chosen here; the values travel inside the child's environment
// and are never put in an argv, a plan line or an error.
func gitEnv(environ []string) []string {
	env := buildEnvFrom(environ)
	for _, kv := range environ {
		key, val, ok := strings.Cut(kv, "=")
		if !ok || val == "" {
			continue
		}
		for _, name := range gitCredentialVars {
			if key == name {
				env = append(env, kv)
			}
		}
	}
	return env
}

// ExecRun runs argv in dir with env as its whole environment, and stops it past
// runCeiling. A failure names the command and the last line of its stderr.
func ExecRun(ctx context.Context, dir string, env, argv []string) error {
	ctx, cancel := context.WithTimeout(ctx, runCeiling)
	defer cancel()
	return runArgv(ctx, dir, env, argv)
}

// runArgv runs argv under ctx, which kills it when ctx ends. A command ctx ends
// fails naming the timeout or the cancellation; any other failure names the
// command and the last line of its stderr.
func runArgv(ctx context.Context, dir string, env, argv []string) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	// A fetch leaves a git-remote-https child holding the output pipes, so Wait
	// would block on it after git is killed; WaitDelay bounds that wait.
	cmd.WaitDelay = time.Second
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		switch {
		case errors.Is(ctx.Err(), context.DeadlineExceeded):
			return fmt.Errorf("%s timed out: %w", argv[0], ctx.Err())
		case errors.Is(ctx.Err(), context.Canceled):
			return fmt.Errorf("%s was canceled: %w", argv[0], ctx.Err())
		}
		return fmt.Errorf("%s failed: %w: %s", argv[0], err, lastLine(stderr.String()))
	}
	return nil
}

// lastLine is the last non-blank line of s: a tool's reason for a failure.
func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "no output"
	}
	lines := strings.Split(s, "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// short is the abbreviation a refusal prints for a commit.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
