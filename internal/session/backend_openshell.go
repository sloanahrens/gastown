package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/buildpaths"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/diskmaint/registry"
	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/sandbox"
	"github.com/steveyegge/gastown/internal/tmux"
	"github.com/steveyegge/gastown/internal/version"
)

func init() { NewOpenShellBackend = newOpenShellBackend }

const (
	sandboxHome             = "/sandbox"        // the sandbox's root; uploads land here
	sandboxTownRoot         = "/sandbox/gt"     // the skeleton's town root inside the sandbox
	sandboxCacheDir         = "/sandbox/.cache" // the cache volume's mount point
	sandboxDoltPort         = "3307"
	defaultControlPlaneHost = "host.docker.internal"
	skeletonArchive         = "town-skel.tgz"
	workspaceArchiveName    = "workspace.tgz"
	readyInterval           = 2 * time.Second
	readyPolls              = 150 // five minutes at readyInterval
)

// sandboxDroppedEnv are the host's credential and host-only variables. An
// empty value in Prepared.Env drops the key (session.Prepared).
var sandboxDroppedEnv = []string{"CLAUDE_CONFIG_DIR", "TMPDIR", "GOTMPDIR", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"}

// cloneScript clones the polecat branch with the Forgejo token the provider
// put in the sandbox's environment, then fetches main into origin/main: the
// single-branch clone's refspec covers only the polecat branch, and gt done
// compares the branch with origin/main. Both fetches are shallow ($4 commits):
// the full history of the town's largest repo took about 10 s and 88 MB to
// clone, 1.3 s and 13 MB at depth 500, and everything gt done does with history
// (merge-base against origin/main, rebase onto it, rev-list counts, patch-id,
// pushing the new commits) needs only the commits since the branch forked from
// main. When main has moved further than the depth since the fork, the fork
// point falls outside origin/main's shallow history and merge-base fails; the
// script then refetches both refs in full, so the depth is a cost bound and not
// a cap on how far behind a branch may fall. Arguments are positional, so no
// value is spliced into the script text.
const cloneScript = `H="http.extraHeader=Authorization: token $FORGEJO_TOKEN"; ` +
	`git -c "$H" clone --branch "$1" --single-branch --depth "$4" "$2" "$3" && cd "$3" && ` +
	`git remote set-branches --add origin main && git -c "$H" fetch --depth "$4" origin main && ` +
	`(git merge-base HEAD origin/main >/dev/null 2>&1 || ` +
	`(git -c "$H" fetch --unshallow origin && git merge-base HEAD origin/main >/dev/null 2>&1))`

// sandboxCloneDepth is the history a sandbox clone takes. A polecat branch
// forks a few hours behind main's tip at most (about a hundred landings a day),
// so this covers the fork point with room to spare; cloneScript refetches in
// full when it does not.
const sandboxCloneDepth = 1000

// redirectScript writes .beads/redirect in the clone. $1 is the worktree and
// $2 the redirect target.
const redirectScript = `mkdir -p "$1/.beads" && printf '%s\n' "$2" > "$1/.beads/redirect"`

// sandboxHolderSession names the tmux session the main process creates to
// start the server. It must never equal a polecat session id: the session
// start creates that one on this server, and tmux refuses a duplicate.
const sandboxHolderSession = "boot"

// sandboxMainScript is the sandbox's main process, created with --no-keep so the
// gateway deletes the sandbox when it exits. It starts the tmux server the host's
// exec'd commands talk to, waits for the host to start the agent's session on it,
// and exits once the session has been seen gone, or all its panes dead, on three
// polls in a row. A poll counts only on a positive answer: the panes were read
// and none is alive, or tmux's server answered and has no such session. A poll
// that reads a live pane resets that count. When tmux cannot be asked at all the
// answer is unknown: the count is left as it is, so a transient failure never
// deletes a live sandbox. A tmux that stays unreachable for sandboxTmuxLostPolls
// polls in a row has lost the agent with it, and the script exits so the sandbox
// does not outlive it. It exits 1 when the session never appears, so a launch
// that failed after the sandbox was made also ends the sandbox.
//
// Positional arguments: $1 the session name; $2 seconds to wait for the session
// (default sandboxSessionWaitSeconds); $3 the poll interval in seconds (2); $4
// the tmux start directory (the sandbox home); $5 the unreachable-poll limit
// (default sandboxTmuxLostPolls). Only $1 is set in production.
const sandboxMainScript = `S="$1"; W="${2:-` + sandboxSessionWaitSeconds + `}"; P="${3:-2}"; U="${5:-` + sandboxTmuxLostPolls + `}"; T="tmux -L ` + SandboxTmuxSocket + `"; ` +
	`$T new-session -d -s ` + sandboxHolderSession + ` -c "${4:-` + sandboxHome + `}" || exit 1; ` +
	`i=0; until $T has-session -t "=$S" 2>/dev/null; do i=$((i+1)); [ "$i" -gt "$W" ] && exit 1; sleep 1; done; ` +
	`gone=0; unk=0; while [ "$gone" -lt 3 ] && [ "$unk" -lt "$U" ]; do ` +
	`if out=$($T list-panes -t "=$S" -F '#{pane_dead}' 2>/dev/null); then unk=0; ` +
	`if printf '%s\n' "$out" | grep -qx 0; then gone=0; else gone=$((gone+1)); fi; ` +
	`elif $T list-sessions >/dev/null 2>&1; then unk=0; gone=$((gone+1)); ` +
	`else unk=$((unk+1)); fi; ` +
	`sleep "$P"; done; exit 0`

// sandboxTmuxLostPolls is how many polls in a row tmux may stay unreachable
// (about five minutes at the 2 s interval) before the main process exits.
const sandboxTmuxLostPolls = "150"

// sandboxSessionWaitSeconds is how long the main process waits for the agent's
// session to appear.
const sandboxSessionWaitSeconds = "900"

// openshellBackend hosts one polecat's session in an OpenShell sandbox named
// for the session. Its tmux client runs every command through
// `openshell sandbox exec`, so nudges, liveness and the rest reach the
// sandbox's own tmux server.
type openshellBackend struct {
	cli      *sandbox.CLI
	townRoot string
	profile  Profile
	wait     func(ctx context.Context, d time.Duration) error
	render   func(sandbox.SkeletonSpec) ([]byte, error)
	push     func(bareRepo, branch string) error
	tmux     *tmux.Tmux // the routed client for the session Prepare made
	// probe is the tmux surface existence and liveness read for a session;
	// nil means the routed client that runs inside the sandbox. Tests set it.
	probe func(session string) liveness.Probe
	// imageGT reads the gt commit an image was built from, and installedGT is
	// the commit of the gt running Prepare. Tests set both.
	imageGT func(ctx context.Context, image string) (string, error)
	// imageRef resolves image to the reference the create names and the local
	// image the label check reads, for a gateway that pulls its images from a
	// registry or one that runs them from the host's docker (gt-16oj9, gt-jbuow).
	// Tests set it.
	imageRef func(ctx context.Context, image string, pull bool) (resolvedImage, error)
	// docker runs the docker CLI for the profile's cache volume. Tests set it.
	docker dockerRun
	// git compares the image's gt commit with the installed one. Tests set it.
	git gitRun
	// policyRoot is the town's gt source checkout, the repo a sandbox profile's
	// policy path is relative to. A polecat's worktree is the wrong base: only
	// the gt repo carries deploy/polecat/policy.yaml, and a sandbox seat serves
	// the town's other rigs too.
	policyRoot func(townRoot string) (string, error)
	// gitEmail reads the host's global git user.email, the address the sandbox's
	// commits carry (host polecats commit with it too); "" when it is unset.
	gitEmail func() string
	// renderPrompt renders the sandbox's system prompt bytes on the host; nil means the installed
	// renderer, read at the spawn (gt-uvwi6).
	renderPrompt config.SystemPromptRender
	// comments posts the spawn's prompt stamp on the work bead (gt-2lu00).
	// Tests set it.
	comments    promptCommentStore
	installedGT func() string
	// phaseWatch is how long this session's gateway phase has been a terminal
	// failure. Nil is the process's watch (sandboxPhases); tests set one.
	phaseWatch *sandboxPhaseWatch
}

func newOpenShellBackend(townRoot string, p Profile) (Backend, error) {
	return &openshellBackend{
		cli:         &sandbox.CLI{Gateway: p.Gateway},
		townRoot:    townRoot,
		profile:     p,
		wait:        waitFor,
		render:      sandbox.RenderSkeleton,
		push:        pushToForgejo,
		imageGT:     dockerImageGT,
		imageRef:    dockerImageRef,
		docker:      runDocker,
		git:         runGit,
		gitEmail:    hostGitEmail,
		installedGT: version.BuildCommit,
		policyRoot:  version.GetRepoRootForTown,
		comments:    beads.New(townRoot),
	}, nil
}

// gtCommitLabel is the image label `make polecat-image` sets to the commit its
// gt was built from.
const gtCommitLabel = "org.gastown.gt-commit"

// dockerRun runs the docker CLI with args and returns its stdout, its stderr
// and the run error. Tests supply their own, so the label read is tested
// without docker.
type dockerRun func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

// dockerImageGT reads the gt commit label of image from the host's docker. An
// image without the label reads as "", which checkImageGT refuses.
func dockerImageGT(ctx context.Context, image string) (string, error) {
	return imageGTFrom(ctx, runDocker, image)
}

// runDocker is the real docker runner.
func runDocker(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	return out, stderr.Bytes(), err
}

// imageGTFrom reads the gt commit label of image through run. Only stdout
// carries the label: a warning docker prints on stderr must not read as a
// commit (gt-pmxo9).
func imageGTFrom(ctx context.Context, run dockerRun, image string) (string, error) {
	format := `{{index .Config.Labels "` + gtCommitLabel + `"}}`
	out, stderr, err := run(ctx, "image", "inspect", "--format", format, image)
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w: %s", image, err, strings.TrimSpace(string(stderr)))
	}
	return strings.TrimSpace(string(out)), nil
}

