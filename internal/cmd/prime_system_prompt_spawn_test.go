package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// A town with one rig and one polecat laid out the way polecat.Manager creates
// them: <town>/<rig>/polecats/<name>/<rig> is the worktree the session starts in.
func newSpawnRenderTown(t *testing.T, rigName, polecat string) (town, rigPath string) {
	t.Helper()
	town = t.TempDir()
	rigPath = filepath.Join(town, rigName)
	for _, d := range []string{
		filepath.Join(rigPath, "polecats", polecat, rigName),
		filepath.Join(rigPath, "witness"),
		filepath.Join(rigPath, "refinery", "rig"),
		filepath.Join(town, "mayor"),
		filepath.Join(town, "deacon"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return town, rigPath
}

func TestRenderSystemPromptFileForSpawn_PolecatMatchesInSessionPrime(t *testing.T) {
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	path := config.SystemPromptFilePath("polecat", town, rigPath, "nux")

	if err := renderSystemPromptFileForSpawn("polecat", town, rigPath, "nux", path); err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("file not written: %v", err)
	}
	if !strings.Contains(string(got), "polecats/nux") {
		t.Fatalf("rendered text does not name the polecat worktree:\n%.400s", got)
	}

	// gt prime inside the session builds its RoleContext from GT_ROLE and the
	// worktree cwd; the spawn-time render must produce the identical text, so
	// prime's refresh on the first run is a no-op instead of a rewrite.
	want, fromTemplate, err := staticRoleText(RoleContext{
		Role:     RolePolecat,
		Rig:      "myrig",
		Polecat:  "nux",
		TownRoot: town,
		WorkDir:  filepath.Join(rigPath, "polecats", "nux", "myrig"),
	})
	if err != nil || !fromTemplate {
		t.Fatalf("staticRoleText: err=%v fromTemplate=%v", err, fromTemplate)
	}
	if string(got) != want {
		t.Fatalf("spawn-time render differs from in-session render")
	}
	if wrote, err := writeSystemPromptFile(path, want); err != nil || wrote {
		t.Fatalf("prime's refresh must find the file current: wrote=%v err=%v", wrote, err)
	}
}

// Every role that has a file must render at spawn time exactly what gt prime
// renders from inside the session (GT_ROLE plus the session cwd), otherwise
// prime's refresh rewrites the file on every first run.
func TestRenderSystemPromptFileForSpawn_AllRolesMatchInSessionPrime(t *testing.T) {
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	if err := os.MkdirAll(filepath.Join(rigPath, "crew", "sloan"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		role  string
		agent string
		prime RoleContext // what gt prime derives inside the live session
	}{
		{"polecat", "nux", RoleContext{Role: RolePolecat, Rig: "myrig", Polecat: "nux", TownRoot: town, WorkDir: filepath.Join(rigPath, "polecats", "nux", "myrig")}},
		{"crew", "sloan", RoleContext{Role: RoleCrew, Rig: "myrig", Polecat: "sloan", TownRoot: town, WorkDir: filepath.Join(rigPath, "crew", "sloan")}},
		{"witness", "", RoleContext{Role: RoleWitness, Rig: "myrig", TownRoot: town, WorkDir: filepath.Join(rigPath, "witness")}},
		{"refinery", "", RoleContext{Role: RoleRefinery, Rig: "myrig", TownRoot: town, WorkDir: filepath.Join(rigPath, "refinery", "rig")}},
		{"mayor", "", RoleContext{Role: RoleMayor, TownRoot: town, WorkDir: filepath.Join(town, "mayor")}},
		{"deacon", "", RoleContext{Role: RoleDeacon, TownRoot: town, WorkDir: filepath.Join(town, "deacon")}},
		// gt prime inside a dog session derives the dog from its kennel cwd
		// (roleContextFromDir) and never sets Rig.
		{"dog", "alpha", RoleContext{Role: RoleDog, Polecat: "alpha", TownRoot: town, WorkDir: filepath.Join(town, "deacon", "dogs", "alpha")}},
	}
	for _, tc := range cases {
		path := config.SystemPromptFilePath(tc.role, town, rigPath, tc.agent)
		if path == "" {
			t.Fatalf("%s: no system prompt path", tc.role)
		}
		if err := renderSystemPromptFileForSpawn(tc.role, town, rigPath, tc.agent, path); err != nil {
			t.Fatalf("%s: render: %v", tc.role, err)
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		want, fromTemplate, err := staticRoleText(tc.prime)
		if err != nil || !fromTemplate {
			t.Fatalf("%s: staticRoleText err=%v fromTemplate=%v", tc.role, err, fromTemplate)
		}
		if string(got) != want {
			t.Errorf("%s: spawn-time render differs from the in-session render", tc.role)
		}
	}
}

func TestSpawnRoleContext_WorkDirsPerRole(t *testing.T) {
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	cases := []struct {
		role, agent, wantWorkDir string
	}{
		{"polecat", "nux", filepath.Join(rigPath, "polecats", "nux", "myrig")},
		{"crew", "sloan", filepath.Join(rigPath, "crew", "sloan")},
		{"witness", "", filepath.Join(rigPath, "witness")},
		{"refinery", "", filepath.Join(rigPath, "refinery", "rig")},
		{"mayor", "", filepath.Join(town, "mayor")},
		{"deacon", "", filepath.Join(town, "deacon")},
		{"dog", "alpha", filepath.Join(town, "deacon", "dogs", "alpha")},
	}
	for _, tc := range cases {
		ctx, err := spawnRoleContext(tc.role, town, rigPath, tc.agent)
		if err != nil {
			t.Fatalf("%s: %v", tc.role, err)
		}
		if ctx.WorkDir != tc.wantWorkDir {
			t.Errorf("%s: WorkDir = %q, want %q", tc.role, ctx.WorkDir, tc.wantWorkDir)
		}
		if ctx.TownRoot != town {
			t.Errorf("%s: TownRoot = %q", tc.role, ctx.TownRoot)
		}
	}

	// Legacy layout: polecats/<name> with no nested clone is the session cwd.
	if err := os.RemoveAll(filepath.Join(rigPath, "polecats", "nux", "myrig")); err != nil {
		t.Fatal(err)
	}
	ctx, err := spawnRoleContext("polecat", town, rigPath, "nux")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(rigPath, "polecats", "nux"); ctx.WorkDir != want {
		t.Fatalf("legacy polecat WorkDir = %q, want %q", ctx.WorkDir, want)
	}
}

func TestSpawnRoleContext_RejectsRolesWithoutAFile(t *testing.T) {
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	for _, tc := range []struct{ role, rig, agent string }{
		{"dog", "", ""}, // no kennel name, no file
		{"boot", "", ""},
		{"polecat", rigPath, ""},
		{"witness", "", ""},
		{"nonsense", rigPath, "x"},
	} {
		if _, err := spawnRoleContext(tc.role, town, tc.rig, tc.agent); err == nil {
			t.Errorf("%+v: expected an error", tc)
		}
	}
	if err := renderSystemPromptFileForSpawn("dog", town, "", "alpha", ""); !errors.Is(err, errNoSystemPromptForRole) {
		t.Fatalf("empty path must report no system prompt for the role, got %v", err)
	}
}

// The end-to-end shape of gt-t30p: resolving a polecat's runtime config in a
// town where its system prompt file has never been written must still carry
// --append-system-prompt-file, because this package's init installed the
// renderer. Before the fix the flag appeared only from the second spawn on.
func TestResolveRoleAgentConfig_FirstPolecatSpawnCarriesSystemPromptFlag(t *testing.T) {
	town, rigPath := newSpawnRenderTown(t, "myrig", "nux")
	path := config.SystemPromptFilePath("polecat", town, rigPath, "nux")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: file must not exist yet (%v)", err)
	}

	rc, err := config.ResolveRoleAgentConfigWithOverride("polecat", town, rigPath, "", "nux")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(rc.Command) != "claude" {
		t.Skipf("default runtime here is %q, the flag applies to Claude agents only", rc.Command)
	}
	found := false
	for i, a := range rc.Args {
		if a == "--append-system-prompt-file" {
			found = true
			if i+1 >= len(rc.Args) || rc.Args[i+1] != path {
				t.Fatalf("flag value = %v, want %s", rc.Args, path)
			}
		}
	}
	if !found {
		t.Fatalf("first spawn resolved without the system prompt flag: %v", rc.Args)
	}
	if rc.Env[config.EnvSystemPromptFile] != path {
		t.Fatalf("env %s = %q, want %q", config.EnvSystemPromptFile, rc.Env[config.EnvSystemPromptFile], path)
	}
	if info, err := os.Stat(path); err != nil || info.Size() == 0 {
		t.Fatalf("file must have been rendered at resolve time: %v", err)
	}
}

// The dog shape of gt-t30p (gt-h7e5): the dog role template is ~8.5 KB, so a
// dog session that prints it in the hook pushes the prime past Claude Code's
// 10,000-character hook budget and gets truncated to a preview. This walks the
// real spawn path — AgentEnv (GT_DOG_NAME) → role config → renderer → command —
// and requires the flag on the FIRST spawn, before any file exists.
func TestBuildStartupCommand_FirstDogSpawnCarriesSystemPromptFlag(t *testing.T) {
	town, _ := newSpawnRenderTown(t, "myrig", "nux")
	path := config.SystemPromptFilePath("dog", town, "", "alpha")
	if path == "" {
		t.Fatal("dog must have a per-agent system prompt path")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("precondition: file must not exist yet (%v)", err)
	}

	cmd, err := config.BuildStartupCommandFromConfig(config.AgentEnvConfig{
		Role:      "dog",
		AgentName: "alpha",
		TownRoot:  town,
		Prompt:    "check your hook",
	}, "", "check your hook", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(cmd, "GT_DOG_NAME=alpha") {
		t.Fatalf("dog command must carry its name (the only handle on its kennel):\n%s", cmd)
	}
	if !strings.Contains(cmd, "--append-system-prompt-file") {
		t.Fatalf("first dog spawn must carry the system prompt flag:\n%s", cmd)
	}
	if !strings.Contains(cmd, config.EnvSystemPromptFile+"=") || !strings.Contains(cmd, path) {
		t.Fatalf("dog command must export %s=%s:\n%s", config.EnvSystemPromptFile, path, cmd)
	}
	got, err := os.ReadFile(path)
	if err != nil || len(got) == 0 {
		t.Fatalf("file must have been rendered at spawn time: %v", err)
	}
	if !strings.Contains(string(got), "alpha") {
		t.Fatalf("rendered dog text does not name the dog:\n%.200s", got)
	}

	// The file is load-bearing, not decorative (gt-mbuf): prime omits the dog's
	// role text from the hook only while GT_SYSTEM_PROMPT_FILE points at a
	// written file, and that text plus the dynamic sections is well over the
	// hook budget, so losing it hands the dog a 2 KB preview of its own work.
	t.Setenv(config.EnvSystemPromptFile, path)
	if !primeStaticTextDelivered() {
		t.Fatal("prime does not read back the file the first spawn wrote, so it reprints the dog role text into the hook")
	}
}
