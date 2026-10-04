package templates

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if tmpl == nil {
		t.Fatal("New() returned nil")
	}
}

// TestRenderRole_MayorGone pins the mayor role's retirement: mayor.md.tmpl is
// deleted, so rendering the role must fail rather than resurrect it (gt-rwp7z.13).
func TestRenderRole_MayorGone(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if _, err := tmpl.RenderRole("mayor", RoleData{Role: "mayor", TownRoot: "/test/town"}); err == nil {
		t.Error(`RenderRole("mayor") succeeded; mayor.md.tmpl should be deleted`)
	}
}

func TestRenderRole_Polecat(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	data := RoleData{
		Role:          "polecat",
		RigName:       "myrig",
		TownRoot:      "/test/town",
		TownName:      "town",
		WorkDir:       "/test/town/myrig/polecats/TestCat",
		DefaultBranch: "main",
		Polecat:       "TestCat",
		MayorSession:  "gt-town-mayor",
	}

	output, err := tmpl.RenderRole("polecat", data)
	if err != nil {
		t.Fatalf("RenderRole() error = %v", err)
	}

	// Check for key content
	if !strings.Contains(output, "Polecat Context") {
		t.Error("output missing 'Polecat Context'")
	}
	if !strings.Contains(output, "TestCat") {
		t.Error("output missing polecat name")
	}
	if !strings.Contains(output, "myrig") {
		t.Error("output missing rig name")
	}
}