// imageIDFormat prints the image's id: the content digest docker holds for a
// reference.
const imageIDFormat = `{{.Id}}`

// imageIDPattern is a digest an image id can be: sha256 and 64 lowercase hex
// digits.
var imageIDPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// digestRefPattern matches a reference that already names its digest.
var digestRefPattern = regexp.MustCompile(`^[^@]+@sha256:[0-9a-f]{64}$`)

// driverPullsImage reports whether the gateway runs the image by pulling its
// reference from a registry rather than from the host's Docker daemon. An unset
// driver is the docker one (SandboxProfile.WithDefaults).
func driverPullsImage(driver string) bool { return driver == DriverKubernetes }

// resolvedImage is the image a Prepare checks and creates. Local is the id the
// host's docker holds for the profile's reference, which the gt label check
// reads. Create is the reference the sandbox create names: Local for a gateway
// that runs images from the host's daemon, the registry's manifest digest for
// one that pulls them (gt-jbuow).
type resolvedImage struct {
	Local, Create string
}

// manifestDigester reads the manifest digest the registry at host holds for
// repo:tag.
type manifestDigester func(ctx context.Context, host, repo, tag string) (string, error)

// registryDigester is the manifestDigester for the registry client, reached over
// plain HTTP at the host the profile's reference names. A nil client is the
// registry client's default; tests inject one whose transport is a fake.
func registryDigester(client *http.Client) manifestDigester {
	return func(ctx context.Context, host, repo, tag string) (string, error) {
		c := &registry.Client{Base: "http://" + host, HTTP: client}
		m, err := c.GetManifest(ctx, repo, tag)
		if err != nil {
			return "", err
		}
		return m.Digest, nil
	}
}

// dockerImageRef resolves an image through the host's docker and, for a pulling
// gateway, the registry the image names: imageRefFor.
func dockerImageRef(ctx context.Context, image string, pull bool) (resolvedImage, error) {
	return imageRefFor(ctx, runDocker, registryDigester(nil), image, pull)
}

// imageRefFor resolves image to the references Prepare names: the local image
// the label check reads and the reference the create names. Both are read from
// the same call, so a rebuild that retags the profile's image between Prepare's
// check and its create cannot put an image nothing checked under the sandbox
// (gt-16oj9).
//
// Docker is asked about image on every path, so a reference the host does not
// have fails here rather than after the push.
//
// A gateway running images from the host's daemon (pull false) is given the
// local image id: the content, which a retag cannot move. A gateway that pulls
// from a registry cannot use an id, so it is given the manifest digest the
// registry holds for the image's tag, read from the registry the reference names
// (gt-jbuow). A tag that moves after that read leaves the sandbox on the checked
// digest. A registry that does not answer, answers with no digest, or answers
// with one that is not a sha256 manifest digest refuses the spawn; the tag is
// never passed on in its place.
func imageRefFor(ctx context.Context, run dockerRun, digest manifestDigester, image string, pull bool) (resolvedImage, error) {
	id, err := imageIDFrom(ctx, run, image)
	if err != nil {
		return resolvedImage{}, err
	}
	if digestRefPattern.MatchString(image) {
		return resolvedImage{Local: id, Create: image}, nil
	}
	if !pull {
		return resolvedImage{Local: id, Create: id}, nil
	}
	ref, err := pulledImageRef(ctx, digest, image)
	if err != nil {
		return resolvedImage{}, err
	}
	return resolvedImage{Local: id, Create: ref}, nil
}

