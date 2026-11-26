package bdgateway

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// trackedGastownConfigRoot walks up from the package directory to the module
// root, the directory that holds go.mod.
func trackedGastownConfigRoot(t *testing.T) string {
	t.Helper()
	start, err := os.Getwd()
	if err != nil {
		t.Fatalf("os.Getwd() = %v", err)
	}
	for dir := start; dir != filepath.Dir(dir); dir = filepath.Dir(dir) {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
	}
	t.Fatalf("no go.mod above %s", start)
	return ""
}

// rigDirFromMayorRig resolves path from mayor/rig and returns the first
// directory name left once the leading ".." steps are gone: the rig that
// the path names.
func rigDirFromMayorRig(path string) string {
	resolved := filepath.ToSlash(filepath.Clean(filepath.Join("mayor", "rig", path)))
	for _, part := range strings.Split(resolved, "/") {
		if part != ".." && part != "." && part != "" {
			return part
		}
	}
	return ""
}

// TestTrackedGastownConfigExternalProjects: the tracked gastown .beads/config.yaml
// names every rig a gastown bead may wait on. Each entry holds the exact path,
// and that path, resolved from mayor/rig, lands on the entry's own rig directory.
func TestTrackedGastownConfigExternalProjects(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(trackedGastownConfigRoot(t), ".beads", "config.yaml"))
	if err != nil {
		t.Fatalf("read tracked .beads/config.yaml: %v", err)
	}
	var cfg struct {
		ExternalProjects map[string]string `yaml:"external_projects"`
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse tracked .beads/config.yaml: %v", err)
	}
	tests := []struct {
		name string
		path string
	}{
		{"beads", "../../../beads/mayor/rig"},
		{"devops", "../../../devops"},
		{"om", "../../../om/mayor/rig"},
		{"hm", "../../../hm/mayor/rig"},
		{"mango", "../../../mango/mayor/rig"},
	}
	if len(cfg.ExternalProjects) != len(tests) {
		t.Errorf("external_projects has %d entries %v, want exactly %d", len(cfg.ExternalProjects), cfg.ExternalProjects, len(tests))
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := cfg.ExternalProjects[tc.name]
			if !ok {
				t.Fatalf("external_projects has no %q entry", tc.name)
			}
			if got != tc.path {
				t.Errorf("external_projects[%q] = %q, want %q", tc.name, got, tc.path)
			}
			if rig := rigDirFromMayorRig(got); rig != tc.name {
				t.Errorf("external_projects[%q] resolves from mayor/rig to rig %q, want %q", tc.name, rig, tc.name)
			}
		})
	}
}
