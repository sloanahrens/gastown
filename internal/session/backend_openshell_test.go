package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sandbox"
)

const (
	openshellTestRig      = "gastown"
	openshellTestPolecat  = "sandbox-ace"
	openshellTestSession  = "gt-gastown-sandbox-ace"
	openshellTestBranch   = "polecat/sandbox-ace/gt-1+abc"
	openshellTestRepoURL  = "http://host.docker.internal:3000/sloan/gastown.git"
	openshellTestGitEmail = "host-dev@example.test"
	openshellTestWorkDir  = "/sandbox/gt/gastown/polecats/sandbox-ace/gastown"

	openshellReadyReply        = `{"name":"gt-gastown-sandbox-ace","phase":"Ready","conditions":[{"type":"Ready","status":"True"}]}`
	openshellProvisioningReply = `{"name":"gt-gastown-sandbox-ace","phase":"Provisioning","conditions":[{"type":"Ready","status":"False"}]}`
)

var openshellTestProfile = Profile{
	Name:      "polecat-default",
	Image:     "gastown-polecat:dev",
	Policy:    "deploy/polecat/policy.yaml",
	Providers: []string{"deepseek", "forgejo-polecat"},
	CPU:       "4",
	Memory:    "8Gi",
}

// openshellTown is a town whose polecat worktree carries the runtime files
// gt writes there, plus one file that must not be sent. The town's gt source
// checkout carries the profile's policy, as the gastown repo does.
func openshellTown(t *testing.T) (town, work string) {
	t.Helper()
	town = t.TempDir()
	work = filepath.Join(town, openshellTestRig, "polecats", openshellTestPolecat, openshellTestRig)
	writeOpenshellFile(t, filepath.Join(work, "CLAUDE.md"), "role context\n")
	writeOpenshellFile(t, filepath.Join(work, ".claude", "commands", "done.md"), "done\n")
	writeOpenshellFile(t, filepath.Join(work, ".claude", "settings.local.json"), "{}\n")
	claudeDir := filepath.Join(town, openshellTestRig, "polecats", ".claude")
	writeOpenshellFile(t, filepath.Join(claudeDir, "settings.json"), openshellHostSettings)
	writeOpenshellFile(t, filepath.Join(claudeDir, "system-prompt-"+openshellTestPolecat+".md"), "town at "+town+"/mayor\n")
	source := openshellTestPolicyRoot(town)
	writeOpenshellFile(t, filepath.Join(source, "cmd", "gt", "main.go"), "package main\n")
	writeOpenshellFile(t, filepath.Join(source, "deploy", "polecat", "policy.yaml"), "version: 1\n")
	return town, work
}

