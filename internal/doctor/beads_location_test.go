package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeBeadsLocationFiles creates dir and writes each file in it.
func writeBeadsLocationFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

// beadsLocationRigFiles are the files of a rig's beads directory with data.
var beadsLocationRigFiles = map[string]string{
	"metadata.json": `{"dolt_database":"testrig"}`,
	"config.yaml":   "prefix: tr\n",
}

// beadsLocationConfigOnlyFiles are a rig's beads files with no metadata.json, so
// beads.ComputeRedirectTarget finds no database and takes its mayor/rig fallback.
var beadsLocationConfigOnlyFiles = map[string]string{
	"config.yaml": "prefix: tr\n",
}

// The rows below cover the four cases each beads-location check must tell
// apart: the beads at <rig>/.beads, the beads at the legacy mayor/rig/.beads,
// a legacy redirect beside data at <rig>/.beads, and a redirect to a missing
// target.

func TestBeadsRedirectCheck_BeadsLocationTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		setup      func(t *testing.T, rigPath string)
		wantStatus CheckStatus
		// afterFix asserts the rig after Fix ran.
		afterFix func(t *testing.T, rigPath string)
	}{
		{
			name: "new location only passes with no redirect",
			setup: func(t *testing.T, rigPath string) {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
			},
			wantStatus: StatusOK,
			afterFix: func(t *testing.T, rigPath string) {
				if _, err := os.Stat(filepath.Join(rigPath, ".beads", "redirect")); err == nil {
					t.Error("Fix wrote a redirect for a rig whose files are at .beads/")
				}
			},
		},
		{
			name: "legacy only without a redirect is an error, and Fix points at the legacy copy",
			setup: func(t *testing.T, rigPath string) {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
			},
			wantStatus: StatusError,
			afterFix: func(t *testing.T, rigPath string) {
				content, err := os.ReadFile(filepath.Join(rigPath, ".beads", "redirect"))
				if err != nil {
					t.Fatalf("redirect after Fix: %v", err)
				}
				if string(content) != "mayor/rig/.beads\n" {
					t.Errorf("redirect after Fix = %q, want the legacy redirect for a legacy-only rig", string(content))
				}
			},
		},
		{
			name: "legacy redirect with data at the new location warns and is not rewritten",
			setup: func(t *testing.T, rigPath string) {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
				if err := os.WriteFile(filepath.Join(rigPath, ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
					t.Fatal(err)
				}
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
			},
			wantStatus: StatusWarning,
			afterFix: func(t *testing.T, rigPath string) {
				content, err := os.ReadFile(filepath.Join(rigPath, ".beads", "redirect"))
				if err != nil {
					t.Fatalf("redirect after Fix: %v", err)
				}
				if string(content) != "mayor/rig/.beads\n" {
					t.Errorf("redirect after Fix = %q, want it left as it was", string(content))
				}
				if _, err := os.Stat(filepath.Join(rigPath, ".beads", "metadata.json")); err != nil {
					t.Errorf("metadata.json after Fix: %v, want it kept", err)
				}
			},
		},
		{
			name: "redirect to a missing legacy target is an error and Fix creates nothing",
			setup: func(t *testing.T, rigPath string) {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), map[string]string{
					"redirect": "mayor/rig/.beads\n",
				})
			},
			wantStatus: StatusError,
			afterFix: func(t *testing.T, rigPath string) {
				if _, err := os.Stat(filepath.Join(rigPath, "mayor", "rig", ".beads")); err == nil {
					t.Error("Fix created the missing legacy target")
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			rigName := "testrig"
			rigPath := filepath.Join(tmpDir, rigName)
			tc.setup(t, rigPath)

			check := NewBeadsRedirectCheck()
			ctx := &CheckContext{TownRoot: tmpDir, RigName: rigName}
			if got := check.Run(ctx); got.Status != tc.wantStatus {
				t.Fatalf("Run status = %v (%s), want %v", got.Status, got.Message, tc.wantStatus)
			}
			if err := check.Fix(ctx); err != nil {
				t.Fatalf("Fix failed: %v", err)
			}
			if tc.afterFix != nil {
				tc.afterFix(t, rigPath)
			}
		})
	}
}

