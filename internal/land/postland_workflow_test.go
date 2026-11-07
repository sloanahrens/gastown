package land

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// postLandWorkflowData reads this repo's committed post-land workflow.
func postLandWorkflowData(t *testing.T) []byte {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, GateWorkflowPath("post-land")))
	if err != nil {
		t.Fatalf("reading this repo's post-land workflow: %v", err)
	}
	return data
}

// postLandWorkflowSteps returns the steps of the workflow's one job.
func postLandWorkflowSteps(t *testing.T) []gateStep {
	t.Helper()
	var doc struct {
		Jobs map[string]struct {
			Steps []gateStep `yaml:"steps"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(postLandWorkflowData(t), &doc); err != nil {
		t.Fatalf("parsing this repo's post-land workflow: %v", err)
	}
	job, ok := doc.Jobs["check"]
	if !ok {
		t.Fatal("this repo's post-land workflow has no check job")
	}
	return job.Steps
}

// TestPostLandWorkflowContextIsTheRigSetting holds the committed post-land
// workflow's name and job key to the context a rig's
// merge_queue.post_land_status_context must name, "post-land / check (push)".
// The landing worker reads that setting from the rig's own
// settings/config.json and nothing in the repo can derive it, so this test is
// the only tie the two have: a rename of either half leaves the setting
// orphaning and every landed commit's verdict UNKNOWN (gt-gg9hv.7.1).
func TestPostLandWorkflowContextIsTheRigSetting(t *testing.T) {
	t.Parallel()
	data := postLandWorkflowData(t)
	wf, err := ParseGateWorkflow(data)
	if err != nil {
		t.Fatalf("this repo's post-land workflow: %v", err)
	}
	if wf.Name != "post-land" || wf.Job != "check" {
		t.Fatalf("post-land workflow = %+v, want the workflow named post-land and its check job", wf)
	}
	if got := wf.Context(); got != "post-land / check (push)" {
		t.Fatalf("Context() = %q, want %q", got, "post-land / check (push)")
	}
}

// TestPostLandWorkflowKeepsHistoryAndTags holds the committed post-land
// workflow to the full-history checkout and a tags fetch, which the shell
// tier's git describe needs (gt-b14yam).
func TestPostLandWorkflowKeepsHistoryAndTags(t *testing.T) {
	t.Parallel()
	for _, p := range postLandHistoryProblems(postLandWorkflowSteps(t)) {
		t.Error(p)
	}
}

// postLandHistoryProblems names each thing the post-land workflow lacks for the
// shell tier's git describe: a checkout with the full history, and a step that
// fetches the tags.
func postLandHistoryProblems(steps []gateStep) []string {
	var problems []string
	fullHistory, tagsFetched := false, false
	for _, step := range steps {
		if strings.HasPrefix(step.Uses, "actions/checkout") && fmt.Sprint(step.With["fetch-depth"]) == "0" {
			fullHistory = true
		}
		for _, words := range gitFetchWords(step.Run) {
			if fetchesTags(words) {
				tagsFetched = true
			}
		}
	}
	if !fullHistory {
		problems = append(problems, "the checkout does not set fetch-depth: 0, so the shell tier has no history for git describe")
	}
	if !tagsFetched {
		problems = append(problems, "no step fetches the tags, so git describe finds none and makefile-gate_test.sh fails")
	}
	return problems
}

// TestPostLandHistoryProblemsNamesMissingHistoryAndTags drives each refusal of
// the history and tags guard, one row per refusal, and the forms it accepts.
// Each row names the problems it expects, by a phrase each one must contain.
func TestPostLandHistoryProblemsNamesMissingHistoryAndTags(t *testing.T) {
	t.Parallel()
	fullCheckout := gateStep{Uses: "actions/checkout@v4", With: map[string]any{"fetch-depth": 0}}
	tagGlob := gateStep{Run: `git fetch --no-tags --prune origin "+refs/tags/*:refs/tags/*"`}
	tests := []struct {
		name  string
		steps []gateStep
		want  []string
	}{
		{name: "a full-history checkout and a tag glob fetch",
			steps: []gateStep{fullCheckout, tagGlob}},
		{name: "a --tags fetch takes the tags too",
			steps: []gateStep{fullCheckout, {Run: "git fetch --tags origin"}}},
		{name: "a checkout with no fetch-depth",
			steps: []gateStep{{Uses: "actions/checkout@v4"}, tagGlob}, want: []string{"fetch-depth: 0"}},
		{name: "a checkout with fetch-depth 1",
			steps: []gateStep{{Uses: "actions/checkout@v4", With: map[string]any{"fetch-depth": 1}}, tagGlob}, want: []string{"fetch-depth: 0"}},
		{name: "a fetch-depth 0 on an action that is not a checkout",
			steps: []gateStep{{Uses: "other/action@v1", With: map[string]any{"fetch-depth": 0}}, tagGlob}, want: []string{"fetch-depth: 0"}},
		{name: "no step fetches the tags",
			steps: []gateStep{fullCheckout}, want: []string{"fetches the tags"}},
		{name: "refs/tags in a run that is not a git fetch",
			steps: []gateStep{fullCheckout, {Run: "echo refs/tags/v1.0.0"}}, want: []string{"fetches the tags"}},
		{name: "neither the checkout nor a tag fetch",
			steps: []gateStep{{Run: "make gate"}}, want: []string{"fetch-depth: 0", "fetches the tags"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := postLandHistoryProblems(tc.steps)
			if len(got) != len(tc.want) {
				t.Fatalf("postLandHistoryProblems() = %q, want %d problem(s)", got, len(tc.want))
			}
			for i, phrase := range tc.want {
				if !strings.Contains(got[i], phrase) {
					t.Errorf("problem %d = %q, want it to contain %q", i, got[i], phrase)
				}
			}
		})
	}
}

// fetchesTags reports whether a git fetch's words take the repository's tags:
// --tags, or any word naming refs/tags.
func fetchesTags(words []string) bool {
	for _, w := range words {
		if w == "--tags" || strings.Contains(w, "refs/tags") {
			return true
		}
	}
	return false
}
