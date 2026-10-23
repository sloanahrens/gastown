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
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/sandbox"
)

const (
	openshellTestRig     = "gastown"
	openshellTestPolecat = "sandbox-ace"
	openshellTestSession = "gt-gastown-sandbox-ace"
	openshellTestBranch  = "polecat/sandbox-ace/gt-1+abc"
	openshellTestRepoURL = "http://host.docker.internal:3000/sloan/gastown.git"
	openshellTestWorkDir = "/sandbox/gt/gastown/polecats/sandbox-ace/gastown"

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
// gt writes there, plus one file that must not be sent.
func openshellTown(t *testing.T) (town, work string) {
	t.Helper()
	town = t.TempDir()
	work = filepath.Join(town, openshellTestRig, "polecats", openshellTestPolecat, openshellTestRig)
	writeOpenshellFile(t, filepath.Join(work, "CLAUDE.md"), "role context\n")
	writeOpenshellFile(t, filepath.Join(work, ".claude", "commands", "done.md"), "done\n")
	writeOpenshellFile(t, filepath.Join(work, ".claude", "settings.local.json"), "{}\n")
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
			*pushes = append(*pushes, pushCall{bare: bare, branch: branch, sandboxCalls: len(fake.Calls)})
			return nil
		},
	}
	return b, pushes
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
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
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
		"--policy", filepath.Join(work, "deploy/polecat/policy.yaml"),
		"--provider", "deepseek", "--provider", "forgejo-polecat",
		"--cpu", "4", "--memory", "8Gi",
		"--label", "polecat=sandbox-ace", "--label", "rig=gastown",
		"--env", "BD_ACTOR=gastown/polecats/sandbox-ace",
		"--env", "BD_DISABLE_METRICS=1",
		"--env", "BEADS_DOLT_SERVER_HOST=host.docker.internal",
		"--env", "BEADS_DOLT_SERVER_MODE=1",
		"--env", "BEADS_DOLT_SERVER_PORT=3307",
		"--env", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"--env", "DISABLE_AUTOUPDATER=1",
		"--env", "DO_NOT_TRACK=1",
		"--env", "GT_POLECAT=sandbox-ace",
		"--env", "GT_RIG=gastown",
		"--env", "GT_ROLE=gastown/polecats/sandbox-ace",
		"--env", "GT_ROOT=/sandbox/gt",
		"--env", "GT_SESSION_BACKEND=openshell",
		"--env", "GT_TOWN_ROOT=/sandbox/gt",
		"--detach", "--output", "json",
		"--", "sh", "-c", sandboxMainScript, "sh", openshellTestSession,
	}
	want := [][]string{
		create,
		{"openshell", "sandbox", "get", openshellTestSession, "--output", "json"},
		{"openshell", "sandbox", "upload", "--no-git-ignore", openshellTestSession, "STAGE/town-skel.tgz", "/sandbox/"},
		sandboxExec("tar", "xzf", "/sandbox/town-skel.tgz", "-C", "/sandbox"),
		sandboxExec("sh", "-c", cloneScript, "sh", openshellTestBranch, openshellTestRepoURL, openshellTestWorkDir),
		{"openshell", "sandbox", "upload", "--no-git-ignore", openshellTestSession, "STAGE/workspace.tgz", "/sandbox/"},
		sandboxExec("tar", "xzf", "/sandbox/workspace.tgz", "-C", openshellTestWorkDir),
		sandboxExec("sh", "-c", redirectScript, "sh", openshellTestWorkDir, "../../../mayor/rig/.beads"),
	}
	if got := stageNames(fake.Calls); !reflect.DeepEqual(got, want) {
		t.Fatalf("openshell calls:\n got %q\nwant %q", got, want)
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
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
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
	for k, want := range map[string]string{
		"GT_TOWN_ROOT":           "/sandbox/gt",
		"GT_ROOT":                "/sandbox/gt",
		"GT_POLECAT_PATH":        openshellTestWorkDir,
		"GT_SESSION_BACKEND":     "openshell",
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

func TestOpenShellPrepareWaitsForReadyBetweenPolls(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	run := b.cli.Run
	gets := 0
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
		if len(argv) > 2 && argv[2] == "get" {
			gets++
			if gets == 1 {
				return []byte(openshellProvisioningReply), nil, nil
			}
			return []byte(openshellReadyReply), nil, nil
		}
		return run(ctx, argv, stdin)
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

func TestOpenShellPrepareCreateFailureMakesNoFurtherCalls(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Err: map[string]error{"create": errors.New("gateway unreachable")}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "creating sandbox") {
		t.Fatalf("err = %v, want creating sandbox", err)
	}
	if len(fake.Calls) != 1 || fake.Calls[0][2] != "create" {
		t.Fatalf("calls = %q, want only the create", fake.Calls)
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("a failed Prepare left a sandbox record behind")
	}
}

func TestOpenShellPrepareCloneFailureDeletesTheSandbox(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	run := b.cli.Run
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
		if slices.ContainsFunc(argv, func(a string) bool { return a == cloneScript }) {
			return nil, []byte("fatal: unable to access the repository"), errors.New("exit status 128")
		}
		return run(ctx, argv, stdin)
	}

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "cloning "+openshellTestBranch) {
		t.Fatalf("err = %v, want the clone failure", err)
	}
	last := fake.Calls[len(fake.Calls)-1]
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
	if len(*pushes) != 0 || len(fake.Calls) != 0 {
		t.Fatalf("pushes = %d, openshell calls = %d; a bad spec must touch nothing", len(*pushes), len(fake.Calls))
	}
}