func TestBeadsRedirectTargetCheck_BeadsLocationTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup writes the rig's beads and returns the crew worktree's redirect target.
		setup      func(t *testing.T, rigPath string) string
		wantStatus CheckStatus
		wantReason string
	}{
		{
			name: "new location only passes",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
				return "../../.beads"
			},
			wantStatus: StatusOK,
		},
		{
			name: "legacy only warns that the move is not done",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
				return "../../mayor/rig/.beads"
			},
			wantStatus: StatusWarning,
			wantReason: legacyRedirectReason,
		},
		{
			name: "legacy redirect with data at the new location warns",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
				return "../../mayor/rig/.beads"
			},
			wantStatus: StatusWarning,
			wantReason: legacyRedirectReason,
		},
		{
			name: "redirect to a missing target is reported as a missing target, not a legacy warning",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
				return "../../mayor/rig/.beads"
			},
			wantStatus: StatusWarning,
			wantReason: "target does not exist",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			rigPath := filepath.Join(tmpDir, "testrig")
			target := tc.setup(t, rigPath)

			crewBeads := filepath.Join(rigPath, "crew", "max", ".beads")
			writeBeadsLocationFiles(t, crewBeads, map[string]string{"redirect": target + "\n"})

			check := NewBeadsRedirectTargetCheck()
			ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
			got := check.Run(ctx)
			if got.Status != tc.wantStatus {
				t.Fatalf("Run status = %v (%s), want %v", got.Status, got.Message, tc.wantStatus)
			}
			if tc.wantStatus == StatusOK {
				return
			}
			if !strings.Contains(strings.Join(got.Details, "\n"), tc.wantReason) {
				t.Errorf("details = %v, want a reason containing %q", got.Details, tc.wantReason)
			}
			if tc.wantReason == legacyRedirectReason {
				// A legacy redirect is a warning: Fix leaves the worktree redirect as it was
				if err := check.Fix(ctx); err != nil {
					t.Fatalf("Fix failed: %v", err)
				}
				content, err := os.ReadFile(filepath.Join(crewBeads, "redirect"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.TrimSpace(string(content)) != target {
					t.Errorf("worktree redirect after Fix = %q, want %q", string(content), target)
				}
			}
		})
	}
}

func TestRoutesCheck_BeadsLocationTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup writes the rig's beads and returns the routes.jsonl lines for crom.
		setup      func(t *testing.T, rigPath string) string
		wantStatus CheckStatus
		wantDetail string
		// afterFix asserts routes.jsonl after Fix ran.
		afterFix func(t *testing.T, routesPath string)
	}{
		{
			name: "new location only passes with the rig-root route",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
				return `{"prefix":"cr-","path":"crom"}`
			},
			wantStatus: StatusOK,
		},
		{
			name: "legacy-only route warns and Fix cannot rewrite it without a real rig-root beads dir",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
				return `{"prefix":"cr-","path":"crom/mayor/rig"}`
			},
			wantStatus: StatusWarning,
			wantDetail: "names the legacy per-rig path",
		},
		{
			name: "legacy route with files at the new location is rewritten to the rig root by Fix",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
				return `{"prefix":"cr-","path":"crom/mayor/rig"}`
			},
			wantStatus: StatusWarning,
			wantDetail: "names the legacy per-rig path",
			afterFix: func(t *testing.T, routesPath string) {
				content, err := os.ReadFile(routesPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(content), `"path":"crom"}`) {
					t.Errorf("routes after Fix = %s, want the route moved to the rig root", content)
				}
			},
		},
		{
			name: "legacy route beside data at the new location warns and is not rewritten",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationRigFiles)
				if err := os.WriteFile(filepath.Join(rigPath, ".beads", "redirect"), []byte("mayor/rig/.beads\n"), 0644); err != nil {
					t.Fatal(err)
				}
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
				return `{"prefix":"cr-","path":"crom/mayor/rig"}`
			},
			wantStatus: StatusWarning,
			wantDetail: "names the legacy per-rig path",
			afterFix: func(t *testing.T, routesPath string) {
				content, err := os.ReadFile(routesPath)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(content), `"path":"crom/mayor/rig"`) {
					t.Errorf("routes after Fix = %s, want the legacy route left as it was", content)
				}
			},
		},
		{
			name: "route to a missing legacy target is invalid",
			setup: func(t *testing.T, rigPath string) string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), map[string]string{
					"redirect": "mayor/rig/.beads\n",
				})
				return `{"prefix":"cr-","path":"crom/mayor/rig"}`
			},
			wantStatus: StatusWarning,
			wantDetail: "path does not exist",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			rigPath := filepath.Join(tmpDir, "crom")
			routeLine := tc.setup(t, rigPath)

			if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
				t.Fatal(err)
			}
			rigsJSON := `{"version":1,"rigs":{"crom":{"git_url":"https://example.com/crom","beads":{"prefix":"cr"}}}}`
			if err := os.WriteFile(filepath.Join(tmpDir, "mayor", "rigs.json"), []byte(rigsJSON), 0644); err != nil {
				t.Fatal(err)
			}
			writeBeadsLocationFiles(t, filepath.Join(tmpDir, ".beads"), nil)
			routesPath := filepath.Join(tmpDir, ".beads", "routes.jsonl")
			if err := os.WriteFile(routesPath, []byte(`{"prefix":"hq-","path":"."}`+"\n"+routeLine+"\n"), 0644); err != nil {
				t.Fatal(err)
			}

			check := NewRoutesCheck()
			ctx := &CheckContext{TownRoot: tmpDir}
			got := check.Run(ctx)
			if got.Status != tc.wantStatus {
				t.Fatalf("Run status = %v (%s), want %v", got.Status, got.Message, tc.wantStatus)
			}
			if tc.wantDetail != "" && !strings.Contains(strings.Join(got.Details, "\n"), tc.wantDetail) {
				t.Errorf("details = %v, want one containing %q", got.Details, tc.wantDetail)
			}
			if err := check.Fix(ctx); err != nil {
				t.Fatalf("Fix failed: %v", err)
			}
			if tc.afterFix != nil {
				tc.afterFix(t, routesPath)
			}
		})
	}
}

