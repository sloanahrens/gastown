package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/git"
	"github.com/steveyegge/gastown/internal/specdispatch"
)

// fakeRiskTree is fakeTree with a revision's file content, so a test reads a
// risk list out of the target branch. A readErr stands for a git failure other
// than an absent path.
type fakeRiskTree struct {
	fakeTree
	content map[string]string
	readErr error
}

func (f fakeRiskTree) ShowFileAtRev(ref, path string) (string, error) {
	if f.readErr != nil {
		return "", f.readErr
	}
	if content, ok := f.content[ref+":"+path]; ok {
		return content, nil
	}
	return "", git.ErrNotAtRef
}

// riskListText is a risk list shaped like internal/land/riskpaths.txt: two
// subtrees and one root file.
const riskListText = "internal/daemon/**\ninternal/land/**\nMakefile\n"

// riskSpec is a clean-shaped work bead whose Goal names the paths under test.
func riskSpec(goal string, labels ...string) specdispatch.Spec {
	return specdispatch.Spec{
		ID:     "gt-risk",
		Type:   "task",
		Status: "open",
		Title:  "Change the daemon",
		Description: "## Goal\n" + goal + "\n\n" +
			"## Constraints\nGo only.\n\n" +
			"## Out of scope\nNothing else.\n\n" +
			"## Gate\nmake gate\n\n" +
			"## Size\none worker, one MR",
		Acceptance: "- [ ] the change lands",
		Labels:     labels,
	}
}

// TestSpecLintRiskPathWarn drives the production path a filer's bead takes: the
// risk list is read from the target branch, the bead's text is matched against
// it, and the lint prints the WARN line. Three or more risk paths get one WARN
// naming the count and the paths; two, none, a size-ok label and a branch with
// no risk list stay silent; an unread list warns that the check did not run.
// Every row keeps the verdict and the exit code a warning never changes.
func TestSpecLintRiskPathWarn(t *testing.T) {
	t.Parallel()
	const three = "Change internal/daemon/supervisor.go, internal/land/riskpaths.txt and the Makefile"
	tests := []struct {
		name     string
		goal     string
		labels   []string
		riskList string
		readErr  error
		want     []string
	}{
		{
			name:     "three risk paths get one WARN naming the count and the paths",
			goal:     three,
			riskList: riskListText,
			want:     []string{"WARN: 3 risk paths named (one risk path per bead): Makefile, internal/daemon/supervisor.go, internal/land/riskpaths.txt"},
		},
		{
			name:     "two risk paths are not warned",
			goal:     "Change internal/daemon/supervisor.go and the Makefile",
			riskList: riskListText,
		},
		{
			name:     "no risk paths are not warned",
			goal:     "Change internal/cmd/spec.go",
			riskList: riskListText,
		},
		{
			name:     "a size-ok bead is not warned",
			goal:     three,
			labels:   []string{"size-ok"},
			riskList: riskListText,
		},
		{
			name: "a branch without the risk list is not warned",
			goal: three,
		},
		{
			name:    "an unread risk list warns that the check did not run",
			goal:    three,
			readErr: errors.New("git: boom"),
			want:    []string{"WARN: the risk-path check did not run: git: boom"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rigPath := rigAt(t)
			files := []string{"internal/cmd/spec.go"}
			content := map[string]string{}
			if tc.riskList != "" {
				files = append(files, "internal/land/riskpaths.txt")
				content["origin/main:internal/land/riskpaths.txt"] = tc.riskList
			}
			target := specTargetTree(rigPath, func(string) treeReader {
				return fakeRiskTree{fakeTree: fakeTree{"origin/main": files}, content: content, readErr: tc.readErr}
			})
			if target.Err != "" {
				t.Fatalf("target inventory: %s", target.Err)
			}
			spec := riskSpec(tc.goal, tc.labels...)
			spec.RiskPaths, spec.RiskPathsErr = specRiskPaths(spec, target)
			var out, errOut bytes.Buffer
			if err := specLint(&out, &errOut, spec.ID, spec, nil, "", nil, false); err != nil {
				t.Fatalf("lint returned %v, want exit 0: a risk warning must not change the exit code", err)
			}
			if got := strings.TrimSpace(out.String()); got != "gt-risk: spec lint ok" {
				t.Errorf("verdict = %q, want the clean line", got)
			}
			var warns []string
			for _, line := range strings.Split(strings.TrimSpace(errOut.String()), "\n") {
				if strings.HasPrefix(line, "WARN: ") {
					warns = append(warns, line)
				}
			}
			if !slices.Equal(warns, tc.want) {
				t.Errorf("WARN lines = %q, want %q\nstderr:\n%s", warns, tc.want, errOut.String())
			}
		})
	}
}