// pulledImageRef names image by the manifest digest its registry holds for its
// tag: repository@sha256:..., the reference a pod pulls by content.
func pulledImageRef(ctx context.Context, digest manifestDigester, image string) (string, error) {
	host, repo, tag, err := splitImage(image)
	if err != nil {
		return "", err
	}
	d, err := digest(ctx, host, repo, tag)
	if err != nil {
		return "", fmt.Errorf("pinning %s by its registry digest: %w", image, err)
	}
	if !registry.ValidDigest(d) {
		return "", fmt.Errorf("registry %s answered %q for %s, not a manifest digest", host, d, image)
	}
	return host + "/" + repo + "@" + d, nil
}

// splitImage splits a pulled reference into the registry host it names and the
// repository and tag inside that registry. The first path element is a host
// when it holds a dot or a colon or is localhost, docker's own rule; a reference
// without one names no registry to ask, and is refused. A reference with no tag
// names latest, as docker reads it.
func splitImage(image string) (host, repo, tag string, err error) {
	first, rest, ok := strings.Cut(image, "/")
	if !ok || !(strings.ContainsAny(first, ".:") || first == "localhost") {
		return "", "", "", fmt.Errorf("%s names no registry host to pin it by", image)
	}
	repo, tag = rest, "latest"
	if i := strings.LastIndex(rest, ":"); i > strings.LastIndex(rest, "/") {
		repo, tag = rest[:i], rest[i+1:]
	}
	return first, repo, tag, nil
}

// imageIDFrom reads the content digest docker holds for image, refusing a reply
// that is not one.
func imageIDFrom(ctx context.Context, run dockerRun, image string) (string, error) {
	out, stderr, err := run(ctx, "image", "inspect", "--format", imageIDFormat, image)
	if err != nil {
		return "", fmt.Errorf("docker image inspect %s: %w: %s", image, err, strings.TrimSpace(string(stderr)))
	}
	id := strings.TrimSpace(string(out))
	if !imageIDPattern.MatchString(id) {
		return "", fmt.Errorf("docker image inspect %s printed %q, not an image digest", image, id)
	}
	return id, nil
}

// gitRun runs the git CLI in dir and returns its stdout. Tests supply their
// own, so no unit test runs git (internal/testpolicy keeps this package
// git-free).
type gitRun func(dir string, args ...string) ([]byte, error)

// gitRepoEnvVars name a repository other than the one a command runs in. A
// session started inside a worktree or a hook carries them, and a comparison
// that inherited them would read the caller's repository instead of the
// town's gt source checkout (gt-aq1d1).
var gitRepoEnvVars = map[string]bool{
	"GIT_DIR":                          true,
	"GIT_WORK_TREE":                    true,
	"GIT_INDEX_FILE":                   true,
	"GIT_COMMON_DIR":                   true,
	"GIT_OBJECT_DIRECTORY":             true,
	"GIT_ALTERNATE_OBJECT_DIRECTORIES": true,
	"GIT_NAMESPACE":                    true,
	"GIT_PREFIX":                       true,
}

// gitEnv is environ without the variables that pick a repository. Everything
// else passes through, so git keeps the caller's configuration.
func gitEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if gitRepoEnvVars[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// gitCommand is the git command runGit runs: dir in environ, with the
// repository-selecting variables removed. It stands apart from runGit so a
// test can read the command it builds without running git.
func gitCommand(environ []string, dir string, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv(environ)
	return cmd
}

// runGit is the real git runner for the town's gt source checkout.
func runGit(dir string, args ...string) ([]byte, error) {
	return gitCommand(os.Environ(), dir, args...).Output()
}

// gtImageDiff reports the paths changed between an image's gt commit and the
// installed gt's. An error means the two could not be compared, which the
// caller reads as drift.
type gtImageDiff func(imageCommit, installed string) ([]string, error)

// commitIDPattern is a commit id git can take as a revision: 40 hex digits, or
// an abbreviation down to the 7 two ids need to be told apart
// (version.SameCommit). The image's commit is written by the image, in a docker
// label and in its `gt version` output, so a value git would read as an option
// never reaches its argv (gt-aq1d1).
var commitIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)

// errNotAncestor is imagePathsBetween's refusal for two commits that are not on
// one line of history: git judged the image drifted, so the refusal still names
// the rebuild. Any other failure means git could not run the comparison at all,
// and the refusal reports that reason instead (gt-aq1d1).
var errNotAncestor = errors.New("the image's gt commit is not an ancestor of the installed gt's commit")