func TestOpenShellCreateMountsTheCacheVolume(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	b, _ := openshellTestBackend(town, Profile{Name: "polecat-default", Image: "img", Policy: "p.yaml", CacheVolume: "gt-cache-{rig}"}, &sandbox.Fake{})

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
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
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
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	b.push = func(string, string) error { return errors.New("forgejo unreachable") }

	_, err := b.Prepare(context.Background(), openshellSpawn(town, work))
	if err == nil || !strings.Contains(err.Error(), "pushing "+openshellTestBranch) {
		t.Fatalf("err = %v, want the push failure", err)
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("openshell calls = %q; a failed push must leave no sandbox behind", fake.Calls)
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("a failed push left a sandbox record behind")
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
	for _, c := range fake.Calls {
		if len(c) > 2 && c[2] == "get" {
			gets++
		}
	}
	if gets != readyPolls || waits != readyPolls-1 {
		t.Fatalf("gets = %d, waits = %d; want %d polls with %d pauses between them", gets, waits, readyPolls, readyPolls-1)
	}
	last := fake.Calls[len(fake.Calls)-1]
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
	last := fake.Calls[len(fake.Calls)-1]
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
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
	b, _ := openshellTestBackend(town, openshellTestProfile, fake)
	run := b.cli.Run
	b.cli.Run = func(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, error) {
		if slices.ContainsFunc(argv, func(a string) bool { return a == cloneScript }) {
			return nil, []byte("fatal"), errors.New("exit status 128")
		}
		return run(ctx, argv, stdin)
	}

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
		t.Fatal("Prepare succeeded with a failing clone")
	}
	if _, ok, _ := ReadSandboxRecord(town, openshellTestSession); ok {
		t.Fatal("rollback left the sandbox record behind; routing would point at a deleted sandbox")
	}
}

func TestOpenShellPolicyPathStaysInTheWorktree(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	for _, policy := range []string{"", "/etc/policy.yaml", "../../../other/policy.yaml", ".."} {
		t.Run(policy, func(t *testing.T) {
			t.Parallel()
			fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
			p := openshellTestProfile
			p.Policy = policy
			b, pushes := openshellTestBackend(town, p, fake)

			if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
				t.Fatalf("Prepare accepted policy %q", policy)
			}
			if len(*pushes) != 0 || len(fake.Calls) != 0 {
				t.Fatalf("pushes = %d, openshell calls = %d; a bad policy must touch nothing", len(*pushes), len(fake.Calls))
			}
		})
	}
	if got, err := confinedPath(work, "deploy/polecat/policy.yaml"); err != nil || got != filepath.Join(work, "deploy/polecat/policy.yaml") {
		t.Fatalf("confinedPath = %q, %v; want the policy under the worktree", got, err)
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