func writeOpenshellFile(t *testing.T, p, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func openshellSpawn(town, work string) SpawnSpec {
	return SpawnSpec{
		TownRoot:    town,
		Rig:         openshellTestRig,
		Polecat:     openshellTestPolecat,
		Session:     openshellTestSession,
		Branch:      openshellTestBranch,
		RepoURL:     openshellTestRepoURL,
		HostWorkDir: work,
		Env: map[string]string{
			"GT_TOWN_ROOT":         town,
			"GT_POLECAT_PATH":      work,
			"GT_BRANCH":            openshellTestBranch,
			"CLAUDE_CONFIG_DIR":    "/host/only/config",
			"ANTHROPIC_AUTH_TOKEN": "HOST-ONLY-VALUE",
		},
	}
}

// pushCall is one push from the rig's bare repo, with how many openshell
// calls had been made when it ran.
type pushCall struct {
	bare, branch string
	sandboxCalls int
}

// openshellTestBackend is a backend whose openshell and git are recorded,
// not run. The skeleton is a placeholder: the renderer has its own tests.
func openshellTestBackend(town string, p Profile, fake *sandbox.Fake) (*openshellBackend, *[]pushCall) {
	pushes := &[]pushCall{}
	b := &openshellBackend{
		cli:      &sandbox.CLI{Gateway: p.Gateway, Run: fake.Runner()},
		townRoot: town,
		profile:  p,
		wait:     func(context.Context, time.Duration) error { return nil },
		render:   func(sandbox.SkeletonSpec) ([]byte, error) { return []byte("skeleton"), nil },
		push: func(bare, branch string) error {
			*pushes = append(*pushes, pushCall{bare: bare, branch: branch, sandboxCalls: len(fake.Calls())})
			return nil
		},
		imageGT: func(context.Context, string) (string, error) { return openshellTestGTCommit, nil },
		// No docker in these tests. A profile with a cache volume sets its own runner.
		docker: func(context.Context, ...string) ([]byte, []byte, error) {
			return nil, []byte("docker is not configured for this test"), errors.New("no docker")
		},
		gitEmail:    func() string { return openshellTestGitEmail },
		installedGT: func() string { return openshellTestGTCommit },
		policyRoot:  func(townRoot string) (string, error) { return openshellTestPolicyRoot(townRoot), nil },
	}
	return b, pushes
}

// openshellPrepareFake is a fake for a Prepare that reaches the sandbox: it
// answers `gt version` with the matching commit, and replies with the rest of
// replies. A caller that needs a different exec reply sets it in replies.
func openshellPrepareFake(replies map[string]string) *sandbox.Fake {
	if replies == nil {
		replies = map[string]string{}
	}
	if _, ok := replies["exec"]; !ok {
		replies["exec"] = openshellTestVersionReply
	}
	return &sandbox.Fake{Reply: replies}
}

// sandboxExec is the argv of `openshell sandbox exec` for the test session.
func sandboxExec(args ...string) []string {
	return append([]string{"openshell", "sandbox", "exec", "-n", openshellTestSession, "--no-tty", "--no-login-shell", "--"}, args...)
}

// stageNames replaces the random staging directory in uploaded paths with a
// fixed name, so the argv can be compared exactly.
func stageNames(calls [][]string) [][]string {
	out := make([][]string, 0, len(calls))
	for _, c := range calls {
		n := make([]string, len(c))
		for i, a := range c {
			switch {
			case strings.HasSuffix(a, "/town-skel.tgz") && !strings.HasPrefix(a, "/sandbox"):
				a = "STAGE/town-skel.tgz"
			case strings.HasSuffix(a, "/polecat-runtime.tgz") && !strings.HasPrefix(a, "/sandbox"):
				a = "STAGE/polecat-runtime.tgz"
			case strings.HasSuffix(a, "/workspace.tgz") && !strings.HasPrefix(a, "/sandbox"):
				a = "STAGE/workspace.tgz"
			}
			n[i] = a
		}
		out = append(out, n)
	}
	return out
}

func TestOpenShellPrepareBuildsTheSandboxInOrder(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	wantPush := []pushCall{{bare: filepath.Join(town, openshellTestRig, ".repo.git"), branch: openshellTestBranch, sandboxCalls: 0}}
	if !reflect.DeepEqual(*pushes, wantPush) {
		t.Fatalf("pushes = %+v, want %+v (the branch must reach Forgejo before the sandbox exists)", *pushes, wantPush)
	}

	create := []string{
		"openshell", "sandbox", "create",
		"--name", openshellTestSession,
		"--from", "gastown-polecat:dev",
		"--policy", filepath.Join(openshellTestPolicyRoot(town), "deploy/polecat/policy.yaml"),
		"--provider", "deepseek", "--provider", "forgejo-polecat",
		"--cpu", "4", "--memory", "8Gi",
		"--label", "polecat=sandbox-ace", "--label", "rig=gastown",
		"--env-from", "BD_ACTOR=GT_SANDBOX_ENV_BD_ACTOR",
		"--env-from", "BD_DISABLE_METRICS=GT_SANDBOX_ENV_BD_DISABLE_METRICS",
		"--env-from", "BEADS_DOLT_SERVER_HOST=GT_SANDBOX_ENV_BEADS_DOLT_SERVER_HOST",
		"--env-from", "BEADS_DOLT_SERVER_MODE=GT_SANDBOX_ENV_BEADS_DOLT_SERVER_MODE",
		"--env-from", "BEADS_DOLT_SERVER_PORT=GT_SANDBOX_ENV_BEADS_DOLT_SERVER_PORT",
		"--env-from", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=GT_SANDBOX_ENV_CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"--env-from", "DISABLE_AUTOUPDATER=GT_SANDBOX_ENV_DISABLE_AUTOUPDATER",
		"--env-from", "DO_NOT_TRACK=GT_SANDBOX_ENV_DO_NOT_TRACK",
		"--env-from", "GT_POLECAT=GT_SANDBOX_ENV_GT_POLECAT",
		"--env-from", "GT_RIG=GT_SANDBOX_ENV_GT_RIG",
		"--env-from", "GT_ROLE=GT_SANDBOX_ENV_GT_ROLE",
		"--env-from", "GT_ROOT=GT_SANDBOX_ENV_GT_ROOT",
		"--env-from", "GT_SESSION_BACKEND=GT_SANDBOX_ENV_GT_SESSION_BACKEND",
		"--env-from", "GT_TMUX_SOCKET=GT_SANDBOX_ENV_GT_TMUX_SOCKET",
		"--env-from", "GT_TOWN_ROOT=GT_SANDBOX_ENV_GT_TOWN_ROOT",
		"--detach", "--no-keep",
		"--", "sh", "-c", sandboxMainScript, "sh", openshellTestSession,
	}
	want := [][]string{
		create,
		{"openshell", "sandbox", "get", openshellTestSession, "--output", "json"},
		sandboxExec("gt", "version"),
		{"openshell", "sandbox", "upload", "--no-git-ignore", openshellTestSession, "STAGE/town-skel.tgz", "/sandbox/"},
		sandboxExec("tar", "xzf", "/sandbox/town-skel.tgz", "-C", "/sandbox"),
		{"openshell", "sandbox", "upload", "--no-git-ignore", openshellTestSession, "STAGE/polecat-runtime.tgz", "/sandbox/"},
		sandboxExec("tar", "xzf", "/sandbox/polecat-runtime.tgz", "-C", "/sandbox"),
		sandboxExec("sh", "-c", cloneScript, "sh", openshellTestBranch, openshellTestRepoURL, openshellTestWorkDir, strconv.Itoa(sandboxCloneDepth)),
		{"openshell", "sandbox", "upload", "--no-git-ignore", openshellTestSession, "STAGE/workspace.tgz", "/sandbox/"},
		sandboxExec("tar", "xzf", "/sandbox/workspace.tgz", "-C", openshellTestWorkDir),
		sandboxExec("sh", "-c", redirectScript, "sh", openshellTestWorkDir, "../../../mayor/rig/.beads"),
	}
	if got := stageNames(fake.Calls()); !reflect.DeepEqual(got, want) {
		t.Fatalf("openshell calls:\n got %q\nwant %q", got, want)
	}
	// The socket is asserted as the literal "gt": the value openshell hands the
	// sandbox is the contract, so a change to SandboxTmuxSocket has to fail here.
	if envs := fake.Envs(); len(envs) == 0 ||
		!slices.Contains(envs[0], "GT_SANDBOX_ENV_GT_ROLE="+openshellTestRig+"/polecats/"+openshellTestPolecat) ||
		!slices.Contains(envs[0], "GT_SANDBOX_ENV_GT_TMUX_SOCKET=gt") {
		t.Fatalf("create env = %q; the contract values must reach openshell's process environment, not its argv", envs)
	}

	rec, ok, err := ReadSandboxRecord(town, openshellTestSession)
	if err != nil || !ok {
		t.Fatalf("record: ok=%v err=%v; the record is what routes the session here", ok, err)
	}
	if rec.Profile != "polecat-default" || rec.Image != "gastown-polecat:dev" || rec.Rig != openshellTestRig || rec.Polecat != openshellTestPolecat {
		t.Fatalf("record = %+v", rec)
	}
}

func TestOpenShellPrepareHandsBackTheSandboxEnv(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)

	prep, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if prep.WorkDir != openshellTestWorkDir {
		t.Fatalf("WorkDir = %q, want %q", prep.WorkDir, openshellTestWorkDir)
	}
	if !prep.OmitSecretPrefix {
		t.Fatal("OmitSecretPrefix = false; the provider, not settings/daemon.env, supplies the credentials")
	}
	// GT_TMUX_SOCKET is the literal "gt", not SandboxTmuxSocket: the value is
	// what the sandbox's tmux server is named, so the test must not track the
	// constant it is checking.
	for k, want := range map[string]string{
		"GT_TOWN_ROOT":           "/sandbox/gt",
		"GT_ROOT":                "/sandbox/gt",
		"GT_POLECAT_PATH":        openshellTestWorkDir,
		"GT_SESSION_BACKEND":     "openshell",
		"GT_TMUX_SOCKET":         "gt",
		"BEADS_DOLT_SERVER_HOST": "host.docker.internal",
		"GT_BRANCH":              openshellTestBranch,
		"CLAUDE_CONFIG_DIR":      "",
		"ANTHROPIC_AUTH_TOKEN":   "",
	} {
		if got := prep.Env[k]; got != want {
			t.Errorf("Env[%s] = %q, want %q", k, got, want)
		}
	}
	if b.Tmux() == nil {
		t.Fatal("Tmux() = nil after Prepare; the session's commands need the sandbox client")
	}
}