func TestRenderRole_PolecatForkRigUsesPRWorkflow(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	output, err := tmpl.RenderRole("polecat", RoleData{
		Role:          "polecat",
		RigName:       "myrig",
		TownRoot:      "/test/town",
		TownName:      "town",
		WorkDir:       "/test/town/myrig/polecats/TestCat",
		DefaultBranch: "main",
		IsForkRig:     true,
		UpstreamURL:   "https://example.com/upstream/repo.git",
		Polecat:       "TestCat",
		MayorSession:  "gt-town-mayor",
	})
	if err != nil {
		t.Fatalf("RenderRole() error = %v", err)
	}

	for _, want := range []string{"Fork-backed rig", "GitHub PR/no-merge workflow", "Do NOT submit upstream changes for local landing"} {
		if !strings.Contains(output, want) {
			t.Fatalf("fork polecat output missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{"Landing Workflow (gastown, beads repos)", "the landing worker lands on main", "Lands your work when complete"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("fork polecat output contains stale MQ guidance %q:\n%s", forbidden, output)
		}
	}
}

func TestRenderRole_CrewForkRigUsesPRWorkflow(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	output, err := tmpl.RenderRole("crew", RoleData{
		Role:          "crew",
		RigName:       "myrig",
		TownRoot:      "/test/town",
		TownName:      "town",
		WorkDir:       "/test/town/myrig/crew/alex",
		DefaultBranch: "main",
		IsForkRig:     true,
		UpstreamURL:   "https://example.com/upstream/repo.git",
		Polecat:       "alex",
		MayorSession:  "gt-town-mayor",
	})
	if err != nil {
		t.Fatalf("RenderRole() error = %v", err)
	}

	for _, want := range []string{"Fork-backed rig", "Fork-Backed PR Workflow", "git fetch upstream main", "gh pr create --base main"} {
		if !strings.Contains(output, want) {
			t.Fatalf("fork crew output missing %q:\n%s", want, output)
		}
	}
	for _, forbidden := range []string{"Crew workers push directly to main", "git push                    # Direct to main", "Refinery immediately", "origin/main", "commit directly to main"} {
		if strings.Contains(output, forbidden) {
			t.Fatalf("fork crew output contains stale direct-main guidance %q:\n%s", forbidden, output)
		}
	}
}

// TestRenderRole_NoHardcodedGtPath verifies that no role template renders
// a literal "~/gt" path — all path references must use {{ .TownRoot }}.
// This is a regression test for instances running outside ~/gt
// (e.g., test instances at a custom path).
func TestRenderRole_NoHardcodedGtPath(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const customTownRoot2 = "/custom/test/instance"

	roles := []struct {
		role string
		data RoleData
	}{
		{
			role: "polecat",
			data: RoleData{
				Role: "polecat", RigName: "myrig", Polecat: "TestCat",
				TownRoot: customTownRoot2, TownName: "instance",
				WorkDir:       customTownRoot2 + "/myrig/polecats/TestCat",
				DefaultBranch: "main",
				MayorSession:  "gt-instance-mayor",
			},
		},
		{
			role: "crew",
			data: RoleData{
				Role: "crew", RigName: "myrig", Polecat: "TestCrew",
				TownRoot: customTownRoot2, TownName: "instance",
				WorkDir:       customTownRoot2 + "/myrig/crew/TestCrew",
				DefaultBranch: "main",
				MayorSession:  "gt-instance-mayor",
			},
		},
	}

	for _, tc := range roles {
		t.Run(tc.role, func(t *testing.T) {
			output, err := tmpl.RenderRole(tc.role, tc.data)
			if err != nil {
				t.Fatalf("RenderRole(%q) error = %v", tc.role, err)
			}
			if strings.Contains(output, "~/gt") {
				var offending []string
				for i, line := range strings.Split(output, "\n") {
					if strings.Contains(line, "~/gt") {
						offending = append(offending, fmt.Sprintf("  line %d: %s", i+1, strings.TrimSpace(line)))
					}
				}
				t.Errorf("rendered %q template still contains hardcoded ~/gt (TownRoot=%q):\n%s",
					tc.role, customTownRoot2, strings.Join(offending, "\n"))
			}
		})
	}
}

// TestRenderRole_NoBDCreateRepoFlag verifies that no role template instructs
// agents to use `bd create --repo <rig>`. That flag silently loses writes
// (be-6mk) — the correct pattern is to cd into the target rig's beads
// directory first, then run a bare `bd create`.
func TestRenderRole_NoBDCreateRepoFlag(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const customTownRoot3 = "/custom/test/instance"

	roles := []struct {
		role string
		data RoleData
	}{
		{
			role: "polecat",
			data: RoleData{
				Role: "polecat", RigName: "myrig", Polecat: "TestCat",
				TownRoot: customTownRoot3, TownName: "instance",
				WorkDir:       customTownRoot3 + "/myrig/polecats/TestCat",
				DefaultBranch: "main",
				MayorSession:  "gt-instance-mayor",
			},
		},
		{
			role: "crew",
			data: RoleData{
				Role: "crew", RigName: "myrig", Polecat: "TestCrew",
				TownRoot: customTownRoot3, TownName: "instance",
				WorkDir:       customTownRoot3 + "/myrig/crew/TestCrew",
				DefaultBranch: "main",
				MayorSession:  "gt-instance-mayor",
			},
		},
	}

	for _, tc := range roles {
		t.Run(tc.role, func(t *testing.T) {
			output, err := tmpl.RenderRole(tc.role, tc.data)
			if err != nil {
				t.Fatalf("RenderRole(%q) error = %v", tc.role, err)
			}
			if strings.Contains(output, "create --repo") {
				var offending []string
				for i, line := range strings.Split(output, "\n") {
					if strings.Contains(line, "create --repo") {
						offending = append(offending, fmt.Sprintf("  line %d: %s", i+1, strings.TrimSpace(line)))
					}
				}
				t.Errorf("rendered %q template still instructs 'bd create --repo' (be-6mk):\n%s",
					tc.role, strings.Join(offending, "\n"))
			}
		})
	}
}

// TestRenderRole_TownRootInOutput verifies that the actual TownRoot value
// appears in the rendered output for roles that reference it in path instructions.
func TestRenderRole_TownRootInOutput(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const customRoot = "/Users/pa/dev/gastown-tests/my-instance"

	roles := []struct {
		role string
		data RoleData
	}{
		{
			role: "polecat",
			data: RoleData{
				Role: "polecat", RigName: "myrig", Polecat: "Sparky",
				TownRoot: customRoot, TownName: "my-instance",
				WorkDir: customRoot + "/myrig/polecats/Sparky", DefaultBranch: "main",
				MayorSession: "gt-my-instance-mayor",
			},
		},
		{
			role: "crew",
			data: RoleData{
				Role: "crew", RigName: "myrig", Polecat: "Sparky",
				TownRoot: customRoot, TownName: "my-instance",
				WorkDir: customRoot + "/myrig/crew/Sparky", DefaultBranch: "main",
				MayorSession: "gt-my-instance-mayor",
			},
		},
	}

	for _, tc := range roles {
		t.Run(tc.role, func(t *testing.T) {
			output, err := tmpl.RenderRole(tc.role, tc.data)
			if err != nil {
				t.Fatalf("RenderRole(%q) error = %v", tc.role, err)
			}
			if !strings.Contains(output, customRoot) {
				t.Errorf("rendered %q template does not contain TownRoot %q — paths may be hardcoded", tc.role, customRoot)
			}
		})
	}
}

// TestRenderRole_Polecat_CwdInstruction verifies the critical cwd instruction
// uses the actual town root, not a hardcoded ~/gt path.
// Regression test: agents were following hardcoded ~/gt even in test instances.
func TestRenderRole_Polecat_CwdInstruction(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	const customRoot = "/srv/gastown-ci"

	data := RoleData{
		Role: "polecat", RigName: "rig1", Polecat: "Worker",
		TownRoot: customRoot, TownName: "gastown-ci",
		WorkDir: customRoot + "/rig1/polecats/Worker", DefaultBranch: "main",
		MayorSession: "gt-gastown-ci-mayor",
	}

	output, err := tmpl.RenderRole("polecat", data)
	if err != nil {
		t.Fatalf("RenderRole() error = %v", err)
	}

	wantCwd := customRoot + "/rig1/polecats/Worker/"
	if !strings.Contains(output, wantCwd) {
		t.Errorf("cwd instruction missing %q\n(agent would use wrong path for non-default instance)", wantCwd)
	}

	wantNeverEdit := customRoot + "/rig1/"
	if !strings.Contains(output, wantNeverEdit) {
		t.Errorf("NEVER edit instruction missing %q", wantNeverEdit)
	}
}

// TestRenderLaunchdPlist_EnvironmentVariables verifies that extra env vars
// (e.g. from settings/daemon.env) are rendered into the plist's
// EnvironmentVariables dict alongside GT_TOWN_ROOT.
func TestRenderLaunchdPlist_EnvironmentVariables(t *testing.T) {
	t.Parallel()
	data := SupervisorData{
		GTPath:   "/usr/local/bin/gt",
		TownRoot: "/test/town",
		Env: map[string]string{
			"CMUX_CLAUDE_HOOKS_DISABLED": "1",
			"SDKROOT":                    "/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk",
			"DEVELOPER_DIR":              "/Library/Developer/CommandLineTools",
		},
	}

	output, err := renderLaunchdPlist(data)
	if err != nil {
		t.Fatalf("renderLaunchdPlist() error = %v", err)
	}

	if !strings.Contains(output, "<key>GT_TOWN_ROOT</key>") {
		t.Error("plist missing GT_TOWN_ROOT key")
	}
	for key, value := range data.Env {
		if !strings.Contains(output, "<key>"+key+"</key>") {
			t.Errorf("plist missing env key %q:\n%s", key, output)
		}
		if !strings.Contains(output, "<string>"+value+"</string>") {
			t.Errorf("plist missing env value %q for key %q:\n%s", value, key, output)
		}
	}
}

// TestRenderSystemdUnit_EnvironmentVariables verifies that extra env vars
// are rendered into the systemd unit as Environment= directives alongside
// GT_TOWN_ROOT.
func TestRenderSystemdUnit_EnvironmentVariables(t *testing.T) {
	t.Parallel()
	data := SupervisorData{
		GTPath:   "/usr/local/bin/gt",
		TownRoot: "/test/town",
		Env: map[string]string{
			"CMUX_CLAUDE_HOOKS_DISABLED": "1",
			"SDKROOT":                    "/Library/Developer/CommandLineTools/SDKs/MacOSX.sdk",
			"DEVELOPER_DIR":              "/Library/Developer/CommandLineTools",
		},
	}

	output, err := renderSystemdUnit(data)
	if err != nil {
		t.Fatalf("renderSystemdUnit() error = %v", err)
	}

	if !strings.Contains(output, `Environment="GT_TOWN_ROOT=/test/town"`) {
		t.Error("unit missing GT_TOWN_ROOT environment directive")
	}
	for key, value := range data.Env {
		want := `Environment="` + key + "=" + value + `"`
		if !strings.Contains(output, want) {
			t.Errorf("unit missing %q:\n%s", want, output)
		}
	}
}

// TestRenderLaunchdPlist_NoExtraEnv verifies the plist still renders cleanly
// with a nil Env map (the common case before settings/daemon.env exists).
func TestRenderLaunchdPlist_NoExtraEnv(t *testing.T) {
	t.Parallel()
	output, err := renderLaunchdPlist(SupervisorData{
		GTPath:   "/usr/local/bin/gt",
		TownRoot: "/test/town",
	})
	if err != nil {
		t.Fatalf("renderLaunchdPlist() error = %v", err)
	}
	if !strings.Contains(output, "<key>GT_TOWN_ROOT</key>") {
		t.Error("plist missing GT_TOWN_ROOT key")
	}
}

// TestRenderLaunchdPlist_StandardProcessType guards gt-2ycne. launchd's
// ProcessType=Background puts the daemon and every child it spawns (landing
// gates, post-land gates, plugin runs) in darwinbg: efficiency cores only, CPU
// and I/O throttled whenever the host is busy. A landing gate that takes ~100s
// from a shell took 8-15 minutes under the daemon (priority 4, linkers idle at
// 0% CPU for 30-50s). The daemon does foreground work for the whole town, so
// it must run as a Standard job.
func TestRenderLaunchdPlist_StandardProcessType(t *testing.T) {
	t.Parallel()
	output, err := renderLaunchdPlist(SupervisorData{
		GTPath:   "/usr/local/bin/gt",
		TownRoot: "/test/town",
	})
	if err != nil {
		t.Fatalf("renderLaunchdPlist() error = %v", err)
	}
	if strings.Contains(output, "<string>Background</string>") {
		t.Errorf("plist runs the daemon as a Background process; its landing gates inherit darwinbg throttling (gt-2ycne):\n%s", output)
	}
	if !strings.Contains(output, "<key>ProcessType</key>\n    <string>Standard</string>") {
		t.Errorf("plist must set ProcessType Standard explicitly (gt-2ycne):\n%s", output)
	}
}

// TestRenderLaunchdPlist_ExitTimeOut verifies ExitTimeOutSeconds renders as
// launchd's ExitTimeOut key, and that a zero value (the default before a
// caller opts in) omits the key entirely rather than writing <integer>0</integer>,
// which would make launchd SIGKILL the daemon almost immediately on restart.
func TestRenderLaunchdPlist_ExitTimeOut(t *testing.T) {
	t.Parallel()
	output, err := renderLaunchdPlist(SupervisorData{
		GTPath:             "/usr/local/bin/gt",
		TownRoot:           "/test/town",
		ExitTimeOutSeconds: 85,
	})
	if err != nil {
		t.Fatalf("renderLaunchdPlist() error = %v", err)
	}
	if !strings.Contains(output, "<key>ExitTimeOut</key>") || !strings.Contains(output, "<integer>85</integer>") {
		t.Errorf("plist missing ExitTimeOut=85:\n%s", output)
	}

	output, err = renderLaunchdPlist(SupervisorData{GTPath: "/usr/local/bin/gt", TownRoot: "/test/town"})
	if err != nil {
		t.Fatalf("renderLaunchdPlist() error = %v", err)
	}
	if strings.Contains(output, "<key>ExitTimeOut</key>") {
		t.Errorf("plist set ExitTimeOut for a zero ExitTimeOutSeconds, leaving launchd's default in effect requires omitting the key:\n%s", output)
	}
}

// writeInstalledPlist writes body where the launchd plist for town would be
// installed and returns its path. SupervisorFileRepair is pointed at the path
// directly, the way the caller resolves it, so the test does not need HOME.
func writeInstalledPlist(t *testing.T, town, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "com.gastown.daemon.plist")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// renderedPlist is what this binary renders for town, the shape an install
// writes. exitTimeout is the budget it is rendered with.
func renderedPlist(t *testing.T, town string, exitTimeout time.Duration) string {
	t.Helper()
	content, ok, err := SupervisorFileContent("launchd", town, exitTimeout)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(launchd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	return content
}

// TestSupervisorFileRepair_AddsAMissingExitTimeOut is the bug gt-x872 names: a
// plist installed before ExitTimeOut existed carries no key, and every restart
// through launchd then SIGKILLs the daemon at the 20s default, part-way
// through a graceful shutdown that is allowed longer than that. The file is
// this binary's rendering of this town in every other respect, so the repair
// is to rewrite it with the key.
func TestSupervisorFileRepair_AddsAMissingExitTimeOut(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeInstalledPlist(t, town, renderedPlist(t, town, 0))

	content, repair, err := SupervisorFileRepair(path, "launchd", town, 55*time.Second)
	if err != nil {
		t.Fatalf("SupervisorFileRepair() error = %v", err)
	}
	if !repair {
		t.Fatal("SupervisorFileRepair() did not repair a plist with no ExitTimeOut")
	}
	if !strings.Contains(content, "<key>ExitTimeOut</key>") || !strings.Contains(content, "<integer>55</integer>") {
		t.Errorf("repair does not carry ExitTimeOut=55:\n%s", content)
	}
	if content != renderedPlist(t, town, 55*time.Second) {
		t.Errorf("repair is not the current rendering (changed something other than ExitTimeOut):\n%s", content)
	}
}

// A plist that has the key but at an older budget is the same repair: the
// value is compiled into the binary, so it follows the binary, not the file.
func TestSupervisorFileRepair_UpdatesAChangedExitTimeOut(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeInstalledPlist(t, town, renderedPlist(t, town, 20*time.Second))

	content, repair, err := SupervisorFileRepair(path, "launchd", town, 55*time.Second)
	if err != nil {
		t.Fatalf("SupervisorFileRepair() error = %v", err)
	}
	if !repair {
		t.Fatal("SupervisorFileRepair() did not repair an out-of-date ExitTimeOut")
	}
	if !strings.Contains(content, "<integer>55</integer>") {
		t.Errorf("repair does not carry the current ExitTimeOut:\n%s", content)
	}
}

// TestSupervisorFileRepair_ReplacesABackgroundProcessType is gt-2ycne: every
// plist installed before the fix says ProcessType Background, which throttles
// the daemon and every landing gate it runs. The value comes from the template,
// so it follows the binary like ExitTimeOut does, and an installed file that
// differs only there is repaired to Standard.
func TestSupervisorFileRepair_ReplacesABackgroundProcessType(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	current := renderedPlist(t, town, 55*time.Second)
	old := strings.Replace(current, "<string>Standard</string>", "<string>Background</string>", 1)
	if old == current {
		t.Fatal("test setup: rendered plist has no Standard ProcessType to replace")
	}
	path := writeInstalledPlist(t, town, old)

	content, repair, err := SupervisorFileRepair(path, "launchd", town, 55*time.Second)
	if err != nil {
		t.Fatalf("SupervisorFileRepair() error = %v", err)
	}
	if !repair {
		t.Fatal("SupervisorFileRepair() did not repair a plist whose ProcessType is Background")
	}
	if content != current {
		t.Errorf("repair is not the current rendering:\n%s", content)
	}
}

// A file this binary would write unchanged needs no repair, and the caller
// must be able to tell that from a repair it declined to make.
func TestSupervisorFileRepair_CurrentFileIsNoRepair(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeInstalledPlist(t, town, renderedPlist(t, town, 55*time.Second))

	content, repair, err := SupervisorFileRepair(path, "launchd", town, 55*time.Second)
	if err != nil {
		t.Fatalf("SupervisorFileRepair() error = %v", err)
	}
	if repair || content != "" {
		t.Errorf("SupervisorFileRepair() = (%q, %v) for a current file, want (\"\", false)", content, repair)
	}
}

// The repair is confined to the one value that belongs to the binary. A file
// that names a different gt is somebody's deliberate configuration — repointing
// a launchd job at whichever gt happens to be running is a reconfiguration, and
// not this function's to make.
func TestSupervisorFileRepair_LeavesAFileThatDiffersInMore(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	other, err := renderLaunchdPlist(SupervisorData{
		GTPath:             "/opt/other/bin/gt",
		TownRoot:           town,
		ExitTimeOutSeconds: 0,
	})
	if err != nil {
		t.Fatalf("renderLaunchdPlist() error = %v", err)
	}
	path := writeInstalledPlist(t, town, other)

	content, repair, err := SupervisorFileRepair(path, "launchd", town, 55*time.Second)
	if err != nil {
		t.Fatalf("SupervisorFileRepair() error = %v", err)
	}
	if repair || content != "" {
		t.Errorf("SupervisorFileRepair() = (%q, %v), want (\"\", false): the file names another gt", content, repair)
	}
}

// A plist serving another town is not this town's to rewrite.
func TestSupervisorFileRepair_AnotherTownsFileIsNotTouched(t *testing.T) {
	t.Parallel()
	other := t.TempDir()
	path := writeInstalledPlist(t, other, renderedPlist(t, other, 0))

	content, repair, err := SupervisorFileRepair(path, "launchd", t.TempDir(), 55*time.Second)
	if err != nil {
		t.Fatalf("SupervisorFileRepair() error = %v", err)
	}
	if repair || content != "" {
		t.Errorf("SupervisorFileRepair() = (%q, %v), want (\"\", false): the file serves another town", content, repair)
	}
}

// The systemd unit renders nothing from the binary's own constants, so a
// binary upgrade leaves nothing in it to repair.
func TestSupervisorFileRepair_SystemdIsNeverRepaired(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	unit, ok, err := SupervisorFileContent("systemd", town, 0)
	if err != nil || !ok {
		t.Fatalf("SupervisorFileContent(systemd, %q) = (ok=%v, err=%v)", town, ok, err)
	}
	path := filepath.Join(t.TempDir(), "gastown-daemon.service")
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil {
		t.Fatal(err)
	}

	content, repair, err := SupervisorFileRepair(path, "systemd", town, 55*time.Second)
	if err != nil {
		t.Fatalf("SupervisorFileRepair() error = %v", err)
	}
	if repair || content != "" {
		t.Errorf("SupervisorFileRepair() = (%q, %v) for a systemd unit, want (\"\", false)", content, repair)
	}
}

// SupervisorFileContent renders nothing for a kind this build does not write,
// including the empty kind a host with no supported supervisor reports —
// rather than erroring on it, since that is an ordinary host.
func TestSupervisorFileContent_UnknownKind(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"", "upstart", "launchd "} {
		content, ok, err := SupervisorFileContent(kind, t.TempDir(), time.Second)
		if err != nil {
			t.Errorf("SupervisorFileContent(%q) error = %v", kind, err)
		}
		if ok || content != "" {
			t.Errorf("SupervisorFileContent(%q) = (%q, %v), want (\"\", false)", kind, content, ok)
		}
	}
}

// TestSupervisorStatus reports the kind of the supervisor file this host
// would use when it is installed, and "none" when it is not.
func TestSupervisorStatus(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		goos, want string
		pathSuffix string
	}{
		{"darwin", "launchd", filepath.Join("Library", "LaunchAgents", "com.gastown.daemon.plist")},
		{"linux", "systemd", filepath.Join("data", "systemd", "user", "gastown-daemon.service")},
	} {
		t.Run(tt.goos, func(t *testing.T) {
			t.Parallel()
			h := fakeHost(t, tt.goos)
			if got := h.status(); got != "none" {
				t.Errorf("status() with no file = %q, want none", got)
			}
			path, _ := h.filePath()
			if !strings.HasSuffix(path, tt.pathSuffix) {
				t.Errorf("filePath() = %q, want it to end in %q", path, tt.pathSuffix)
			}
			writeFileAt(t, path, "<plist/>")
			if got := h.status(); got != tt.want {
				t.Errorf("status() = %q, want %q", got, tt.want)
			}
		})
	}
}

