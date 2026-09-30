package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
)

func TestBuildWitnessPatrolVars_NilContext(t *testing.T) {
	t.Parallel()
	ctx := RoleContext{}
	vars := buildWitnessPatrolVars(ctx)
	if len(vars) != 0 {
		t.Errorf("expected empty vars for nil context, got %v", vars)
	}
}

func TestBuildWitnessPatrolVars_InjectsRigAndPrefix(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigDir := filepath.Join(tmpDir, "testrig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}

	ctx := RoleContext{
		TownRoot: tmpDir,
		Rig:      "testrig",
	}
	vars := buildWitnessPatrolVars(ctx)
	if len(vars) != 2 {
		t.Fatalf("expected 2 vars (rig, prefix), got %v", vars)
	}
	varMap := make(map[string]string)
	for _, v := range vars {
		parts := splitFirstEquals(v)
		if len(parts) == 2 {
			varMap[parts[0]] = parts[1]
		}
	}
	if got := varMap["rig"]; got != "testrig" {
		t.Errorf("rig = %q, want %q", got, "testrig")
	}
	if got := varMap["prefix"]; got != "gt" {
		t.Errorf("prefix = %q, want %q (default fallback)", got, "gt")
	}
}

// splitFirstEquals splits a string on the first '=' only.
func splitFirstEquals(s string) []string {
	idx := -1
	for i, c := range s {
		if c == '=' {
			idx = i
			break
		}
	}
	if idx < 0 {
		return []string{s}
	}
	return []string{s[:idx], s[idx+1:]}
}

func TestPatrolRigName(t *testing.T) {
	t.Parallel()
	if got := patrolRigName(PatrolConfig{Assignee: "gastown/refinery"}); got != "gastown" {
		t.Fatalf("patrolRigName = %q, want gastown", got)
	}
	if got := patrolRigName(PatrolConfig{Assignee: "deacon"}); got != "" {
		t.Fatalf("patrolRigName without rig = %q, want empty", got)
	}
}

func TestRenderPatrolWispDescription_DeaconInlinesStepsAndVars(t *testing.T) {
	t.Parallel()
	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-deacon-patrol",
		BeadsDir:      t.TempDir(),
		Assignee:      "deacon",
		ExtraVars:     []string{"idle_effort_threshold=7"},
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	for _, want := range []string{
		"Mayor's daemon patrol loop.",
		"**Formula Checklist**",
		"gt deacon heartbeat \"starting patrol cycle\"",
		"idle_cycles >= 7",
	} {
		if !strings.Contains(desc, want) {
			t.Fatalf("description missing %q:\n%s", want, desc)
		}
	}
}

func TestRenderPatrolWispDescription_AppliesOverlay(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	overlayDir := filepath.Join(townRoot, "formula-overlays")
	if err := os.MkdirAll(overlayDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(overlayDir, "mol-deacon-patrol.toml"), []byte(`[[step-overrides]]
step_id = "heartbeat"
mode = "append"
description = "overlay heartbeat note"
`), 0644); err != nil {
		t.Fatal(err)
	}

	desc, err := renderPatrolWispDescription(PatrolConfig{
		PatrolMolName: "mol-deacon-patrol",
		BeadsDir:      townRoot,
		Assignee:      "deacon",
	})
	if err != nil {
		t.Fatalf("renderPatrolWispDescription: %v", err)
	}
	if !strings.Contains(desc, "overlay heartbeat note") {
		t.Fatalf("description did not include overlay text:\n%s", desc)
	}
}

func TestUpdatePatrolWispDescriptionUsesBodyFileStdin(t *testing.T) {
	binDir := t.TempDir()
	logFile := filepath.Join(t.TempDir(), "bd.log")
	writeBDStub(t, binDir, `#!/usr/bin/env sh
{
  printf 'args:%s\n' "$*"
  printf 'stdin:'
  cat
  printf '\n'
} >> "$BD_STUB_LOG"
`, `@echo off
echo args:%* >> %BD_STUB_LOG%
set /p stdin=
echo stdin:%stdin% >> %BD_STUB_LOG%
`)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("BD_STUB_LOG", logFile)

	err := updatePatrolWispDescription(PatrolConfig{BeadsDir: t.TempDir()}, filepath.Join(t.TempDir(), ".beads"), "gt-wisp-test", "line one\nline two")
	if err != nil {
		t.Fatalf("updatePatrolWispDescription: %v", err)
	}
	data, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	log := string(data)
	if !strings.Contains(log, "args:update gt-wisp-test --body-file=-") {
		t.Fatalf("expected body-file update args, got:\n%s", log)
	}
	if strings.Contains(log, "--description=") {
		t.Fatalf("description must not be passed through argv:\n%s", log)
	}
	if !strings.Contains(log, "stdin:line one\nline two") {
		t.Fatalf("expected multiline description on stdin, got:\n%s", log)
	}
}
