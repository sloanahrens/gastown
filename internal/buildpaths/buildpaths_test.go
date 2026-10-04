package buildpaths

import "testing"

// TestAffectsBuild pins the shared rule on the shapes that look exempt but are
// not, and the embed carve-outs that look eligible but are not: the two
// consumers of it disagree about an install only if this table is wrong.
func TestAffectsBuild(t *testing.T) {
	t.Parallel()
	tests := []struct {
		path    string
		affects bool
	}{
		// Documentation, markdown, test sources and testdata: the binary the
		// build writes is the same either way.
		{"docs/design/architecture.md", false},
		{"README.md", false},
		{"AGENTS.md", false},
		{"internal/daemon/rebuild_gt_test.go", false},
		{"testdata/fixture.txt", false},
		{"internal/daemon/testdata/fixture.txt", false},
		// An embed directory's contents are compiled into the binary, so even
		// the documentation under one is.
		{"internal/templates/polecat-CLAUDE.md", true},
		{"internal/templates/commands/bodies/foo.md", true},
		{"internal/cmdtree/bd-command-tree.json", true},
		{"internal/config/roles/worker.toml", true},
		// A test source is never compiled in, embed directory or not.
		{"internal/templates/townroot_test.go", false},
		// Source is compiled, scripts and plugins are run, markdown in their
		// trees is still markdown.
		{"internal/version/stale.go", true},
		{"main.go", true},
		{"scripts/install-gt.sh", true},
		{"plugins/README.md", false},
	}
	for _, tc := range tests {
		if got := AffectsBuild(tc.path); got != tc.affects {
			t.Errorf("AffectsBuild(%q) = %v, want %v", tc.path, got, tc.affects)
		}
	}
}