// Without XDG_DATA_HOME the systemd unit lives under ~/.local/share.
func TestSystemdUnitPath_DefaultsUnderHome(t *testing.T) {
	t.Parallel()
	h := fakeHost(t, "linux")
	h.getenv = func(string) string { return "" }
	home, _ := h.homeDir()
	got, err := h.systemdUnitPath()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(home, ".local", "share", "systemd", "user", "gastown-daemon.service"); got != want {
		t.Errorf("systemdUnitPath() = %q, want %q", got, want)
	}
}

func TestRoleNames(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	names := tmpl.RoleNames()
	expected := []string{"polecat", "crew"}

	if len(names) != len(expected) {
		t.Errorf("RoleNames() = %v, want %v", names, expected)
	}

	for i, name := range names {
		if name != expected[i] {
			t.Errorf("RoleNames()[%d] = %q, want %q", i, name, expected[i])
		}
	}
}

func TestCreatePolecatCLAUDEmd(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	created, err := CreatePolecatCLAUDEmd(dir, "greenplace", "furiosa")
	if err != nil {
		t.Fatalf("CreatePolecatCLAUDEmd() error = %v", err)
	}
	if !created {
		t.Fatal("CreatePolecatCLAUDEmd() created = false, want true")
	}

	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	content := string(data)

	// Verify placeholders were replaced
	if strings.Contains(content, "{{rig}}") {
		t.Error("CLAUDE.md still contains {{rig}} placeholder")
	}
	if strings.Contains(content, "{{name}}") {
		t.Error("CLAUDE.md still contains {{name}} placeholder")
	}

	// Verify substituted values are present
	if !strings.Contains(content, "greenplace") {
		t.Error("CLAUDE.md does not contain rig name 'greenplace'")
	}
	if !strings.Contains(content, "furiosa") {
		t.Error("CLAUDE.md does not contain polecat name 'furiosa'")
	}

	// Verify critical gt done instructions are present
	if !strings.Contains(content, "gt done") {
		t.Fatal("CLAUDE.md does not contain 'gt done' — polecats will not know to call it")
	}
	if !strings.Contains(content, "IDLE POLECAT HERESY") {
		t.Error("CLAUDE.md missing 'IDLE POLECAT HERESY' warning section")
	}
	if !strings.Contains(content, "MANDATORY FINAL STEP") {
		t.Error("CLAUDE.md missing completion protocol with MANDATORY FINAL STEP")
	}
}

