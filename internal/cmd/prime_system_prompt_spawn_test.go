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
		{"dog", "", "alpha"},
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
