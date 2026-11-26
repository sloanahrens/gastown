package bdgateway

import (
	"strings"
	"testing"
)

// assertExternalProjectsErr fails unless err is nil when want is empty, and
// otherwise is non-nil and names every substring in want.
func assertExternalProjectsErr(t *testing.T, err error, want []string) {
	t.Helper()
	if len(want) == 0 {
		if err != nil {
			t.Fatalf("checkExternalProjects() = %v, want nil", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("checkExternalProjects() = nil, want an error naming %q", want)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("checkExternalProjects() = %q, want it to name %q", err, w)
		}
	}
}

// TestCheckExternalProjects: gastown's beads at ../../../beads/mayor/rig resolves
// to beads/mayor/rig, which must hold a .beads/; a sibling of .beads does not count.
func TestCheckExternalProjects(t *testing.T) {
	t.Parallel()
	const gastown = "gt/mayor/rig/.beads/config.yaml"
	beadsCfg := "external_projects:\n  beads: ../../../beads/mayor/rig\n"
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"resolved project has .beads", map[string]string{gastown: beadsCfg, "beads/mayor/rig/.beads/config.yaml": "issue-prefix: \"be-\"\n"}, nil},
		{"resolved project has no .beads", map[string]string{gastown: beadsCfg}, []string{gastown, `"beads"`, "beads/mayor/rig"}},
		{"sibling of .beads does not count", map[string]string{gastown: beadsCfg, "beads/mayor/rig/.beads-old/config.yaml": "x\n"}, []string{gastown, `"beads"`, "beads/mayor/rig"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertExternalProjectsErr(t, checkExternalProjects(tt.files), tt.want)
		})
	}
}

// TestCheckExternalProjectsShapes: devops's .beads sits at devops/.beads, so
// ../gastown/mayor/rig resolves to gastown/mayor/rig. Also covers absolute paths
// (skipped), configs without external_projects or that do not parse (not errors
// here), an empty path and a path climbing above the tree's root (refused).
func TestCheckExternalProjectsShapes(t *testing.T) {
	t.Parallel()
	const devops = "devops/.beads/config.yaml"
	const gastownBeads = "gastown/mayor/rig/.beads/config.yaml"
	devopsCfg := "external_projects:\n  gastown: ../gastown/mayor/rig\n"
	tests := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"devops resolves to gastown", map[string]string{devops: devopsCfg, gastownBeads: "issue-prefix: \"gt-\"\n"}, nil},
		{"devops names gastown with no .beads", map[string]string{devops: devopsCfg}, []string{devops, `"gastown"`, "gastown/mayor/rig"}},
		{"absolute path is skipped", map[string]string{devops: "external_projects:\n  gastown: /srv/gastown/mayor/rig\n"}, nil},
		{"no external_projects is not an error", map[string]string{devops: "issue-prefix: \"dv-\"\n"}, nil},
		{"unparseable config is not an error here", map[string]string{devops: "external_projects: [\n"}, nil},
		{"empty path is refused", map[string]string{devops: "external_projects:\n  gastown: \"\"\n"}, []string{devops, `"gastown"`}},
		{"path climbing above root is refused", map[string]string{".beads/config.yaml": "external_projects:\n  beads: ../beads/mayor/rig\n"}, []string{".beads/config.yaml", `"beads"`, "above the tree root"}},
		{"root config resolving inside the tree passes", map[string]string{".beads/config.yaml": "external_projects:\n  devops: devops\n", "devops/.beads/config.yaml": "x\n"}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assertExternalProjectsErr(t, checkExternalProjects(tt.files), tt.want)
		})
	}
}
