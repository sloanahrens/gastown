package land

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// gateStep is one step of a gate workflow, read for what it fetches.
type gateStep struct {
	Uses string         `yaml:"uses"`
	Run  string         `yaml:"run"`
	With map[string]any `yaml:"with"`
}

// TestGateWorkflowFetchesMainNotAllBranches holds the committed gate
// workflow's fetch to the refs the gate reads: the candidate and main with
// history, and no other head. fetch-depth: 0 fetched all 669 heads, 8s of a
// 49s job, and a depth-1 checkout with no deepening leaves `git describe
// --tags --always` no tag to reach (gt-ebco0).
func TestGateWorkflowFetchesMainNotAllBranches(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, GateWorkflowPath("gate")))
	if err != nil {
		t.Fatalf("reading this repo's gate workflow: %v", err)
	}
	var doc struct {
		Jobs map[string]struct {
			Steps []gateStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("parsing this repo's gate workflow: %v", err)
	}
	job, ok := doc.Jobs["gate"]
	if !ok {
		t.Fatal("this repo's gate workflow has no gate job")
	}
	deepensMain := false
	for _, step := range job.Steps {
		if depth, set := step.With["fetch-depth"]; set && fmt.Sprint(depth) == "0" {
			t.Errorf("the %s checkout fetches every head (fetch-depth: 0)", step.Uses)
		}
		if strings.Contains(step.Run, "+refs/heads/*") {
			t.Errorf("the step fetching %q creates a ref for every branch", step.Run)
		}
		if strings.Contains(step.Run, "+refs/heads/main:refs/remotes/origin/main") && strings.Contains(step.Run, "--depth=2147483647") {
			deepensMain = true
		}
	}
	if !deepensMain {
		t.Fatal("no step fetches origin/main with full history, which git describe and the landing's ancestry reads need")
	}
}