// A profile that names a separate forgejo_host moves only the clone host: the
// sandbox still reaches Dolt at the control-plane host (gt-k42fg).
func TestOpenShellForgejoHostFallsBackToTheControlPlaneHost(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		p    Profile
		want string
	}{
		{"profile names a forgejo host", Profile{ControlPlaneHost: "dolt.internal", ForgejoHost: "forgejo.internal"}, "forgejo.internal"},
		{"profile names no forgejo host", Profile{ControlPlaneHost: "dolt.internal"}, "dolt.internal"},
		{"profile names neither", Profile{}, defaultControlPlaneHost},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &openshellBackend{profile: tt.p}
			if got := b.ForgejoHost(); got != tt.want {
				t.Fatalf("ForgejoHost() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Prepare gives the skeleton and the agent's env the control-plane host even
// when the profile names another Forgejo host: only the clone URL the session
// start derives moves (gt-k42fg).
func TestOpenShellPrepareKeepsDoltAtTheControlPlaneHost(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	p := openshellTestProfile
	p.ControlPlaneHost = "dolt.internal"
	p.ForgejoHost = "forgejo.internal"
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, p, fake)
	var skeleton sandbox.SkeletonSpec
	b.render = func(s sandbox.SkeletonSpec) ([]byte, error) {
		skeleton = s
		return []byte("skeleton"), nil
	}

	prep, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if skeleton.ControlPlaneHost != "dolt.internal" {
		t.Errorf("skeleton ControlPlaneHost = %q, want the control-plane host dolt.internal", skeleton.ControlPlaneHost)
	}
	if got := prep.Env["BEADS_DOLT_SERVER_HOST"]; got != "dolt.internal" {
		t.Errorf("Env[BEADS_DOLT_SERVER_HOST] = %q, want the control-plane host dolt.internal", got)
	}
}

func TestOpenShellPrepareWaitsForReadyBetweenPolls(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(nil)
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	run := b.cli.Run
	gets := 0
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		if len(argv) > 2 && argv[2] == "get" {
			gets++
			if gets == 1 {
				return []byte(openshellProvisioningReply), nil, nil
			}
			return []byte(openshellReadyReply), nil, nil
		}
		return run(ctx, argv, stdin, env)
	}
	var waits []time.Duration
	b.wait = func(_ context.Context, d time.Duration) error {
		waits = append(waits, d)
		return nil
	}

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if gets != 2 {
		t.Fatalf("sandbox get called %d times, want 2 (Provisioning, then Ready)", gets)
	}
	if want := []time.Duration{readyInterval}; !reflect.DeepEqual(waits, want) {
		t.Fatalf("waits = %v, want %v (one pause between the two polls)", waits, want)
	}
}

// openshellTestGTCommit is the commit the test image's gt and the installed gt
// both report; openshellTestDriftedGT is an older image's commit.
const (
	openshellTestGTCommit    = "e32fa546a4992ec40088a201296a535a03ddcdde"
	openshellTestDriftedGT   = "80c667f7d1a2b3c4d5e6f708192a3b4c5d6e7f80"
	openshellTestInstalledGT = "ac578ef4" // a short commit, as make build stamps it

	// openshellTestVersionReply is what the sandbox's `gt version` prints for
	// an image built from openshellTestGTCommit.
	openshellTestVersionReply = "gt version 1.2.1 (dev: main@" + openshellTestGTCommit + ")\n"
)

func TestCheckImageGT(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, imageCommit, installed, wantErr string
	}{
		{name: "full image commit matches a short installed commit", imageCommit: openshellTestGTCommit, installed: openshellTestGTCommit[:9]},
		{name: "drifted image names both commits", imageCommit: openshellTestDriftedGT, installed: openshellTestInstalledGT, wantErr: openshellTestDriftedGT + "|" + openshellTestInstalledGT},
		{name: "image without the label is refused", imageCommit: "", installed: openshellTestGTCommit, wantErr: "records no gt commit"},
		{name: "installed gt with no commit is refused", imageCommit: openshellTestGTCommit, installed: "", wantErr: "records no build commit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := checkImageGT("gastown-polecat:dev", gtLabelSource, tt.imageCommit, tt.installed)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkImageGT = %v, want accepted", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkImageGT accepted the image, want refusal naming %q", tt.wantErr)
			}
			for _, want := range strings.Split(tt.wantErr, "|") {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want it to name %q", err, want)
				}
			}
		})
	}
}

func TestImageGTFrom(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stdout  string
		stderr  string
		runErr  error
		want    string
		wantErr string
	}{
		{name: "label is read from stdout", stdout: openshellTestGTCommit + "\n", want: openshellTestGTCommit},
		{name: "an unlabelled image reads as no commit", stdout: "\n", want: ""},
		{
			name:    "a failing inspect names the image and its stderr",
			stderr:  "Error response from daemon: No such image: gastown-polecat:dev\n",
			runErr:  errors.New("exit status 1"),
			wantErr: "gastown-polecat:dev|exit status 1|No such image",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var argv []string
			run := func(_ context.Context, args ...string) ([]byte, []byte, error) {
				argv = args
				return []byte(tt.stdout), []byte(tt.stderr), tt.runErr
			}
			got, err := imageGTFrom(context.Background(), run, "gastown-polecat:dev")
			wantArgv := []string{"image", "inspect", "--format", `{{index .Config.Labels "` + gtCommitLabel + `"}}`, "gastown-polecat:dev"}
			if !reflect.DeepEqual(argv, wantArgv) {
				t.Fatalf("argv = %q, want %q", argv, wantArgv)
			}
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("imageGTFrom = %v, want the label", err)
				}
				if got != tt.want {
					t.Fatalf("imageGTFrom = %q, want %q", got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("imageGTFrom = %q, nil, want a failure naming %q", got, tt.wantErr)
			}
			for _, want := range strings.Split(tt.wantErr, "|") {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("err = %v, want it to name %q", err, want)
				}
			}
		})
	}
}

func TestParseGTVersionCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name, out, want string
	}{
		{name: "branch and commit", out: "gt version 1.2.1 (dev: main@e32fa54)\n", want: "e32fa54"},
		{name: "commit without a branch", out: "gt version 1.2.1 (dev: e32fa54)\n", want: "e32fa54"},
		{name: "no commit named", out: "gt version 1.2.1 (dev)\n", want: ""},
		{name: "unexpected output names no commit", out: "Error: sandbox not found\n", want: ""},
		{name: "empty output names no commit", out: "", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := parseGTVersionCommit(tt.out); got != tt.want {
				t.Fatalf("parseGTVersionCommit(%q) = %q, want %q", tt.out, got, tt.want)
			}
		})
	}
}

func TestOpenShellPrepareRefusesADriftedImageBeforeAnySandbox(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{}
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)
	b.imageGT = func(context.Context, string) (string, error) { return openshellTestDriftedGT, nil }

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), openshellTestDriftedGT) || !strings.Contains(err.Error(), openshellTestGTCommit) {
		t.Fatalf("err = %v, want the refusal to name the image's commit and the installed commit", err)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("calls = %q, want none: the refusal comes before the sandbox", fake.Calls())
	}
	if len(*pushes) != 0 {
		t.Fatalf("pushes = %v, want none: the refusal comes before the push", *pushes)
	}
}

// TestOpenShellPrepareRefusesADriftedSandboxImage is gt-zdilt's case: the host
// docker's label says one commit, and the image the gateway actually ran says
// another. The sandbox's own gt decides, and the refused sandbox is deleted.
func TestOpenShellPrepareRefusesADriftedSandboxImage(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Reply: map[string]string{
		"get":  openshellReadyReply,
		"exec": "gt version 1.2.1 (dev: main@" + openshellTestDriftedGT + ")\n",
	}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), openshellTestDriftedGT) || !strings.Contains(err.Error(), openshellTestGTCommit) {
		t.Fatalf("err = %v, want the refusal to name the sandbox's commit and the installed commit", err)
	}
	calls := fake.Calls()
	for _, c := range calls {
		if len(c) > 2 && c[2] == "upload" {
			t.Fatalf("calls = %q, want no upload: the refusal comes before the sandbox is filled", calls)
		}
	}
	last := calls[len(calls)-1]
	want := []string{"openshell", "sandbox", "delete", openshellTestSession}
	if !reflect.DeepEqual(last, want) {
		t.Fatalf("last call = %q, want %q (rollback)", last, want)
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("a refused sandbox left a record behind")
	}
}

func TestOpenShellPrepareCreateFailureMakesNoFurtherCalls(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Err: map[string]error{"create": errors.New("gateway unreachable")}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "creating sandbox") {
		t.Fatalf("err = %v, want creating sandbox", err)
	}
	if len(fake.Calls()) != 1 || fake.Calls()[0][2] != "create" {
		t.Fatalf("calls = %q, want only the create", fake.Calls())
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("a failed Prepare left a sandbox record behind")
	}
}

func TestOpenShellPrepareCloneFailureDeletesTheSandbox(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	run := b.cli.Run
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		if slices.ContainsFunc(argv, func(a string) bool { return a == cloneScript }) {
			return nil, []byte("fatal: unable to access the repository"), errors.New("exit status 128")
		}
		return run(ctx, argv, stdin, env)
	}

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "cloning "+openshellTestBranch) {
		t.Fatalf("err = %v, want the clone failure", err)
	}
	last := fake.Calls()[len(fake.Calls())-1]
	want := []string{"openshell", "sandbox", "delete", openshellTestSession}
	if !reflect.DeepEqual(last, want) {
		t.Fatalf("last call = %q, want %q (rollback)", last, want)
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("a failed Prepare left a sandbox record behind")
	}
}

func TestOpenShellPrepareRefusesAWorktreeOutsideTheTown(t *testing.T) {
	t.Parallel()
	town, _ := openshellTown(t)
	fake := &sandbox.Fake{}
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)

	spec := openshellSpawn(town, t.TempDir())
	if _, err := b.Prepare(context.Background(), spec); err == nil {
		t.Fatal("Prepare accepted a worktree outside the town")
	}
	if len(*pushes) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("pushes = %d, openshell calls = %d; a bad spec must touch nothing", len(*pushes), len(fake.Calls()))
	}
}

// dockerRecorder is a dockerRun that records its argv and runs no docker. The
// volume is present unless absent is set; create fails with createErr.
type dockerRecorder struct {
	absent    bool
	createErr error
	calls     [][]string
}

func (d *dockerRecorder) run(_ context.Context, args ...string) ([]byte, []byte, error) {
	d.calls = append(d.calls, args)
	switch args[1] {
	case "inspect":
		if d.absent {
			return nil, []byte("Error: No such volume: gt-cache-gastown"), errors.New("exit status 1")
		}
	case "create":
		if d.createErr != nil {
			return nil, []byte("Cannot connect to the Docker daemon"), d.createErr
		}
	}
	return nil, nil, nil
}

var openshellTestCacheCreate = []string{"volume", "create", "--label", "openshell.ai/sandbox-attachable=true", "--label", "openshell.ai/sandbox-attachable-workspace=default", "gt-cache-gastown"}

func openshellCachedProfile() Profile {
	p := openshellTestProfile
	p.CacheVolume = "gt-cache-{rig}"
	return p
}

func TestEnsureCacheVolumeCreatesAMissingVolume(t *testing.T) {
	t.Parallel()
	d := &dockerRecorder{absent: true}
	if err := ensureCacheVolume(context.Background(), d.run, "gt-cache-gastown"); err != nil {
		t.Fatalf("ensureCacheVolume: %v", err)
	}
	want := [][]string{{"volume", "inspect", "gt-cache-gastown"}, openshellTestCacheCreate}
	if !reflect.DeepEqual(d.calls, want) {
		t.Fatalf("docker calls = %q, want %q", d.calls, want)
	}
}

