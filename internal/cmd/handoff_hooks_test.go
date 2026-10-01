package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/runtime"
)

// A handoff or mol-step respawn syncs the successor's managed settings the
// way its first start did, and reports hooks:present|absent for it
// (gt-4k3fj.8.4).
func TestBuildRestartPlan_SyncsCrewSettings(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	rigPath := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(rigPath, "crew", "holden"), 0755); err != nil {
		t.Fatal(err)
	}

	plan, err := buildRestartPlan("gt-crew-holden", restartOpts(townRoot, map[string]string{}))
	if err != nil {
		t.Fatalf("buildRestartPlan: %v", err)
	}
	h := plan.Hooks
	if h == nil {
		t.Fatal("crew restart plan has no settings sync")
	}
	type fields struct{ TownRoot, Actor, Session, Role, SettingsDir, WorkDir string }
	want := fields{
		TownRoot:    townRoot,
		Actor:       "gastown/crew/holden",
		Session:     "gt-crew-holden",
		Role:        "crew",
		SettingsDir: filepath.Join(rigPath, "crew"),
		WorkDir:     townRoot + "/gastown/crew/holden",
	}
	got := fields{TownRoot: h.TownRoot, Actor: h.Actor, Session: h.Session, Role: h.Role, SettingsDir: h.SettingsDir, WorkDir: h.WorkDir}
	if got != want {
		t.Errorf("plan hooks = %+v\nwant %+v", got, want)
	}

	var synced []string
	var reported runtime.HooksStatus
	var reportedTo [3]string
	h.sync = func(settingsDir, workDir, role string) (runtime.HooksStatus, error) {
		synced = []string{settingsDir, workDir, role}
		return runtime.HooksStatus{Role: role, Reason: "fake"}, nil
	}
	h.report = func(townRoot, actor, session string, s runtime.HooksStatus) {
		reportedTo = [3]string{townRoot, actor, session}
		reported = s
	}
	plan.syncSettings()
	if len(synced) != 3 || synced[0] != want.SettingsDir || synced[1] != want.WorkDir || synced[2] != "crew" {
		t.Errorf("synced %q", synced)
	}
	if reportedTo != [3]string{townRoot, "gastown/crew/holden", "gt-crew-holden"} || reported.Reason != "fake" {
		t.Errorf("reported %+v to %q", reported, reportedTo)
	}
}

// Roles without a shared settings directory (mayor, witness, ...) keep their
// settings in the working directory, as session.StartSession does.
func TestBuildRestartPlan_MayorSettingsInWorkDir(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "mayor"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(townRoot, "mayor", "town.json"), []byte(`{"name":"gastown"}`), 0644); err != nil {
		t.Fatal(err)
	}
	plan, err := buildRestartPlan(getMayorSessionName(), restartOpts(townRoot, map[string]string{}))
	if err != nil {
		t.Fatalf("buildRestartPlan: %v", err)
	}
	if plan.Hooks == nil || plan.Hooks.Role != "mayor" || plan.Hooks.SettingsDir != townRoot+"/mayor" || plan.Hooks.WorkDir != townRoot+"/mayor" {
		t.Errorf("mayor plan hooks = %+v", plan.Hooks)
	}
}