func TestCreatePolecatCLAUDEmd_WritesToLocalWhenTrackedExists(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// Write a CLAUDE.md with the exact town-root template content that gets
	// tracked in repos. This is the real-world scenario: gt install creates
	// ~/gt/CLAUDE.md with Dolt operational awareness, the user commits it to
	// their repo, and git worktree add checks it out in the polecat worktree.
	existing := TownRootCLAUDEmd()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte(existing), 0644); err != nil {
		t.Fatalf("writing existing CLAUDE.md: %v", err)
	}

	created, err := CreatePolecatCLAUDEmd(dir, "greenplace", "furiosa")
	if err != nil {
		t.Fatalf("CreatePolecatCLAUDEmd() error = %v", err)
	}
	if !created {
		t.Fatal("CreatePolecatCLAUDEmd() created = false, want true (should write to CLAUDE.local.md)")
	}

	// CLAUDE.md must NOT be modified — it's a tracked file and modifying it
	// creates uncommitted changes that the gt done safety net would commit onto
	// the polecat's branch, polluting the PR diff.
	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("reading CLAUDE.md: %v", err)
	}
	if string(data) != existing {
		t.Error("CLAUDE.md was modified — tracked file must not be touched when CLAUDE.local.md is used")
	}
	if strings.Contains(string(data), PolecatLifecycleMarker) {
		t.Error("polecat lifecycle marker written to tracked CLAUDE.md — should go to CLAUDE.local.md")
	}

	// Polecat lifecycle instructions written to CLAUDE.local.md (gitignored)
	localData, err := os.ReadFile(filepath.Join(dir, "CLAUDE.local.md"))
	if err != nil {
		t.Fatalf("reading CLAUDE.local.md: %v", err)
	}
	localContent := string(localData)
	if !strings.Contains(localContent, "IDLE POLECAT HERESY") {
		t.Error("polecat lifecycle instructions not written to CLAUDE.local.md")
	}
	if !strings.Contains(localContent, "gt done") {
		t.Fatal("gt done instructions not in CLAUDE.local.md — polecats will not know to call it")
	}
}