// imagePathsBetween lists the paths whose content differs between from and to,
// through run. A pair that is not in an ancestor relationship is an error
// wrapping errNotAncestor: a commit off the installed line is not the installed
// gt missing a few commits, so it is rebuilt like any drifted one. --no-renames
// puts both sides of a rename in the list, so a source file renamed to a
// documentation path still reads as a build change.
func imagePathsBetween(run gitRun, root, from, to string) ([]string, error) {
	if !commitIDPattern.MatchString(from) {
		return nil, fmt.Errorf("the image's gt commit %q is not a commit id", from)
	}
	if !commitIDPattern.MatchString(to) {
		return nil, fmt.Errorf("the installed gt's commit %q is not a commit id", to)
	}
	if _, err := run(root, "merge-base", "--is-ancestor", from, to); err != nil {
		return nil, fmt.Errorf("%w: %s is not an ancestor of %s: %v", errNotAncestor, from, to, err)
	}
	out, err := run(root, "diff", "--name-only", "--no-renames", from, to)
	if err != nil {
		return nil, fmt.Errorf("listing the paths changed between %s and %s: %w", from, to, err)
	}
	var paths []string
	for _, line := range strings.Split(string(out), "\n") {
		if p := strings.TrimSpace(line); p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// imageDiff answers the ancestry rule from the town's gt source checkout, the
// repository the image's gt commit was built from.
func (b *openshellBackend) imageDiff(imageCommit, installed string) ([]string, error) {
	root, err := b.policyRoot(b.townRoot)
	if err != nil {
		return nil, err
	}
	return imagePathsBetween(b.git, root, imageCommit, installed)
}

// ancestryWithoutBuildChange reports whether the installed gt could be the
// image's gt plus commits that change no path `make build` reads
// (buildpaths.AffectsBuild), so the sandbox runs the same gt in every way it
// can observe. An empty listing for two commits that differ reports false with
// no error, and the caller refuses the image as drift. A comparison git could
// not make reports false with its reason, so the caller names that reason
// instead of sending the operator to rebuild an image git never judged
// (gt-aq1d1).
func ancestryWithoutBuildChange(diff gtImageDiff, imageCommit, installed string) (bool, error) {
	if diff == nil {
		return false, nil
	}
	paths, err := diff(imageCommit, installed)
	if err != nil {
		return false, err
	}
	if len(paths) == 0 {
		return false, nil
	}
	for _, p := range paths {
		if buildpaths.AffectsBuild(p) {
			return false, nil
		}
	}
	return true, nil
}

// checkImageGT refuses a sandbox image whose gt is not the installed gt's
// commit, so a drifted image fails rather than becoming a settings warning
// inside a sandbox (gt-pmxo9). An image only non-build commits behind is not
// drift: a manual install at a docs-only HEAD leaves the image's gt the same
// binary, and refusing it would force a full rebuild (gt-v18z6). source says
// where imageCommit was read, so a missing commit names the place to fix. A
// comparison git could not make is refused with its own reason, because the
// rebuild instruction would misdirect an operator whose checkout is the fault
// (gt-aq1d1).
func checkImageGT(image, source, imageCommit, installed string, diff gtImageDiff) error {
	switch {
	case installed == "":
		return fmt.Errorf("sandbox image %s: the installed gt records no build commit, so its gt cannot be checked; rebuild gt with make build", image)
	case imageCommit == "":
		return fmt.Errorf("sandbox image %s %s, so its gt cannot be checked; rebuild it with make polecat-image", image, source)
	case version.SameCommit(imageCommit, installed):
		return nil
	}
	ok, err := ancestryWithoutBuildChange(diff, imageCommit, installed)
	switch {
	case ok:
		return nil
	case err != nil && !errors.Is(err, errNotAncestor):
		// runGit keeps git's stderr out of its error, so the reason holds no
		// command output.
		return &imageDriftError{msg: fmt.Sprintf("sandbox image %s carries gt commit %s, but the installed gt is commit %s, and the two could not be compared: %v; check the town's gt source checkout, then spawn again", image, imageCommit, installed, err)}
	}
	return &imageDriftError{msg: fmt.Sprintf("sandbox image %s carries gt commit %s, but the installed gt is commit %s; rebuild the image with make polecat-image", image, imageCommit, installed)}
}

// ErrImageDrift identifies a sandbox image whose gt is not the installed gt's
// commit. It is a precondition of the sandbox seat, cleared by rebuilding the
// image, not a fault of the work a caller was starting: an automatic dispatcher
// defers the work instead of failing it.
var ErrImageDrift = errors.New("sandbox image gt differs from the installed gt")

// imageDriftError is checkImageGT's refusal. Its message is the operator's
// instruction to rebuild; errors.Is matches ErrImageDrift.
type imageDriftError struct{ msg string }

func (e *imageDriftError) Error() string { return e.msg }

func (e *imageDriftError) Is(target error) bool { return target == ErrImageDrift }

// ErrCacheVolume identifies a per-rig cache volume that is missing and could
// not be created. Like ErrImageDrift it is a precondition of the sandbox seat,
// not a fault of the work being started: an automatic dispatcher defers the
// work instead of failing it.
var ErrCacheVolume = errors.New("sandbox cache volume is missing and cannot be created")

// gtLabelSource is where the host's docker read the image's commit.
const gtLabelSource = "records no gt commit (label " + gtCommitLabel + ")"

// sandboxVersionSource is where the sandbox's own gt read its commit.
const sandboxVersionSource = "reports no gt commit in its gt version"

// sandboxVersionArgv is the in-sandbox probe for the image's gt commit: the
// sandbox's own gt names the commit its binary was built from.
var sandboxVersionArgv = []string{"gt", "version"}

// parseGTVersionCommit reads the commit out of `gt version`'s output: the last
// parenthesised group is "<build>: <branch>@<commit>" or "<build>: <commit>".
// Output in any other shape names no commit and reads as "".
func parseGTVersionCommit(out string) string {
	open := strings.LastIndex(out, "(")
	if open < 0 {
		return ""
	}
	group := out[open+1:]
	if end := strings.Index(group, ")"); end >= 0 {
		group = group[:end]
	}
	_, field, ok := strings.Cut(group, ":")
	if !ok {
		return ""
	}
	field = strings.TrimSpace(field)
	if _, commit, ok := strings.Cut(field, "@"); ok {
		return strings.TrimSpace(commit)
	}
	return field
}

// waitFor blocks for d or until ctx is done.
func waitFor(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// pushToForgejo pushes branch from the rig's bare repo to origin (Forgejo).
func pushToForgejo(bareRepo, branch string) error {
	return pushOrAcceptRemoteAhead(git.NewGitWithDir(bareRepo, ""), branch)
}

// pushGit is the part of *git.Git the push step uses.
type pushGit interface {
	Push(remote, branch string, force bool) error
	FetchBranchWithTimeout(remote, branch string, timeout time.Duration) error
	Rev(ref string) (string, error)
	IsAncestor(ancestor, descendant string) (bool, error)
}

// The production push step hands in *git.Git, so this fails the build if its
// methods drift from pushGit.
var _ pushGit = (*git.Git)(nil)

// remoteRefreshTimeout bounds the fetch that follows a rejected push.
const remoteRefreshTimeout = 2 * time.Minute

// isNonFastForwardRejection reports whether git refused the push because the
// remote branch holds commits the local one lacks. Git names that refusal with
// a "[rejected]" ref line. A remote hook refusal reads "[remote rejected]", and
// an auth or network failure has no ref line, so neither is a rejection.
func isNonFastForwardRejection(err error) bool {
	return strings.Contains(err.Error(), "[rejected]")
}

// pushOrAcceptRemoteAhead pushes branch to origin. A sandbox pushes its commits
// straight to Forgejo, so a seat that is started again finds the host branch
// behind its remote counterpart and the push is rejected non-fast-forward. When
// the local tip is an ancestor of the remote one the remote is simply ahead and
// the clone inside the sandbox will fetch it, so that counts as pushed. Every
// other outcome returns the push's own error, and nothing is forced (gt-gii34).
func pushOrAcceptRemoteAhead(g pushGit, branch string) error {
	pushErr := g.Push("origin", branch, false)
	if pushErr == nil {
		return nil
	}
	if !isNonFastForwardRejection(pushErr) {
		return pushErr
	}
	if err := g.FetchBranchWithTimeout("origin", branch, remoteRefreshTimeout); err != nil {
		return pushErr
	}
	local, err := g.Rev("refs/heads/" + branch)
	if err != nil {
		return pushErr
	}
	remote, err := g.Rev("refs/remotes/origin/" + branch)
	if err != nil {
		return pushErr
	}
	if ahead, err := g.IsAncestor(local, remote); err != nil || !ahead {
		return pushErr
	}
	return nil
}

func (b *openshellBackend) Name() string     { return BackendOpenShell }
func (b *openshellBackend) Tmux() *tmux.Tmux { return b.tmux }

// Prepare makes the sandbox for spec.Session and fills it: the branch goes to
// Forgejo, the sandbox is created and ready, the town skeleton and the
// runtime files are extracted, the clone is made and the record is written.
// Any failure after the create deletes the sandbox again.
func (b *openshellBackend) Prepare(ctx context.Context, spec SpawnSpec) (*Prepared, error) {
	rel, err := sandboxRel(spec)
	if err != nil {
		return nil, err
	}
	if spec.Rig == "" || spec.Polecat == "" || spec.Branch == "" || spec.RepoURL == "" {
		return nil, fmt.Errorf("sandbox %s: spec needs rig, polecat, branch and repo URL", spec.Session)
	}
	// Built before anything is pushed or created, so a bad profile touches nothing.
	create, err := b.createSpec(spec)
	if err != nil {
		return nil, err
	}
	// Resolved once, so the label check and the create name the same image even
	// if a rebuild retags the profile's reference between them (gt-16oj9). A
	// pulling gateway's create names the registry digest read here (gt-jbuow).
	image, err := b.imageRef(ctx, b.profile.Image, driverPullsImage(b.profile.Driver))
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
	}
	create.Image = image.Create
	// The image's gt must be the installed gt's commit. Checked before the push
	// and the create, so a drifted image leaves nothing behind (gt-pmxo9). This
	// reads the host docker's image for that reference; the sandbox's own gt is
	// checked again once it is Ready, because the gateway may run a different
	// image for that reference (gt-zdilt).
	imageCommit, err := b.imageGT(ctx, image.Local)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
	}
	if err := checkImageGT(b.profile.Image, gtLabelSource, imageCommit, b.installedGT(), b.imageDiff); err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
	}

	// Before the push and the create, as the image check is: a missing cache
	// volume that cannot be made leaves nothing behind (gt-td6pl). A kubernetes
	// gateway has no docker volumes, so it does none of this.
	if b.usesDockerVolumes() {
		if err := ensureCacheVolume(ctx, b.docker, cacheVolumeName(b.profile.CacheVolume, spec.Rig)); err != nil {
			return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
		}
	}

	// Also before anything is pushed or created: the agent command names these
	// files, so a polecat whose settings are missing or cannot be rendered for
	// the sandbox must stop here.
	runtime, err := buildRuntimeArchiveLogging(spec, b.gitEmail(), os.Stderr, b.promptRenderer(), b.comments)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
	}

	// The clone inside the sandbox fetches the branch, so it must exist on
	// Forgejo before the sandbox is created.
	if err := spec.Trace.Phase("push", func() error {
		return b.push(filepath.Join(spec.TownRoot, spec.Rig, ".repo.git"), spec.Branch)
	}); err != nil {
		return nil, fmt.Errorf("pushing %s to forgejo: %w", spec.Branch, err)
	}

	stage, err := b.stageDir()
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(stage)

	skeleton, err := b.render(sandbox.SkeletonSpec{
		TownRoot:         spec.TownRoot,
		Rig:              spec.Rig,
		ControlPlaneHost: b.controlPlaneHost(),
		SandboxRepoURL:   spec.RepoURL,
	})
	if err != nil {
		return nil, fmt.Errorf("rendering town skeleton: %w", err)
	}
	var staged stagedArchives
	staged.skeleton = filepath.Join(stage, skeletonArchive)
	if err := os.WriteFile(staged.skeleton, skeleton, 0o600); err != nil {
		return nil, err
	}
	staged.runtime = filepath.Join(stage, runtimeArchiveName)
	if err := os.WriteFile(staged.runtime, runtime, 0o600); err != nil {
		return nil, err
	}
	workspace, err := buildWorkspaceArchive(spec.HostWorkDir)
	if err != nil {
		return nil, fmt.Errorf("archiving runtime files: %w", err)
	}
	if workspace != nil {
		staged.workspace = filepath.Join(stage, workspaceArchiveName)
		if err := os.WriteFile(staged.workspace, workspace, 0o600); err != nil {
			return nil, err
		}
	}

	if err := spec.Trace.Phase("create", func() error { return b.cli.Create(ctx, create) }); err != nil {
		return nil, fmt.Errorf("creating sandbox %s: %w", spec.Session, err)
	}

	workDir := path.Join(sandboxTownRoot, rel)
	if err := b.populate(ctx, spec, workDir, redirectTarget(rel), staged); err != nil {
		return nil, b.rollback(ctx, spec.Session, err, spec.Trace)
	}

	b.tmux = tmux.NewTmuxWithExecPrefix(SandboxTmuxSocket, SandboxExecPrefix(b.profile.Gateway, spec.Session)).Routed()
	return &Prepared{WorkDir: workDir, Env: b.agentEnv(spec, workDir), OmitSecretPrefix: true}, nil
}