func TestEnsureCacheVolumeLeavesAPresentVolumeAlone(t *testing.T) {
	t.Parallel()
	d := &dockerRecorder{}
	if err := ensureCacheVolume(context.Background(), d.run, "gt-cache-gastown"); err != nil {
		t.Fatalf("ensureCacheVolume: %v", err)
	}
	want := [][]string{{"volume", "inspect", "gt-cache-gastown"}}
	if !reflect.DeepEqual(d.calls, want) {
		t.Fatalf("docker calls = %q, want %q: a present volume is not recreated", d.calls, want)
	}
}

func TestEnsureCacheVolumeFailureMatchesErrCacheVolume(t *testing.T) {
	t.Parallel()
	d := &dockerRecorder{absent: true, createErr: errors.New("exit status 1")}
	err := ensureCacheVolume(context.Background(), d.run, "gt-cache-gastown")
	if !errors.Is(err, ErrCacheVolume) {
		t.Fatalf("err = %v, want ErrCacheVolume", err)
	}
	if !strings.Contains(err.Error(), "Cannot connect to the Docker daemon") {
		t.Errorf("err = %v, want docker's stderr in it", err)
	}
}

// A failed inspect that is not docker's "no such volume" answer, a daemon that
// is down for one, must not read as an absent volume: no create is attempted.
func TestEnsureCacheVolumeInspectErrorIsNotAbsent(t *testing.T) {
	t.Parallel()
	var calls [][]string
	run := func(_ context.Context, args ...string) ([]byte, []byte, error) {
		calls = append(calls, args)
		return nil, []byte("Cannot connect to the Docker daemon"), errors.New("exit status 1")
	}
	err := ensureCacheVolume(context.Background(), run, "gt-cache-gastown")
	if !errors.Is(err, ErrCacheVolume) {
		t.Fatalf("err = %v, want ErrCacheVolume", err)
	}
	if want := [][]string{{"volume", "inspect", "gt-cache-gastown"}}; !reflect.DeepEqual(calls, want) {
		t.Fatalf("docker calls = %q, want %q: an unreadable volume is not created", calls, want)
	}
}

// A rig name reaches docker's argv through {rig}: a name docker would not take,
// or one that reads as a flag, stops before any docker call.
func TestEnsureCacheVolumeRefusesAnInvalidName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "-v", "--rm", "gt cache", "gt-cache-a/b", "gt-cache-a;b", "gt-cache-a\nb"} {
		d := &dockerRecorder{absent: true}
		err := ensureCacheVolume(context.Background(), d.run, name)
		if err == nil {
			t.Errorf("name %q: want an error", name)
		}
		if errors.Is(err, ErrCacheVolume) {
			t.Errorf("name %q: err = %v; a bad name is not a defer-able precondition", name, err)
		}
		if len(d.calls) != 0 {
			t.Errorf("name %q: docker calls = %q, want none", name, d.calls)
		}
	}
}

func TestEnsureCacheVolumeRefusesANilRunner(t *testing.T) {
	t.Parallel()
	if err := ensureCacheVolume(context.Background(), nil, "gt-cache-gastown"); err == nil {
		t.Fatal("want an error, not a nil-func panic")
	}
}

// The volume is ensured before the sandbox create: both docker calls happen
// while the sandbox has made no calls.
func TestOpenShellPrepareEnsuresTheCacheVolumeBeforeTheCreate(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellCachedProfile(), fake)
	d := &dockerRecorder{absent: true}
	var sandboxCalls []int
	b.docker = func(ctx context.Context, args ...string) ([]byte, []byte, error) {
		sandboxCalls = append(sandboxCalls, len(fake.Calls()))
		return d.run(ctx, args...)
	}

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if want := [][]string{{"volume", "inspect", "gt-cache-gastown"}, openshellTestCacheCreate}; !reflect.DeepEqual(d.calls, want) {
		t.Fatalf("docker calls = %q, want %q", d.calls, want)
	}
	if want := []int{0, 0}; !reflect.DeepEqual(sandboxCalls, want) {
		t.Fatalf("sandbox calls before each docker call = %v, want %v: the volume exists before the create", sandboxCalls, want)
	}
}

// A volume that cannot be made stops Prepare before the push and the sandbox,
// and the error keeps ErrCacheVolume for the dispatcher.
func TestOpenShellPrepareStopsWhenTheCacheVolumeCannotBeMade(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{}
	b, pushes := openshellTestBackend(town, openshellCachedProfile(), fake)
	b.docker = (&dockerRecorder{absent: true, createErr: errors.New("exit status 1")}).run

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if !errors.Is(err, ErrCacheVolume) {
		t.Fatalf("err = %v, want ErrCacheVolume", err)
	}
	if len(*pushes) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("pushes = %d, sandbox calls = %d; a missing volume must touch nothing", len(*pushes), len(fake.Calls()))
	}
}

func TestOpenShellPrepareMakesNoDockerCallWithoutACacheVolume(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	d := &dockerRecorder{absent: true}
	b.docker = d.run

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("docker calls = %q, want none for a profile with no cache volume", d.calls)
	}
}

// A kubernetes profile has no docker volumes: Prepare makes no docker call and
// the create carries no docker.mounts config, even if the profile names a
// volume (Validate refuses that, but the backend does not rely on it).
func TestOpenShellKubernetesProfileDoesNoDockerVolumeWork(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	p := openshellCachedProfile()
	p.Driver = DriverKubernetes
	b, _ := openshellTestBackend(town, p, fake)
	d := &dockerRecorder{absent: true}
	b.docker = d.run

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("docker calls = %q, want none for a kubernetes profile", d.calls)
	}
	cs, err := b.createSpec(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("createSpec: %v", err)
	}
	if cs.DriverConfigJSON != "" {
		t.Fatalf("DriverConfigJSON = %s, want none for a kubernetes profile", cs.DriverConfigJSON)
	}
}

