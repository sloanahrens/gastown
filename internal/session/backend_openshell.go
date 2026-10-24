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
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

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
// compares the branch with origin/main. Arguments are positional, so no value
// is spliced into the script text.
const cloneScript = `H="http.extraHeader=Authorization: token $FORGEJO_TOKEN"; ` +
	`git -c "$H" clone --branch "$1" --single-branch "$2" "$3" && cd "$3" && ` +
	`git remote set-branches --add origin main && git -c "$H" fetch origin main`

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
	// docker runs the docker CLI for the profile's cache volume. Tests set it.
	docker dockerRun
	// policyRoot is the town's gt source checkout, the repo a sandbox profile's
	// policy path is relative to. A polecat's worktree is the wrong base: only
	// the gt repo carries deploy/polecat/policy.yaml, and a sandbox seat serves
	// the town's other rigs too.
	policyRoot func(townRoot string) (string, error)
	// gitEmail reads the host's global git user.email, the address the sandbox's
	// commits carry (host polecats commit with it too); "" when it is unset.
	gitEmail    func() string
	installedGT func() string
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
		docker:      runDocker,
		gitEmail:    hostGitEmail,
		installedGT: version.BuildCommit,
		policyRoot:  version.GetRepoRootForTown,
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

// checkImageGT refuses a sandbox image whose gt is not the installed gt's
// commit, so a drifted image fails rather than becoming a settings warning
// inside a sandbox (gt-pmxo9). source says where imageCommit was read, so a
// missing commit names the place to fix.
func checkImageGT(image, source, imageCommit, installed string) error {
	switch {
	case installed == "":
		return fmt.Errorf("sandbox image %s: the installed gt records no build commit, so its gt cannot be checked; rebuild gt with make build", image)
	case imageCommit == "":
		return fmt.Errorf("sandbox image %s %s, so its gt cannot be checked; rebuild it with make polecat-image", image, source)
	case !version.SameCommit(imageCommit, installed):
		return &imageDriftError{msg: fmt.Sprintf("sandbox image %s carries gt commit %s, but the installed gt is commit %s; rebuild the image with make polecat-image", image, imageCommit, installed)}
	}
	return nil
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
	return git.NewGitWithDir(bareRepo, "").Push("origin", branch, false)
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
	// The image's gt must be the installed gt's commit. Checked before the push
	// and the create, so a drifted image leaves nothing behind (gt-pmxo9). This
	// reads the host docker's image for the reference; the sandbox's own gt is
	// checked again once it is Ready, because the gateway may run a different
	// image for that reference (gt-zdilt).
	imageCommit, err := b.imageGT(ctx, b.profile.Image)
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
	}
	if err := checkImageGT(b.profile.Image, gtLabelSource, imageCommit, b.installedGT()); err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
	}

	// Before the push and the create, as the image check is: a missing cache
	// volume that cannot be made leaves nothing behind (gt-td6pl).
	if b.profile.CacheVolume != "" {
		if err := ensureCacheVolume(ctx, b.docker, cacheVolumeName(b.profile.CacheVolume, spec.Rig)); err != nil {
			return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
		}
	}

	// Also before anything is pushed or created: the agent command names these
	// files, so a polecat whose settings are missing or cannot be rendered for
	// the sandbox must stop here.
	runtime, err := buildRuntimeArchive(spec, b.gitEmail())
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: %w", spec.Session, err)
	}

	// The clone inside the sandbox fetches the branch, so it must exist on
	// Forgejo before the sandbox is created.
	if err := b.push(filepath.Join(spec.TownRoot, spec.Rig, ".repo.git"), spec.Branch); err != nil {
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

	if err := b.cli.Create(ctx, create); err != nil {
		return nil, fmt.Errorf("creating sandbox %s: %w", spec.Session, err)
	}

	workDir := path.Join(sandboxTownRoot, rel)
	if err := b.populate(ctx, spec, workDir, redirectTarget(rel), staged); err != nil {
		return nil, b.rollback(ctx, spec.Session, err)
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
	if err := b.awaitReady(ctx, spec.Session); err != nil {
		return err
	}
	// The sandbox's own gt is the one that will run; Prepare's label check only
	// saw the host docker's image for the same reference (gt-zdilt).
	if err := b.checkSandboxGT(ctx, spec.Session); err != nil {
		return err
	}
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
	if err := b.run(ctx, spec.Session, "sh", "-c", cloneScript, "sh", spec.Branch, spec.RepoURL, workDir); err != nil {
		return fmt.Errorf("cloning %s: %w", spec.Branch, err)
	}
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
	return checkImageGT(b.profile.Image, sandboxVersionSource, parseGTVersionCommit(string(out)), b.installedGT())
}

// rollback deletes the sandbox after a failed populate and removes its
// record, which populate may have written before it failed. The cleanup runs
// without the caller's context, so a canceled spawn still cleans up.
func (b *openshellBackend) rollback(ctx context.Context, name string, cause error) error {
	ctx = context.WithoutCancel(ctx)
	err := errors.Join(b.cli.Delete(ctx, name), RemoveSandboxRecord(b.townRoot, name))
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
	if b.profile.CacheVolume != "" {
		cfg, err := cacheDriverConfig(b.profile.CacheVolume, spec.Rig)
		if err != nil {
			return cs, err
		}
		cs.DriverConfigJSON = cfg
	}
	return cs, nil
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

// ControlPlaneHost is the host a sandbox reaches Dolt and Forgejo at; the
// session start derives the sandbox clone URL from it.
func (b *openshellBackend) ControlPlaneHost() string { return b.controlPlaneHost() }

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

// ensureCacheVolume creates volume when docker does not have it, labeled the
// way the gateway attaches it: the same `docker volume create` that
// scripts/polecat-caches.sh runs, so a new rig needs no onboarding step. An
// existing volume is left alone. A volume that cannot be made is ErrCacheVolume.
func ensureCacheVolume(ctx context.Context, run dockerRun, volume string) error {
	if _, _, err := run(ctx, "volume", "inspect", volume); err == nil {
		return nil
	}
	_, stderr, err := run(ctx, "volume", "create",
		"--label", "openshell.ai/sandbox-attachable=true",
		"--label", "openshell.ai/sandbox-attachable-workspace=default",
		volume)
	if err != nil {
		return fmt.Errorf("%w: docker volume create %s: %v: %s", ErrCacheVolume, volume, err, strings.TrimSpace(string(stderr)))
	}
	return nil
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