// stagedArchives are the host files Prepare uploads. workspace is empty when
// the worktree has no runtime files to send.
type stagedArchives struct {
	skeleton, runtime, workspace string
}

// populate runs everything between the create and the record: wait for
// Ready, extract the skeleton, clone, extract the runtime files, write the
// redirect, and record the sandbox so the tmux router sends its commands here.
func (b *openshellBackend) populate(ctx context.Context, spec SpawnSpec, workDir, redirect string, files stagedArchives) error {
	if err := spec.Trace.Phase("ready", func() error { return b.awaitReady(ctx, spec.Session) }); err != nil {
		return err
	}
	// The sandbox's own gt is the one that will run; Prepare's label check only
	// saw the host docker's image for the same reference (gt-zdilt).
	if err := spec.Trace.Phase("gt check", func() error { return b.checkSandboxGT(ctx, spec.Session) }); err != nil {
		return err
	}
	if err := spec.Trace.Phase("skeleton and env", func() error {
		if err := b.cli.Upload(ctx, spec.Session, files.skeleton, sandboxHome+"/"); err != nil {
			return fmt.Errorf("uploading town skeleton: %w", err)
		}
		if err := b.run(ctx, spec.Session, "tar", "xzf", path.Join(sandboxHome, skeletonArchive), "-C", sandboxHome); err != nil {
			return fmt.Errorf("extracting town skeleton: %w", err)
		}
		if err := b.cli.Upload(ctx, spec.Session, files.runtime, sandboxHome+"/"); err != nil {
			return fmt.Errorf("uploading polecat settings: %w", err)
		}
		if err := b.run(ctx, spec.Session, "tar", "xzf", path.Join(sandboxHome, runtimeArchiveName), "-C", sandboxHome); err != nil {
			return fmt.Errorf("extracting polecat settings: %w", err)
		}
		return nil
	}); err != nil {
		return err
	}
	if err := spec.Trace.Phase("clone", func() error {
		if err := b.run(ctx, spec.Session, "sh", "-c", cloneScript, "sh", spec.Branch, spec.RepoURL, workDir, strconv.Itoa(sandboxCloneDepth)); err != nil {
			return fmt.Errorf("cloning %s: %w", spec.Branch, err)
		}
		return nil
	}); err != nil {
		return err
	}
	return spec.Trace.Phase("workspace and record", func() error {
		if files.workspace != "" {
			if err := b.cli.Upload(ctx, spec.Session, files.workspace, sandboxHome+"/"); err != nil {
				return fmt.Errorf("uploading runtime files: %w", err)
			}
			if err := b.run(ctx, spec.Session, "tar", "xzf", path.Join(sandboxHome, workspaceArchiveName), "-C", workDir); err != nil {
				return fmt.Errorf("extracting runtime files: %w", err)
			}
		}
		if err := b.run(ctx, spec.Session, "sh", "-c", redirectScript, "sh", workDir, redirect); err != nil {
			return fmt.Errorf("writing beads redirect: %w", err)
		}
		return WriteSandboxRecord(b.townRoot, SandboxRecord{
			Session:   spec.Session,
			Rig:       spec.Rig,
			Polecat:   spec.Polecat,
			Gateway:   b.profile.Gateway,
			Profile:   b.profile.Name,
			Image:     b.profile.Image,
			CreatedAt: time.Now().UTC(),
			Branch:    spec.Branch,
			WorkDir:   workDir,
		})
	})
}