// A kubernetes profile's placement and resources reach the create call as
// driver config, nested as the gateway's KubernetesPodDriverConfig reads it:
// node selector, tolerations, priority and runtime class under pod, requests
// and limits under containers.agent.resources.
func TestOpenShellKubernetesDriverConfig(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	p := Profile{
		Name:          "polecat-default",
		Image:         "img",
		Policy:        openshellTestProfile.Policy,
		Driver:        DriverKubernetes,
		NodeSelector:  map[string]string{"gt.dev/pool": "sandbox"},
		Tolerations:   []config.SandboxToleration{{Key: "gt.dev/sandbox", Operator: "Equal", Value: "true", Effect: "NoSchedule"}},
		PriorityClass: "gt-sandbox",
		RuntimeClass:  "gvisor",
		Requests:      &config.SandboxResources{CPU: "1", Memory: "2Gi"},
		Limits:        &config.SandboxResources{CPU: "2", Memory: "4Gi"},
	}
	b, _ := openshellTestBackend(town, p, &sandbox.Fake{})

	cs, err := b.createSpec(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("createSpec: %v", err)
	}
	want := `{"kubernetes":{"containers":{"agent":{"resources":{"limits":{"cpu":"2","memory":"4Gi"},"requests":{"cpu":"1","memory":"2Gi"}}}},"pod":{"node_selector":{"gt.dev/pool":"sandbox"},"priority_class_name":"gt-sandbox","runtime_class_name":"gvisor","tolerations":[{"key":"gt.dev/sandbox","operator":"Equal","value":"true","effect":"NoSchedule"}]}}}`
	if cs.DriverConfigJSON != want {
		t.Fatalf("DriverConfigJSON = %s, want %s", cs.DriverConfigJSON, want)
	}
}