func TestStaleDoltPortCheck_BeadsLocationPortFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		// setup writes the rig's dolt-server.port files; it returns the paths expected from findPortFiles.
		setup func(t *testing.T, rigPath string) []string
	}{
		{
			name: "new location only is found",
			setup: func(t *testing.T, rigPath string) []string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), map[string]string{"dolt-server.port": "3307"})
				return []string{filepath.Join(rigPath, ".beads", "dolt-server.port")}
			},
		},
		{
			name: "legacy only is found",
			setup: func(t *testing.T, rigPath string) []string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), map[string]string{"dolt-server.port": "3307"})
				return []string{filepath.Join(rigPath, "mayor", "rig", ".beads", "dolt-server.port")}
			},
		},
		{
			name: "both locations are found",
			setup: func(t *testing.T, rigPath string) []string {
				writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), map[string]string{"dolt-server.port": "3307"})
				writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), map[string]string{"dolt-server.port": "3307"})
				return []string{
					filepath.Join(rigPath, ".beads", "dolt-server.port"),
					filepath.Join(rigPath, "mayor", "rig", ".beads", "dolt-server.port"),
				}
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tmpDir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(tmpDir, "mayor"), 0755); err != nil {
				t.Fatal(err)
			}
			rigsJSON := `{"version":1,"rigs":{"testrig":{"git_url":"https://example.com/testrig"}}}`
			if err := os.WriteFile(filepath.Join(tmpDir, "mayor", "rigs.json"), []byte(rigsJSON), 0644); err != nil {
				t.Fatal(err)
			}
			want := tc.setup(t, filepath.Join(tmpDir, "testrig"))

			got := map[string]bool{}
			for _, f := range (&StaleDoltPortCheck{}).findPortFiles(tmpDir) {
				got[f] = true
			}
			for _, w := range want {
				if !got[w] {
					t.Errorf("findPortFiles missing %s; got %v", w, got)
				}
			}
			if len(got) != len(want) {
				t.Errorf("findPortFiles = %v, want exactly %v", got, want)
			}
		})
	}
}

// A half-moved rig has its files at <rig>/.beads and a legacy copy still
// standing. ComputeRedirectTarget names the legacy copy for it, so the fixes
// must refuse that answer rather than write it back.
func TestBeadsRedirectTargetCheck_FixRefusesLegacyRecompute(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationConfigOnlyFiles)
	writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
	crewBeads := filepath.Join(rigPath, "crew", "max", ".beads")
	writeBeadsLocationFiles(t, crewBeads, map[string]string{"redirect": "../../missing/.beads\n"})

	check := NewBeadsRedirectTargetCheck()
	ctx := &CheckContext{TownRoot: tmpDir, RigName: "testrig"}
	if got := check.Run(ctx); got.Status != StatusWarning {
		t.Fatalf("Run status = %v (%s), want StatusWarning for the missing target", got.Status, got.Message)
	}
	err := check.Fix(ctx)
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect to the legacy mayor/rig/.beads") {
		t.Fatalf("Fix error = %v, want a refusal naming the legacy mayor/rig/.beads", err)
	}
	content, readErr := os.ReadFile(filepath.Join(crewBeads, "redirect"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "../../missing/.beads\n" {
		t.Errorf("worktree redirect after Fix = %q, want it left as it was", string(content))
	}
}

func TestStaleBeadsRedirectCheck_FixRefusesLegacyRedirect(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "testrig")
	writeBeadsLocationFiles(t, filepath.Join(rigPath, ".beads"), beadsLocationConfigOnlyFiles)
	writeBeadsLocationFiles(t, filepath.Join(rigPath, "mayor", "rig", ".beads"), beadsLocationRigFiles)
	crewBeads := filepath.Join(rigPath, "crew", "max", ".beads")
	writeBeadsLocationFiles(t, crewBeads, map[string]string{"redirect": "../../missing/.beads\n"})

	check := NewStaleBeadsRedirectCheck()
	ctx := &CheckContext{TownRoot: tmpDir}
	if got := check.Run(ctx); got.Status != StatusWarning {
		t.Fatalf("Run status = %v (%s), want StatusWarning for the incorrect redirect", got.Status, got.Message)
	}
	err := check.Fix(ctx)
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect to the legacy mayor/rig/.beads") {
		t.Fatalf("Fix error = %v, want a refusal naming the legacy mayor/rig/.beads", err)
	}
	content, readErr := os.ReadFile(filepath.Join(crewBeads, "redirect"))
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(content) != "../../missing/.beads\n" {
		t.Errorf("worktree redirect after Fix = %q, want it left as it was", string(content))
	}
}