// awaitReady polls the sandbox until its Ready condition is True. The pause
// between polls goes through b.wait, so a test records it instead of sleeping.
func (b *openshellBackend) awaitReady(ctx context.Context, name string) error {
	for polls := 1; ; polls++ {
		st, err := b.cli.Get(ctx, name)
		if err != nil {
			return fmt.Errorf("waiting for sandbox %s: %w", name, err)
		}
		if st.Ready {
			return nil
		}
		if polls == readyPolls {
			if !st.ReadyKnown {
				return fmt.Errorf("sandbox %s reported no Ready condition after %d polls (phase %q)", name, readyPolls, st.Phase)
			}
			return fmt.Errorf("sandbox %s not Ready after %d polls (phase %q)", name, readyPolls, st.Phase)
		}
		if err := b.wait(ctx, readyInterval); err != nil {
			return err
		}
	}
}

// run executes argv inside the sandbox and discards its output.
func (b *openshellBackend) run(ctx context.Context, name string, argv ...string) error {
	_, _, err := b.cli.Exec(ctx, name, nil, argv, nil)
	return err
}

// checkSandboxGT refuses a sandbox whose own gt is not the installed gt's
// commit. Prepare's label check reads the host docker's image for the
// profile's reference; the gateway can run a different image for that
// reference, so the gt inside the sandbox is what this check trusts
// (gt-zdilt). A refusal here rolls the sandbox back like any failed populate.
func (b *openshellBackend) checkSandboxGT(ctx context.Context, name string) error {
	out, _, err := b.cli.Exec(ctx, name, nil, sandboxVersionArgv, nil)
	if err != nil {
		return fmt.Errorf("reading the gt version in sandbox %s: %w", name, err)
	}
	return checkImageGT(b.profile.Image, sandboxVersionSource, parseGTVersionCommit(string(out)), b.installedGT(), b.imageDiff)
}

// rollback deletes the sandbox after a failed populate and removes its
// record, which populate may have written before it failed. The cleanup runs
// without the caller's context, so a canceled spawn still cleans up.
func (b *openshellBackend) rollback(ctx context.Context, name string, cause error, trace *SandboxTrace) error {
	ctx = context.WithoutCancel(ctx)
	err := trace.Phase("teardown", func() error {
		return errors.Join(b.cli.Delete(ctx, name), RemoveSandboxRecord(b.townRoot, name))
	})
	if err != nil {
		return fmt.Errorf("%w (rolling back sandbox %s: %v)", cause, name, err)
	}
	return cause
}

// createSpec is the `sandbox create` for spec: the profile's image, policy,
// providers and sizing, the identity labels, the env contract, and the
// main process that keeps the tmux server alive.
func (b *openshellBackend) createSpec(spec SpawnSpec) (sandbox.CreateSpec, error) {
	root, err := b.policyRoot(spec.TownRoot)
	if err != nil {
		return sandbox.CreateSpec{}, fmt.Errorf("sandbox %s: locating the policy %q: %w", spec.Session, b.profile.Policy, err)
	}
	policy, err := confinedPath(root, b.profile.Policy)
	if err != nil {
		return sandbox.CreateSpec{}, err
	}
	if _, err := os.Stat(policy); err != nil {
		return sandbox.CreateSpec{}, fmt.Errorf("sandbox %s: the policy is missing from the gt source checkout %s: %w", spec.Session, root, err)
	}
	cs := sandbox.CreateSpec{
		Name:       spec.Session,
		Image:      b.profile.Image,
		PolicyPath: policy,
		Providers:  b.profile.Providers,
		CPU:        b.profile.CPU,
		Memory:     b.profile.Memory,
		Labels:     map[string]string{"rig": spec.Rig, "polecat": spec.Polecat},
		Env:        b.contract(spec),
		Command:    []string{"sh", "-c", sandboxMainScript, "sh", spec.Session},
		NoKeep:     true,
	}
	if b.usesDockerVolumes() {
		cfg, err := cacheDriverConfig(b.profile.CacheVolume, spec.Rig)
		if err != nil {
			return cs, err
		}
		cs.DriverConfigJSON = cfg
	}
	if b.profile.Driver == DriverKubernetes {
		cfg, err := kubernetesDriverConfig(b.profile)
		if err != nil {
			return cs, err
		}
		cs.DriverConfigJSON = cfg
	}
	return cs, nil
}

// usesDockerVolumes reports whether this seat mounts a docker cache volume:
// the profile names one and its gateway runs the docker driver. An unknown
// driver gets no volume work: Validate refuses it, and a docker call against
// a gateway we cannot name is the wrong default.
func (b *openshellBackend) usesDockerVolumes() bool {
	if b.profile.CacheVolume == "" {
		return false
	}
	switch b.profile.Driver {
	case "", DriverDocker:
		return true
	default:
		return false
	}
}