func TestCreatePolecatCLAUDEmd_SkipsWhenAlreadyProvisioned(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// First call — creates the file
	created, err := CreatePolecatCLAUDEmd(dir, "greenplace", "furiosa")
	if err != nil {
		t.Fatalf("first CreatePolecatCLAUDEmd() error = %v", err)
	}
	if !created {
		t.Fatal("first call should create")
	}

	data1, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))

	// Second call — should skip (marker already present)
	created, err = CreatePolecatCLAUDEmd(dir, "greenplace", "furiosa")
	if err != nil {
		t.Fatalf("second CreatePolecatCLAUDEmd() error = %v", err)
	}
	if created {
		t.Fatal("second call should skip (lifecycle instructions already present)")
	}

	data2, _ := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if string(data1) != string(data2) {
		t.Fatal("file was modified on second call — should be idempotent")
	}
}

// TestCreatePolecatCLAUDEmd_ReusePath simulates the polecat reuse scenario:
// 1. Worktree has tracked CLAUDE.md from repo (town-root Dolt content)
// 2. CreatePolecatCLAUDEmd writes lifecycle instructions to CLAUDE.local.md
// 3. git reset --hard restores CLAUDE.md (CLAUDE.local.md unaffected — it's gitignored)
// 4. Second CreatePolecatCLAUDEmd call is a no-op (CLAUDE.local.md still has the marker)
//
// This is better than the old append-to-CLAUDE.md approach because git reset --hard
// no longer loses the lifecycle instructions.
func TestCreatePolecatCLAUDEmd_ReusePath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")
	claudeLocalPath := filepath.Join(dir, "CLAUDE.local.md")

	// Step 1: Simulate tracked CLAUDE.md from repo (town-root content)
	townRoot := TownRootCLAUDEmd()
	if err := os.WriteFile(claudePath, []byte(townRoot), 0644); err != nil {
		t.Fatalf("writing tracked CLAUDE.md: %v", err)
	}

	// Step 2: First provision — writes lifecycle instructions to CLAUDE.local.md
	created, err := CreatePolecatCLAUDEmd(dir, "greenplace", "furiosa")
	if err != nil {
		t.Fatalf("first CreatePolecatCLAUDEmd() error = %v", err)
	}
	if !created {
		t.Fatal("first call should create CLAUDE.local.md")
	}

	// Lifecycle instructions are in CLAUDE.local.md, not CLAUDE.md
	localData, _ := os.ReadFile(claudeLocalPath)
	if !strings.Contains(string(localData), PolecatLifecycleMarker) {
		t.Fatal("lifecycle marker not found in CLAUDE.local.md after first provision")
	}
	claudeData, _ := os.ReadFile(claudePath)
	if strings.Contains(string(claudeData), PolecatLifecycleMarker) {
		t.Fatal("lifecycle marker written to tracked CLAUDE.md — must not modify tracked file")
	}

	// Step 3: Simulate git reset --hard (restores tracked CLAUDE.md, but CLAUDE.local.md
	// is gitignored/untracked so it survives the reset)
	if err := os.WriteFile(claudePath, []byte(townRoot), 0644); err != nil {
		t.Fatalf("simulating git reset --hard: %v", err)
	}

	// CLAUDE.local.md still has the lifecycle marker (survived git reset)
	localData, _ = os.ReadFile(claudeLocalPath)
	if !strings.Contains(string(localData), PolecatLifecycleMarker) {
		t.Fatal("CLAUDE.local.md lifecycle marker lost — should survive git reset --hard")
	}

	// Step 4: Second provision — no-op since CLAUDE.local.md already has the marker
	created, err = CreatePolecatCLAUDEmd(dir, "greenplace", "furiosa")
	if err != nil {
		t.Fatalf("second CreatePolecatCLAUDEmd() error = %v", err)
	}
	if created {
		t.Fatal("second call should be a no-op (lifecycle instructions still in CLAUDE.local.md)")
	}

	// Both CLAUDE.md (unchanged) and CLAUDE.local.md (with lifecycle) should be intact
	claudeData, _ = os.ReadFile(claudePath)
	if !strings.Contains(string(claudeData), "Dolt Server") {
		t.Error("town-root content in CLAUDE.md was lost")
	}
	localData, _ = os.ReadFile(claudeLocalPath)
	if !strings.Contains(string(localData), "gt done") {
		t.Fatal("gt done instructions not found in CLAUDE.local.md")
	}
}