// A kubernetes profile sends only the sections it sets: a node selector alone
// is the one-key pod the gateway's CLI help shows, and resources alone carry
// no empty pod object.
func TestOpenShellKubernetesDriverConfigSendsOnlyWhatIsSet(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name   string
		mutate func(*Profile)
		want   string
	}{
		{
			name:   "node selector alone",
			mutate: func(p *Profile) { p.NodeSelector = map[string]string{"gt.dev/pool": "sandbox"} },
			want:   `{"kubernetes":{"pod":{"node_selector":{"gt.dev/pool":"sandbox"}}}}`,
		},
		{
			name:   "limits alone",
			mutate: func(p *Profile) { p.Limits = &config.SandboxResources{Memory: "4Gi"} },
			want:   `{"kubernetes":{"containers":{"agent":{"resources":{"limits":{"memory":"4Gi"}}}}}}`,
		},
		{
			name:   "empty resources are not sent",
			mutate: func(p *Profile) { p.Requests = &config.SandboxResources{} },
			want:   "",
		},
		{
			name:   "a cache claim alone mounts the shared claim at the cache dir",
			mutate: func(p *Profile) { p.KubernetesCacheClaim = "gt-shared-cache" },
			want:   `{"kubernetes":{"containers":{"agent":{"volume_mounts":[{"name":"gt-cache","mount_path":"/sandbox/.cache","read_only":false}]}},"volumes":[{"name":"gt-cache","persistent_volume_claim":{"claim_name":"gt-shared-cache","read_only":false}}]}}`,
		},
		{
			name: "a cache claim with limits sends both",
			mutate: func(p *Profile) {
				p.KubernetesCacheClaim = "gt-shared-cache"
				p.Limits = &config.SandboxResources{Memory: "4Gi"}
			},
			want: `{"kubernetes":{"containers":{"agent":{"resources":{"limits":{"memory":"4Gi"}},"volume_mounts":[{"name":"gt-cache","mount_path":"/sandbox/.cache","read_only":false}]}},"volumes":[{"name":"gt-cache","persistent_volume_claim":{"claim_name":"gt-shared-cache","read_only":false}}]}}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			p := Profile{Name: "polecat-default", Image: "img", Policy: openshellTestProfile.Policy, Driver: DriverKubernetes}
			c.mutate(&p)
			b, _ := openshellTestBackend(town, p, &sandbox.Fake{})

			cs, err := b.createSpec(openshellSpawn(town, work))
			if err != nil {
				t.Fatalf("createSpec: %v", err)
			}
			if cs.DriverConfigJSON != c.want {
				t.Fatalf("DriverConfigJSON = %s, want %q", cs.DriverConfigJSON, c.want)
			}
		})
	}
}

// A driver that is neither docker nor kubernetes gets no docker volume work
// either: the backend names the docker driver instead of treating every other
// value as docker.
func TestOpenShellUnknownDriverDoesNoDockerVolumeWork(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	p := openshellCachedProfile()
	p.Driver = "podman"
	b, _ := openshellTestBackend(town, p, fake)
	d := &dockerRecorder{absent: true}
	b.docker = d.run

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if len(d.calls) != 0 {
		t.Fatalf("docker calls = %q, want none for an unknown driver", d.calls)
	}
	cs, err := b.createSpec(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("createSpec: %v", err)
	}
	if cs.DriverConfigJSON != "" {
		t.Fatalf("DriverConfigJSON = %s, want none for an unknown driver", cs.DriverConfigJSON)
	}
}

// A docker profile, named or defaulted, keeps the volume ensure and the mount.
func TestOpenShellDockerDriverKeepsTheCacheVolumeWork(t *testing.T) {
	t.Parallel()
	for _, driver := range []string{"", "docker"} {
		t.Run("driver="+driver, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
			p := openshellCachedProfile()
			p.Driver = driver
			b, _ := openshellTestBackend(town, p, fake)
			d := &dockerRecorder{absent: true}
			b.docker = d.run

			if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			if want := [][]string{{"volume", "inspect", "gt-cache-gastown"}, openshellTestCacheCreate}; !reflect.DeepEqual(d.calls, want) {
				t.Fatalf("docker calls = %q, want %q", d.calls, want)
			}
			cs, err := b.createSpec(openshellSpawn(town, work))
			if err != nil {
				t.Fatalf("createSpec: %v", err)
			}
			if !strings.Contains(cs.DriverConfigJSON, `"source":"gt-cache-gastown"`) {
				t.Fatalf("DriverConfigJSON = %s, want the cache mount", cs.DriverConfigJSON)
			}
		})
	}
}

func TestOpenShellCreateMountsTheCacheVolume(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	b, _ := openshellTestBackend(town, Profile{Name: "polecat-default", Image: "img", Policy: openshellTestProfile.Policy, CacheVolume: "gt-cache-{rig}"}, &sandbox.Fake{})

	cs, err := b.createSpec(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("createSpec: %v", err)
	}
	want := `{"docker":{"mounts":[{"read_only":false,"source":"gt-cache-gastown","target":"/sandbox/.cache","type":"volume"}]}}`
	if cs.DriverConfigJSON != want {
		t.Fatalf("DriverConfigJSON = %s, want %s", cs.DriverConfigJSON, want)
	}
}

func TestBuildWorkspaceArchiveSendsOnlyTheRuntimeFiles(t *testing.T) {
	t.Parallel()
	_, work := openshellTown(t)

	raw, err := buildWorkspaceArchive(work)
	if err != nil {
		t.Fatalf("buildWorkspaceArchive: %v", err)
	}
	var names []string
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		names = append(names, hdr.Name)
	}
	if want := []string{"CLAUDE.md", ".claude/commands/done.md"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("archive entries = %q, want %q", names, want)
	}

	empty, err := buildWorkspaceArchive(t.TempDir())
	if err != nil || empty != nil {
		t.Fatalf("empty worktree: archive=%d bytes, err=%v; want nil, nil", len(empty), err)
	}
}

func TestRedirectTargetPointsAtTheRigBeads(t *testing.T) {
	t.Parallel()
	// The host worktree's .beads/redirect says the same thing.
	if got := redirectTarget("gastown/polecats/sandbox-ace/gastown"); got != "../../../mayor/rig/.beads" {
		t.Fatalf("redirectTarget = %q", got)
	}
}

func TestOpenShellPrepareDropsHostOnlyEnv(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)

	prep, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	for _, k := range sandboxDroppedEnv {
		if v, ok := prep.Env[k]; !ok || v != "" {
			t.Errorf("Env[%s] = %q (present %v), want present and empty so the host value is dropped", k, v, ok)
		}
	}
	for k, v := range prep.Env {
		if strings.Contains(v, "HOST-ONLY-VALUE") || strings.Contains(v, "/host/only/config") {
			t.Errorf("Env[%s] carries a host-only value %q", k, v)
		}
	}
}

func TestOpenShellPrepareStopsWhenThePushFails(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	b.push = func(string, string) error { return errors.New("forgejo unreachable") }

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "pushing "+openshellTestBranch) {
		t.Fatalf("err = %v, want the push failure", err)
	}
	if len(fake.Calls()) != 0 {
		t.Fatalf("openshell calls = %q; a failed push must leave no sandbox behind", fake.Calls())
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("a failed push left a sandbox record behind")
	}
}

func TestOpenShellPrepareNamesAMissingReadyCondition(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Reply: map[string]string{"get": `{"name":"x","phase":"Provisioning"}`}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	b.wait = func(context.Context, time.Duration) error { return nil }

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), `reported no Ready condition after 150 polls (phase "Provisioning")`) {
		t.Fatalf("err = %v, want the missing-condition timeout", err)
	}
}

func TestOpenShellPrepareTimesOutWhenNeverReady(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellProvisioningReply}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	waits := 0
	b.wait = func(context.Context, time.Duration) error {
		waits++
		return nil
	}

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), `not Ready after 150 polls (phase "Provisioning")`) {
		t.Fatalf("err = %v, want the readiness timeout", err)
	}
	gets := 0
	for _, c := range fake.Calls() {
		if len(c) > 2 && c[2] == "get" {
			gets++
		}
	}
	if gets != readyPolls || waits != readyPolls-1 {
		t.Fatalf("gets = %d, waits = %d; want %d polls with %d pauses between them", gets, waits, readyPolls, readyPolls-1)
	}
	last := fake.Calls()[len(fake.Calls())-1]
	if want := []string{"openshell", "sandbox", "delete", openshellTestSession}; !reflect.DeepEqual(last, want) {
		t.Fatalf("last call = %q, want %q (rollback)", last, want)
	}
}

func TestOpenShellPrepareCancelDuringReadyWaitRollsBack(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellProvisioningReply}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	b.wait = func(context.Context, time.Duration) error {
		return context.Canceled
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := b.Prepare(ctx, openshellSpawn(town, work))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	last := fake.Calls()[len(fake.Calls())-1]
	if want := []string{"openshell", "sandbox", "delete", openshellTestSession}; !reflect.DeepEqual(last, want) {
		t.Fatalf("last call = %q, want %q (rollback despite the canceled spawn)", last, want)
	}
}

func TestOpenShellPrepareRollbackRemovesTheRecord(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	// A record left by an earlier partial write for this session.
	if err := WriteSandboxRecord(town, SandboxRecord{Session: openshellTestSession, Rig: openshellTestRig, Polecat: openshellTestPolecat, Profile: "polecat-default"}); err != nil {
		t.Fatal(err)
	}
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	run := b.cli.Run
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte, env []string) ([]byte, []byte, error) {
		if slices.ContainsFunc(argv, func(a string) bool { return a == cloneScript }) {
			return nil, []byte("fatal"), errors.New("exit status 128")
		}
		return run(ctx, argv, stdin, env)
	}

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
		t.Fatal("Prepare succeeded with a failing clone")
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("rollback left the sandbox record behind; routing would point at a deleted sandbox")
	}
}

// openshellTestPolicyRoot is where the tests' town keeps the gt source checkout,
// the repo a profile's policy path is relative to. The polecat's worktree is a
// different rig's repo and does not hold the policy.
func openshellTestPolicyRoot(townRoot string) string {
	return filepath.Join(townRoot, "gastown", "mayor", "rig")
}

// The profile's policy is the town's copy in the gt source checkout, whichever
// rig the polecat works in: a worktree without deploy/polecat/policy.yaml (every
// rig but gastown) still gets the sandbox, and the create names the town's
// file.
func TestOpenShellPolicyComesFromTheTownCopyNotTheWorktree(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	if _, err := os.Stat(filepath.Join(work, "deploy", "polecat", "policy.yaml")); err == nil {
		t.Fatalf("the test worktree %s has a policy; it must lack one to prove the town copy is used", work)
	}
	b, _ := openshellTestBackend(town, openshellTestProfile, &sandbox.Fake{})

	cs, err := b.createSpec(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("createSpec: %v", err)
	}
	if want := filepath.Join(openshellTestPolicyRoot(town), "deploy", "polecat", "policy.yaml"); cs.PolicyPath != want {
		t.Fatalf("PolicyPath = %q, want the town copy %q", cs.PolicyPath, want)
	}
}

// The backend a town gets from newOpenShellBackend resolves the policy with the
// production resolver, not the test seam: the gt source checkout under the town.
func TestOpenShellProductionPolicyRootIsTheTownCheckout(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	b, err := newOpenShellBackend(town, openshellTestProfile)
	if err != nil {
		t.Fatalf("newOpenShellBackend: %v", err)
	}
	cs, err := b.(*openshellBackend).createSpec(openshellSpawn(town, work))
	if err != nil {
		t.Fatalf("createSpec: %v", err)
	}
	if want := filepath.Join(openshellTestPolicyRoot(town), "deploy", "polecat", "policy.yaml"); cs.PolicyPath != want {
		t.Fatalf("PolicyPath = %q, want the town copy %q", cs.PolicyPath, want)
	}
}

// A town whose gt source checkout lacks the policy file is refused before
// anything is pushed or created, and the error names the checkout.
func TestOpenShellPrepareRefusesAPolicyMissingFromTheCheckout(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	if err := os.Remove(filepath.Join(openshellTestPolicyRoot(town), "deploy", "polecat", "policy.yaml")); err != nil {
		t.Fatal(err)
	}
	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), openshellTestPolicyRoot(town)) {
		t.Fatalf("Prepare with the policy missing = %v; want an error naming the checkout %s", err, openshellTestPolicyRoot(town))
	}
	if len(*pushes) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("pushes = %d, openshell calls = %d; a missing policy must touch nothing", len(*pushes), len(fake.Calls()))
	}
}

// A policy path that is empty, absolute or climbs out of the source checkout is
// refused before anything is pushed or created; a town with no gt source
// checkout is an error naming the policy, not a path into the worktree.
func TestOpenShellPolicyPathStaysInTheSourceCheckout(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	for _, policy := range []string{"", "/etc/policy.yaml", "../../../other/policy.yaml", ".."} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
			p := openshellTestProfile
			p.Policy = policy
			b, pushes := openshellTestBackend(town, p, fake)

			if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
				t.Fatalf("Prepare accepted policy %q", policy)
			}
			if len(*pushes) != 0 || len(fake.Calls()) != 0 {
				t.Fatalf("pushes = %d, openshell calls = %d; a bad policy must touch nothing", len(*pushes), len(fake.Calls()))
			}
		})
	}
	root := openshellTestPolicyRoot(town)
	if got, err := confinedPath(root, "deploy/polecat/policy.yaml"); err != nil || got != filepath.Join(root, "deploy/polecat/policy.yaml") {
		t.Fatalf("confinedPath = %q, %v; want the policy under the source checkout", got, err)
	}

	fake := openshellPrepareFake(map[string]string{"get": openshellReadyReply})
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)
	b.policyRoot = func(string) (string, error) { return "", errors.New("no gt source checkout") }
	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "deploy/polecat/policy.yaml") || !strings.Contains(err.Error(), "no gt source checkout") {
		t.Fatalf("Prepare without a source checkout = %v; want an error naming the policy and the cause", err)
	}
	if len(*pushes) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("pushes = %d, openshell calls = %d; a missing source checkout must touch nothing", len(*pushes), len(fake.Calls()))
	}
}

func TestOpenShellStageDirIsPrivate(t *testing.T) {
	t.Parallel()
	town, _ := openshellTown(t)
	b, _ := openshellTestBackend(town, openshellTestProfile, &sandbox.Fake{})

	stage, err := b.stageDir()
	if err != nil {
		t.Fatalf("stageDir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(stage) })
	info, err := os.Stat(stage)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o700 {
		t.Fatalf("stage dir mode = %o, want 0700 (it holds the skeleton)", mode)
	}
}

// The main process holds the tmux server under a fixed name; the session start
// creates the polecat's session on the same server, and tmux refuses a
// duplicate (found on a live sandbox, gt-t1798).
func TestOpenShellMainScriptHolderIsNotThePolecatSession(t *testing.T) {
	t.Parallel()
	// Every session name the town builds carries a rig or role prefix and a
	// hyphen, so the bare holder name cannot collide with any of them. Check
	// the guarantee against each shape, not only the fixture id.
	for _, sess := range []string{
		openshellTestSession,
		PolecatSessionName(DefaultPrefix, openshellTestPolecat),
		PolecatSessionName("bd", "furiosa"),
		CrewSessionName(DefaultPrefix, "sloan"),
		OverseerSessionName(),
	} {
		if sandboxHolderSession == sess {
			t.Fatalf("holder session %q equals the polecat session %q", sandboxHolderSession, sess)
		}
	}
	if !strings.Contains(sandboxMainScript, "-s "+sandboxHolderSession+" ") {
		t.Fatalf("main script %q does not create the holder session %q", sandboxMainScript, sandboxHolderSession)
	}
	if strings.Contains(sandboxMainScript, `new-session -d -s "$1"`) || strings.Contains(sandboxMainScript, `new-session -d -s "$S"`) {
		t.Fatalf("main script %q creates the polecat's session as the holder; the host's session start creates that one", sandboxMainScript)
	}
}

// A commit mismatch is ErrImageDrift, which the dispatcher defers on. A missing
// commit is a plain refusal, not drift, so the dispatcher labels the bead instead.
func TestCheckImageGTClassifiesDrift(t *testing.T) {
	t.Parallel()
	err := checkImageGT("gastown-polecat:dev", gtLabelSource, "aaaaaaa1111", "bbbbbbb2222")
	if !errors.Is(err, ErrImageDrift) {
		t.Fatalf("a differing commit = %v, want ErrImageDrift", err)
	}
	if want := "sandbox image gastown-polecat:dev carries gt commit aaaaaaa1111, but the installed gt is commit bbbbbbb2222; rebuild the image with make polecat-image"; err.Error() != want {
		t.Errorf("message = %q, want %q", err.Error(), want)
	}
	if err := checkImageGT("gastown-polecat:dev", gtLabelSource, "", "bbbbbbb2222"); errors.Is(err, ErrImageDrift) {
		t.Errorf("a missing image commit = %v, must not be ErrImageDrift", err)
	}
	if err := checkImageGT("gastown-polecat:dev", gtLabelSource, "bbbbbbb2222", "bbbbbbb2222"); err != nil {
		t.Errorf("a matching commit = %v, want nil", err)
	}
}