// contract is the environment the sandbox carries for the gt and bd it runs:
// the in-sandbox town root and the identity and Dolt endpoint of this polecat.
func (b *openshellBackend) contract(spec SpawnSpec) map[string]string {
	identity := spec.Rig + "/polecats/" + spec.Polecat
	return map[string]string{
		"GT_TOWN_ROOT":       sandboxTownRoot,
		"GT_ROOT":            sandboxTownRoot,
		"GT_RIG":             spec.Rig,
		"GT_POLECAT":         spec.Polecat,
		"GT_ROLE":            identity,
		"BD_ACTOR":           identity,
		"GT_SESSION_BACKEND": BackendOpenShell,
		// The sandbox's tmux server is on SandboxTmuxSocket. Without this a gt run
		// in the sandbox derives its socket from the town path (/sandbox/gt), asks
		// a server that is not there, and its tmux reads (the Stop hook's pane
		// scan, among others) fail.
		"GT_TMUX_SOCKET":                           SandboxTmuxSocket,
		"BEADS_DOLT_SERVER_MODE":                   "1",
		"BEADS_DOLT_SERVER_HOST":                   b.controlPlaneHost(),
		"BEADS_DOLT_SERVER_PORT":                   sandboxDoltPort,
		"BD_DISABLE_METRICS":                       "1",
		"DO_NOT_TRACK":                             "1",
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
	}
}

// agentEnv is the environment override the agent's startup command gets
// inside the sandbox: the host's spawn env with the sandbox contract laid
// over it and the host-only keys dropped.
func (b *openshellBackend) agentEnv(spec SpawnSpec, workDir string) map[string]string {
	env := maps.Clone(spec.Env)
	if env == nil {
		env = map[string]string{}
	}
	for _, k := range sandboxDroppedEnv {
		env[k] = ""
	}
	maps.Copy(env, b.contract(spec))
	env["GT_POLECAT_PATH"] = workDir
	return env
}

// ForgejoHost is the host a sandbox reaches Forgejo at; the session start
// derives the sandbox clone URL from it. A profile that names no forgejo_host
// falls back to the control-plane host, so existing configs clone from where
// they always did.
func (b *openshellBackend) ForgejoHost() string {
	if b.profile.ForgejoHost != "" {
		return b.profile.ForgejoHost
	}
	return b.controlPlaneHost()
}

func (b *openshellBackend) controlPlaneHost() string {
	if b.profile.ControlPlaneHost != "" {
		return b.profile.ControlPlaneHost
	}
	return defaultControlPlaneHost
}

// stageDir makes a scratch directory for the archives this Prepare uploads.
// It lives under the town's .runtime so nothing leaves the town tree. The
// skeleton carries config, so the directory is private whatever the stdlib
// default is: it is chmodded to 0700 explicitly.
func (b *openshellBackend) stageDir() (string, error) {
	dir := filepath.Join(b.townRoot, ".runtime")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(dir, "openshell-prepare-")
	if err != nil {
		return "", err
	}
	if err := os.Chmod(stage, 0o700); err != nil {
		os.RemoveAll(stage)
		return "", err
	}
	return stage, nil
}

// confinedPath joins rel onto dir. rel must be a relative path that stays
// inside dir: an absolute path or one that climbs out with .. is refused.
func confinedPath(dir, rel string) (string, error) {
	clean := filepath.Clean(rel)
	if rel == "" || filepath.IsAbs(rel) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("policy path %q must be relative to the gt source checkout and stay inside it", rel)
	}
	return filepath.Join(dir, clean), nil
}

// sandboxRel is the host worktree's path under the town root. The sandbox
// keeps the town's layout under /sandbox/gt, so the same relative path is the
// in-sandbox one.
func sandboxRel(spec SpawnSpec) (string, error) {
	rel, err := filepath.Rel(spec.TownRoot, spec.HostWorkDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("sandbox %s: worktree %s is outside town %s", spec.Session, spec.HostWorkDir, spec.TownRoot)
	}
	rel = filepath.ToSlash(rel)
	if !strings.Contains(rel, "/") {
		return "", fmt.Errorf("sandbox %s: worktree %s is not inside a rig", spec.Session, spec.HostWorkDir)
	}
	return rel, nil
}

// redirectTarget is the rig's shared beads, relative to the worktree. For
// gastown/polecats/<name>/gastown it is ../../../mayor/rig/.beads, the same
// string the host worktree's .beads/redirect carries.
func redirectTarget(rel string) string {
	return strings.Repeat("../", strings.Count(rel, "/")) + "mayor/rig/.beads"
}

// cacheVolumeName is the rig's cache volume. {rig} in the profile's name
// expands to the rig.
func cacheVolumeName(volume, rig string) string {
	return strings.ReplaceAll(volume, "{rig}", rig)
}

// cacheVolumePattern is the names docker accepts for a volume. The first
// character is never "-", so a name cannot read as a docker flag.
var cacheVolumePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