// TestCreatePolecatCLAUDEmd_GitCleanRemovesLocal simulates git clean -f removing
// the untracked CLAUDE.local.md. On re-provision, the function must recreate it.
func TestCreatePolecatCLAUDEmd_GitCleanRemovesLocal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")
	claudeLocalPath := filepath.Join(dir, "CLAUDE.local.md")

	// Tracked CLAUDE.md exists
	townRoot := TownRootCLAUDEmd()
	if err := os.WriteFile(claudePath, []byte(townRoot), 0644); err != nil {
		t.Fatalf("writing tracked CLAUDE.md: %v", err)
	}

	// First provision: writes to CLAUDE.local.md
	if _, err := CreatePolecatCLAUDEmd(dir, "greenplace", "nux"); err != nil {
		t.Fatalf("first provision: %v", err)
	}

	// Simulate git clean -f removing the untracked CLAUDE.local.md
	if err := os.Remove(claudeLocalPath); err != nil {
		t.Fatalf("simulating git clean -f: %v", err)
	}

	// Second provision: CLAUDE.local.md is gone, must recreate it
	created, err := CreatePolecatCLAUDEmd(dir, "greenplace", "nux")
	if err != nil {
		t.Fatalf("second provision: %v", err)
	}
	if !created {
		t.Fatal("should recreate CLAUDE.local.md after git clean removed it")
	}

	localData, _ := os.ReadFile(claudeLocalPath)
	if !strings.Contains(string(localData), PolecatLifecycleMarker) {
		t.Fatal("lifecycle marker not in recreated CLAUDE.local.md")
	}
	// CLAUDE.md must still be unmodified
	claudeData, _ := os.ReadFile(claudePath)
	if string(claudeData) != townRoot {
		t.Error("tracked CLAUDE.md was modified")
	}
}