// TestSpecRiskPathsWithoutATarget pins the guard for a caller that read no
// branch: the check did not run, which is named, not read as a branch with no
// risk list.
func TestSpecRiskPathsWithoutATarget(t *testing.T) {
	t.Parallel()
	paths, readErr := specRiskPaths(riskSpec("Change the Makefile"), nil)
	if len(paths) != 0 || readErr == "" {
		t.Errorf("specRiskPaths without a target = %q, %q; want no paths and the unread reason", paths, readErr)
	}
}

// TestSpecForLintFillsTheRiskPaths pins the wiring the warning rides on: gt
// spec lint's own read (specForLint) fills Spec.RiskPaths from the target
// branch's risk list and the bead's text. Removing that match from specForLint
// turns this test red.
func TestSpecForLintFillsTheRiskPaths(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, ".beads"), 0o755); err != nil {
		t.Fatal(err)
	}
	routes := `{"prefix":"gt-","path":"gastown/mayor/rig"}` + "\n"
	if err := os.WriteFile(filepath.Join(townRoot, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatal(err)
	}
	rig := filepath.Join(townRoot, "gastown")
	if err := os.MkdirAll(filepath.Join(rig, ".repo.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	show := func(id string) (specdispatch.Spec, error) {
		return riskSpec("Change internal/daemon/supervisor.go, internal/land/riskpaths.txt and the Makefile"), nil
	}
	spec, err := specForLint(townRoot, "gt-risk", show, unexpectedBlockerRead(t), func(string) treeReader {
		return fakeRiskTree{
			fakeTree: fakeTree{"origin/main": {"internal/cmd/spec.go", "internal/land/riskpaths.txt"}},
			content:  map[string]string{"origin/main:internal/land/riskpaths.txt": riskListText},
		}
	})
	if err != nil {
		t.Fatalf("specForLint: %v", err)
	}
	want := []string{"Makefile", "internal/daemon/supervisor.go", "internal/land/riskpaths.txt"}
	if !slices.Equal(spec.RiskPaths, want) {
		t.Errorf("RiskPaths = %q, want the three named paths matched against the list", spec.RiskPaths)
	}
	if spec.RiskPathsErr != "" {
		t.Errorf("RiskPathsErr = %q, want none", spec.RiskPathsErr)
	}
}

// TestSpecLintRiskPathWarnJSON pins the report: --json carries the warning in
// its warnings array and writes nothing to stderr, as advice does.
func TestSpecLintRiskPathWarnJSON(t *testing.T) {
	t.Parallel()
	spec := riskSpec("Change internal/daemon/supervisor.go, internal/land/riskpaths.txt and the Makefile")
	spec.RiskPaths = []string{"Makefile", "internal/daemon/supervisor.go", "internal/land/riskpaths.txt"}
	var out, errOut bytes.Buffer
	if err := specLint(&out, &errOut, spec.ID, spec, nil, "", nil, true); err != nil {
		t.Fatalf("lint returned %v, want exit 0", err)
	}
	if errOut.Len() != 0 {
		t.Errorf("--json wrote to stderr: %q", errOut.String())
	}
	var got specLintReport
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out.String())
	}
	want := []string{"3 risk paths named (one risk path per bead): Makefile, internal/daemon/supervisor.go, internal/land/riskpaths.txt"}
	if !slices.Equal(got.Warnings, want) {
		t.Errorf("warnings = %q, want %q", got.Warnings, want)
	}
	if !got.OK {
		t.Errorf("ok = false, want the bead dispatchable; a warning must not change the verdict")
	}
}