// ensureCacheVolume creates volume when docker does not have it, labeled the
// way the gateway attaches it. An existing volume is left alone. Only docker's
// "no such volume" answer to the inspect means the volume is absent; any other
// inspect failure, such as a daemon that is down, is ErrCacheVolume with no
// create attempted. A name docker would not accept, or a nil run, is a plain
// error: retrying the work cannot fix either. A volume that cannot be made is
// ErrCacheVolume.
func ensureCacheVolume(ctx context.Context, run dockerRun, volume string) error {
	if run == nil {
		return errors.New("sandbox cache volume: no docker runner configured")
	}
	if !cacheVolumePattern.MatchString(volume) {
		return fmt.Errorf("sandbox cache volume name %q is not a valid docker volume name", volume)
	}
	_, stderr, err := run(ctx, "volume", "inspect", volume)
	if err == nil {
		return nil
	}
	if !strings.Contains(strings.ToLower(string(stderr)), "no such volume") {
		return fmt.Errorf("%w: docker volume inspect %s: %v: %s", ErrCacheVolume, volume, err, strings.TrimSpace(string(stderr)))
	}
	_, stderr, err = run(ctx, "volume", "create",
		"--label", "openshell.ai/sandbox-attachable=true",
		"--label", "openshell.ai/sandbox-attachable-workspace=default",
		volume)
	if err != nil {
		return fmt.Errorf("%w: docker volume create %s: %v: %s", ErrCacheVolume, volume, err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// kubernetesDriverConfigJSON is the create call's driver config document: the
// kubernetes driver's config under its driver name. These types are the shape's
// one definition, so a field the gateway reads is renamed here or not at all;
// the gateway's names match crates/openshell-driver-kubernetes/src/driver.rs.
type kubernetesDriverConfigJSON struct {
	Kubernetes kubernetesPodDriverConfig `json:"kubernetes"`
}

// kubernetesPodDriverConfig is placement under pod and the agent container's
// sizing under containers.agent.resources.
type kubernetesPodDriverConfig struct {
	// Containers precedes Pod and Volumes follows them so the marshaled keys stay
	// alphabetical, the order the golden tests pin.
	Containers *kubernetesContainers `json:"containers,omitempty"`
	Pod        *kubernetesPod        `json:"pod,omitempty"`
	Volumes    []kubernetesVolume    `json:"volumes,omitempty"`
}

// kubernetesVolume is one volume of the sandbox pod: the shared claim the agent
// mounts as its cache (gt-zl804).
type kubernetesVolume struct {
	Name                  string                `json:"name"`
	PersistentVolumeClaim kubernetesClaimSource `json:"persistent_volume_claim"`
}

// kubernetesClaimSource names the PersistentVolumeClaim a volume uses.
type kubernetesClaimSource struct {
	ClaimName string `json:"claim_name"`
	ReadOnly  bool   `json:"read_only"`
}

// kubernetesCacheVolumeName is the fixed name of the cache volume in the pod.
const kubernetesCacheVolumeName = "gt-cache"

// kubernetesVolumeMount mounts one of the pod's volumes into a container.
type kubernetesVolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mount_path"`
	ReadOnly  bool   `json:"read_only"`
}

// kubernetesContainers sizes the agent container by name; agent is the only
// container gt configures.
type kubernetesContainers struct {
	Agent kubernetesContainer `json:"agent"`
}

type kubernetesContainer struct {
	Resources    *kubernetesResources    `json:"resources,omitempty"`
	VolumeMounts []kubernetesVolumeMount `json:"volume_mounts,omitempty"`
}

// kubernetesResources carries the container's limits and requests, limits first
// for the same alphabetical order.
type kubernetesResources struct {
	Limits   *config.SandboxResources `json:"limits,omitempty"`
	Requests *config.SandboxResources `json:"requests,omitempty"`
}

// kubernetesPod places the sandbox pod.
type kubernetesPod struct {
	NodeSelector      map[string]string          `json:"node_selector,omitempty"`
	PriorityClassName string                     `json:"priority_class_name,omitempty"`
	RuntimeClassName  string                     `json:"runtime_class_name,omitempty"`
	Tolerations       []config.SandboxToleration `json:"tolerations,omitempty"`
}

// kubernetesDriverConfig is the driver config that places the sandbox pod and
// sizes its agent container. A profile that sets none of the fields returns "":
// the gateway's pod defaults stand, and an empty object is not sent.
func kubernetesDriverConfig(p Profile) (string, error) {
	var cfg kubernetesDriverConfigJSON
	if len(p.NodeSelector) > 0 || len(p.Tolerations) > 0 || p.PriorityClass != "" || p.RuntimeClass != "" {
		cfg.Kubernetes.Pod = &kubernetesPod{
			NodeSelector:      p.NodeSelector,
			PriorityClassName: p.PriorityClass,
			RuntimeClassName:  p.RuntimeClass,
			Tolerations:       p.Tolerations,
		}
	}
	var resources kubernetesResources
	if p.Requests != nil && *p.Requests != (config.SandboxResources{}) {
		resources.Requests = p.Requests
	}
	if p.Limits != nil && *p.Limits != (config.SandboxResources{}) {
		resources.Limits = p.Limits
	}
	agent := kubernetesContainer{}
	if resources != (kubernetesResources{}) {
		agent.Resources = &resources
	}
	if p.KubernetesCacheClaim != "" {
		cfg.Kubernetes.Volumes = []kubernetesVolume{{
			Name:                  kubernetesCacheVolumeName,
			PersistentVolumeClaim: kubernetesClaimSource{ClaimName: p.KubernetesCacheClaim},
		}}
		agent.VolumeMounts = []kubernetesVolumeMount{{Name: kubernetesCacheVolumeName, MountPath: sandboxCacheDir}}
	}
	if agent.Resources != nil || agent.VolumeMounts != nil {
		cfg.Kubernetes.Containers = &kubernetesContainers{Agent: agent}
	}
	if cfg.Kubernetes.Pod == nil && cfg.Kubernetes.Containers == nil && cfg.Kubernetes.Volumes == nil {
		return "", nil
	}
	cfgJSON, err := json.Marshal(cfg)
	return string(cfgJSON), err
}

// cacheDriverConfig is the driver config that mounts the cache volume at
// /sandbox/.cache. {rig} in the volume name expands to the rig.
func cacheDriverConfig(volume, rig string) (string, error) {
	mount := map[string]any{
		"type":      "volume",
		"source":    cacheVolumeName(volume, rig),
		"target":    sandboxCacheDir,
		"read_only": false,
	}
	cfg, err := json.Marshal(map[string]any{"docker": map[string]any{"mounts": []any{mount}}})
	return string(cfg), err
}

// buildWorkspaceArchive packs the runtime files gt writes into the host
// worktree that the agent in the sandbox reads: CLAUDE.md and the slash
// commands under .claude/commands. Nothing else in the worktree is sent. It
// returns nil when there is nothing to send.
func buildWorkspaceArchive(worktree string) ([]byte, error) {
	rels := []string{"CLAUDE.md"}
	cmds, err := os.ReadDir(filepath.Join(worktree, ".claude", "commands"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	for _, e := range cmds {
		if e.Type().IsRegular() {
			rels = append(rels, path.Join(".claude", "commands", e.Name()))
		}
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	n := 0
	for _, rel := range rels {
		data, err := os.ReadFile(filepath.Join(worktree, filepath.FromSlash(rel)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := tw.WriteHeader(&tar.Header{Name: rel, Mode: 0o644, Size: int64(len(data)), Typeflag: tar.TypeReg}); err != nil {
			return nil, err
		}
		if _, err := tw.Write(data); err != nil {
			return nil, err
		}
		n++
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if err := gz.Close(); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, nil
	}
	return buf.Bytes(), nil
}

// hostGitEmail is the host's global git identity email, "" when unset or
// unreadable. git runs with only HOME and PATH, so neither a repository's nor an
// ambient GIT_* setting decides the answer.
func hostGitEmail() string {
	return gitEmailFrom(func(name string, args ...string) ([]byte, error) {
		cmd := exec.Command(name, args...) //nolint:gosec // G204: fixed command
		cmd.Env = []string{"HOME=" + os.Getenv("HOME"), "PATH=" + os.Getenv("PATH")}
		return cmd.Output()
	})
}

// gitEmailFrom asks run for `git config --global user.email` and returns the
// trimmed answer, "" when git fails.
func gitEmailFrom(run func(name string, args ...string) ([]byte, error)) string {
	out, err := run("git", "config", "--global", "user.email")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// promptRenderer is the renderer the sandbox's system prompt is rendered with: the backend's own, or
// the installed one.
func (b *openshellBackend) promptRenderer() config.SystemPromptRender {
	if b.renderPrompt != nil {
		return b.renderPrompt
	}
	return config.SystemPromptRenderer
}