// TestCreatePolecatCLAUDEmd_GitCleanScenario simulates git clean -f removing
// an untracked CLAUDE.md (repo without tracked CLAUDE.md), then re-provisioning.
func TestCreatePolecatCLAUDEmd_GitCleanScenario(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	claudePath := filepath.Join(dir, "CLAUDE.md")

	// Step 1: First provision — creates fresh file
	created, err := CreatePolecatCLAUDEmd(dir, "greenplace", "nux")
	if err != nil {
		t.Fatalf("first CreatePolecatCLAUDEmd() error = %v", err)
	}
	if !created {
		t.Fatal("first call should create file")
	}

	// Step 2: Simulate git clean -f (removes untracked files)
	os.Remove(claudePath)
	if _, err := os.Stat(claudePath); !os.IsNotExist(err) {
		t.Fatal("git clean simulation should have removed CLAUDE.md")
	}

	// Step 3: Re-provision after clean
	created, err = CreatePolecatCLAUDEmd(dir, "greenplace", "nux")
	if err != nil {
		t.Fatalf("second CreatePolecatCLAUDEmd() error = %v", err)
	}
	if !created {
		t.Fatal("second call should re-create file after git clean")
	}

	data, _ := os.ReadFile(claudePath)
	if !strings.Contains(string(data), "gt done") {
		t.Fatal("gt done instructions not found after re-creation")
	}
}

