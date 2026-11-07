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

// TestPostLandWorkflowFetchesNoRef holds the committed post-land workflow to
// fetching no ref beyond the checkout. Under GITHUB_ACTIONS post-land-shell.sh
// takes its CI path and runs the shell tier without reading main's first-parent
// window (gt-phsp89), so a deepened fetch pays a full-history transfer per push
// for a read this run does not perform, and fetch-depth: 0 pays for every head
// on the way (gt-ebco0).
func TestPostLandWorkflowFetchesNoRef(t *testing.T) {
	t.Parallel()
	steps := postLandWorkflowSteps(t)
	problems, deepensMain := gateFetchProblems(steps)
	for _, p := range problems {
		t.Error(p)
	}
	if deepensMain {
		t.Error("a step deepens origin/main, but this run reads no history: post-land-shell.sh runs the shell tier under GITHUB_ACTIONS without one")
	}
	for _, step := range steps {
		if fetches := gitFetchWords(step.Run); len(fetches) > 0 {
			t.Errorf("a step runs git fetch %q, but this run reads no history and no tag", fetches)
		}
	}
}

// TestPostLandWorkflowFetchesNoTags holds the committed post-land workflow to
// fetching no tag: nothing the shell tier runs reads one.
func TestPostLandWorkflowFetchesNoTags(t *testing.T) {
	t.Parallel()
	for _, p := range gateTagFetchProblems(postLandWorkflowSteps(t)) {
		t.Error(p)
	}
}

// gateTagFetchProblems names each step that fetches a tag, whether the fetch is
// a run command or actions/checkout's fetch-tags input. An input the guard does
// not recognize as false counts as a tag fetch: unknown denies.
func gateTagFetchProblems(steps []gateStep) []string {
	var problems []string
	for _, step := range steps {
		if v, set := step.With["fetch-tags"]; set && fmt.Sprint(v) != "false" {
			problems = append(problems, fmt.Sprintf("the %s checkout has fetch-tags: %v, so it fetches tags", step.Uses, v))
		}
		for _, words := range gitFetchWords(step.Run) {
			if fetchesTags(words) {
				problems = append(problems, fmt.Sprintf("the step fetching %q fetches a tag", strings.Join(words, " ")))
			}
		}
	}
	return problems
}

// TestGateTagFetchProblemsNamesTagFetches drives the no-tag guard's refusing
// branch, which the committed workflow cannot: it fetches no tag, so the guard
// on it stays quiet either way. Each form of tag fetch is refused here, and a
// fetch that leaves tags alone is not.
func TestGateTagFetchProblemsNamesTagFetches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		steps        []gateStep
		wantProblems int
	}{
		{name: "a deepening fetch of main that leaves tags alone",
			steps: []gateStep{{Run: `git fetch --no-tags --prune --depth=2147483647 origin "+refs/heads/main:refs/remotes/origin/main"`}}},
		{name: "a --tags fetch takes every tag",
			steps: []gateStep{{Run: "git fetch --tags origin main"}}, wantProblems: 1},
		{name: "a tag glob refspec",
			steps: []gateStep{{Run: `git fetch --no-tags --prune origin "+refs/tags/*:refs/tags/*"`}}, wantProblems: 1},
		{name: "a single tag refspec",
			steps: []gateStep{{Run: "git fetch origin refs/tags/v1.0.0"}}, wantProblems: 1},
		{name: "a tag fetch after a separator",
			steps: []gateStep{{Run: "make gate && git fetch --tags"}}, wantProblems: 1},
		{name: "a tag fetch in a subshell",
			steps: []gateStep{{Run: "(git fetch --tags)"}}, wantProblems: 1},
		{name: "a tag fetch on a later step",
			steps: []gateStep{{Run: "make gate"}, {Run: "git fetch origin +refs/tags/*:refs/tags/*"}}, wantProblems: 1},
		{name: "a checkout that fetches tags",
			steps: []gateStep{{Uses: "actions/checkout@v4", With: map[string]any{"fetch-tags": true}}}, wantProblems: 1},
		{name: "a checkout with fetch-tags false",
			steps: []gateStep{{Uses: "actions/checkout@v4", With: map[string]any{"fetch-tags": false}}}},
		{name: "a run with no fetch",
			steps: []gateStep{{Run: "make gate"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gateTagFetchProblems(tc.steps); len(got) != tc.wantProblems {
				t.Errorf("gateTagFetchProblems() = %q, want %d problem(s)", got, tc.wantProblems)
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
