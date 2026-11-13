package specdispatch

import (
	"slices"
	"testing"
)

// TestPathTokens pins the token shapes the risk match reads: a repo path, a
// package path, a root file with no directory, a glob in prose and a line
// reference. A form that is not one literal repo path (an absolute path, a
// parent segment) is dropped, and a name named twice is read once.
func TestPathTokens(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		text string
		want []string
	}{
		{name: "a repo path", text: "internal/land/riskpaths.go", want: []string{"internal/land/riskpaths.go"}},
		{name: "a package path in backticks and a root file", text: "`internal/daemon` Makefile", want: []string{"internal/daemon", "Makefile"}},
		{name: "a line reference", text: "./internal/cmd/spec.go:42", want: []string{"internal/cmd/spec.go"}},
		{name: "a glob in prose names its subtree", text: "internal/daemon/**", want: []string{"internal/daemon"}},
		{name: "a name named twice is read once", text: "internal/land, internal/land", want: []string{"internal/land"}},
		{name: "an absolute path and a parent segment are dropped", text: "/usr/local/bin ../x/y.go", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := PathTokens(tc.text); !slices.Equal(got, tc.want) {
				t.Errorf("PathTokens(%q) = %v, want %v", tc.text, got, tc.want)
			}
		})
	}
}

// riskPathsThree is a bead text naming three paths on the risk list, sorted as
// the landing worker's matcher returns them.
var riskPathsThree = []string{"Makefile", "internal/daemon/supervisor.go", "internal/land/riskpaths.txt"}

// TestLintRiskPathWarnings pins the warning: three or more risk paths named get
// one WARN line with the count and the paths, two and none get nothing, a
// size-ok bead is not warned, and an unread risk list warns that the check did
// not run. A warning is advisory: every row keeps the bead clean.
func TestLintRiskPathWarnings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		paths   []string
		labels  []string
		readErr string
		want    []string
	}{
		{
			name:  "three risk paths warn once",
			paths: riskPathsThree,
			want:  []string{"3 risk paths named (one risk path per bead): Makefile, internal/daemon/supervisor.go, internal/land/riskpaths.txt"},
		},
		{name: "two risk paths do not warn", paths: riskPathsThree[:2]},
		{name: "no risk paths do not warn"},
		{name: "a size-ok bead is not warned", paths: riskPathsThree, labels: []string{"size-ok"}},
		{name: "a size-ok label in any case is not warned", paths: riskPathsThree, labels: []string{" Size-OK "}},
		{
			name:    "an unread risk list warns that the check did not run",
			readErr: "git: boom",
			want:    []string{"the risk-path check did not run: git: boom"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := goodSpec()
			s.RiskPaths = tc.paths
			s.Labels = tc.labels
			s.RiskPathsErr = tc.readErr
			v := Lint(s, defaultTemplate())
			if !v.Clean() {
				t.Fatalf("verdict = %q, want the bead clean: a warning must not change the route", v.Line("gt-good"))
			}
			if !slices.Equal(v.Warnings, tc.want) {
				t.Errorf("Warnings = %q, want %q", v.Warnings, tc.want)
			}
		})
	}
}

// TestRiskPathWarningRidesOnARefusedBead pins the Warnings comment: a refused
// bead still learns its text names too many risk paths, and the refusal is
// still what decides its line.
func TestRiskPathWarningRidesOnARefusedBead(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	s.Description = "## Goal\nA thing.\n\n## Size\none worker, one MR"
	s.RiskPaths = riskPathsThree
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse {
		t.Fatalf("route = %q, want a refusal", v.Route)
	}
	if len(v.Warnings) != 1 {
		t.Errorf("Warnings = %q, want the risk-path warning beside the refusal", v.Warnings)
	}
}

// TestRiskPathWarningSkipsNonWorkBeads pins the work-bead guard: an epic's text
// names many paths by design, so it is not warned.
func TestRiskPathWarningSkipsNonWorkBeads(t *testing.T) {
	t.Parallel()
	s := goodSpec()
	s.Type = "epic"
	s.RiskPaths = riskPathsThree
	v := Lint(s, defaultTemplate())
	if v.Route != RouteRefuse {
		t.Fatalf("route = %q, want the epic refused as not a work bead", v.Route)
	}
	if len(v.Warnings) != 0 {
		t.Errorf("Warnings = %q, want none for a non-work bead", v.Warnings)
	}
}