func TestPolecatCLAUDEmd_PointsAtWritingForAgents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := CreatePolecatCLAUDEmd(dir, "gastown", "agate"); err != nil {
		t.Fatalf("CreatePolecatCLAUDEmd: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "CLAUDE.md"))
	if err != nil {
		t.Fatalf("read rendered CLAUDE.md: %v", err)
	}
	want := "if the repo has docs/writing-for-agents.md, read it"
	if !strings.Contains(string(data), want) {
		t.Fatalf("rendered CLAUDE.md lacks the writing-for-agents pointer %q", want)
	}
}

// TestPolecatGuidanceForbidsSlotPollingLoops covers gt-7dxw: a polecat hit the
// test-verify slot cap inside `gt done` and improvised a polling loop around
// it. The instruction that forbids the loop reaches the polecat through the
// prime output — asserted on the RENDERED text rather than the template
// source, because what matters is what the agent reads. The three other homes
// for the same wording are covered where they live: the formulas in
// internal/formula, the /done body in internal/templates/commands.
//
// The provisioned polecat CLAUDE.md deliberately does NOT carry this text:
// docs-lint holds that file to a 2,000-word ceiling and it sits at 1,983, so a
// second copy would break the gate. The rule lives in the prime output, the
// mol-polecat-work formula, and the /done body; the dangerous-command guard is
// what enforces it.
func TestPolecatGuidanceForbidsSlotPollingLoops(t *testing.T) {
	t.Parallel()
	// The calm-wait sentence, character-for-character: a polecat that reads
	// the wait as a hang closes its bead mid-`gt done` (overseer hq-wisp-6q5ib).
	const calmWait = "`gt done` runs the local gate itself (lint, build and the tests of the packages your branch changed; " +
		"no container slot), which can take several minutes. That is normal. Do not interrupt it " +
		"and do not close the bead."

	rendered := renderPolecatForTest(t)
	if !strings.Contains(rendered, calmWait) {
		t.Errorf("rendered polecat prime output lacks the gt done wait sentence")
	}

	for _, want := range []string{
		"Never script a retry around `gt done`",
		"`gt escalate -s medium`",
		"No flag skips the gate.",
		"Do not run container suites yourself. Run the non-container packages, then",
		// The explanation lives once, in the home the prime points at (R2).
		"read the container-gate rule in `docs/reference.md`",
	} {
		if !strings.Contains(rendered, want) {
			t.Errorf("rendered polecat prime output lacks %q", want)
		}
	}
}

// renderPolecatForTest renders the role template the way `gt prime` does, with
// a non-fork rig (the only branch that runs `gt done`'s landing workflow).
func renderPolecatForTest(t *testing.T) string {
	t.Helper()
	tmpl, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	output, err := tmpl.RenderRole("polecat", RoleData{
		Role: "polecat", RigName: "myrig", Polecat: "TestCat",
		TownRoot: "/test/town", TownName: "town",
		WorkDir:      "/test/town/myrig/polecats/TestCat",
		MayorSession: "gt-town-mayor",
	})
	if err != nil {
		t.Fatalf("RenderRole() error = %v", err)
	}
	return output
}

// TestRoleTemplatesCarryInterruptPolicy guards claude-9a8: the patrol and
// work-loop roles must be told that a bare "[Request interrupted by user for
// tool use]" is a nudge-delivery artifact, not an operator stop. Crew is left
// out: a human may really be at those panes.
func TestRoleTemplatesCarryInterruptPolicy(t *testing.T) {
	t.Parallel()
	tmpl, err := New()
	if err != nil {
		t.Fatal(err)
	}
	const marker = "An interrupt is a delivery artifact"
	for role, want := range map[string]bool{
		"polecat": true,
		"crew":    false,
	} {
		data := RoleData{Role: role, RigName: "gastown", TownRoot: "/t", TownName: "t", Polecat: "p", DefaultBranch: "main"}
		out, err := tmpl.RenderRole(role, data)
		if err != nil {
			t.Fatalf("RenderRole(%s): %v", role, err)
		}
		if got := strings.Contains(out, marker); got != want {
			t.Errorf("role %s carries interrupt policy = %v, want %v", role, got, want)
		}
	}
}

// A token in settings/daemon.env stays out of the supervisor file: spawn
// reads it from daemon.env, and the plist is not a secret store (gt-y3pgh.5).
func TestSupervisorData_LeavesDaemonEnvTokensOut(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Join(town, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "SDKROOT=/fake/sdk\nDS_TOKEN=sk-fake0000000000000000000000000000\n"
	if err := os.WriteFile(filepath.Join(town, "settings", "daemon.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := supervisorData(town, 0)
	if err != nil {
		t.Fatal(err)
	}
	if data.Env["SDKROOT"] != "/fake/sdk" {
		t.Errorf("SDKROOT dropped: %v", data.Env)
	}
	if _, ok := data.Env["DS_TOKEN"]; ok {
		t.Error("supervisor env carries the daemon.env token")
	}
}
