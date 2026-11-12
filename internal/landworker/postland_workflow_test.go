package landworker

import (
	"strings"
	"testing"
)

// postLandContext is the status context a rig's merge_queue.post_land_status_context
// names for this repo's post-land workflow.
const postLandContext = "post-land / check (push)"

// postLandFile is the path the committed post-land workflow lives at.
const postLandFile = ".forgejo/workflows/post-land.yml"

// gastownPostLandWorkflow is the committed .forgejo/workflows/post-land.yml, copied
// in so the matcher is tested on the text that ships, with no file read.
const gastownPostLandWorkflow = `# The post-land check: runs on every push to main, on the landed commit, and
# posts the commit status "post-land / check (push)". The landing worker reads
# that status as the rig's post-land verdict (merge_queue.post_land_status_context
# in the rig's own settings). Changing this file is a human step.
#
# The workflow's name and its one job key are the status context; renaming
# either orphans the setting that names it. That setting is read from the rig's
# own settings/config.json and nothing in this repo can derive it, so
# TestPostLandWorkflowContextIsTheRigSetting holds both halves to the context it
# must name (gt-gg9hv.7.1).
#
# The checkout takes the full history and the tags. Under GITHUB_ACTIONS
# post-land-shell.sh skips its first-parent window (gt-phsp89), but the shell
# tier still stamps versions with git describe, and scripts/makefile-gate_test.sh
# fails without the history and the tags: removing them turned every post-land
# check on main red from 7ae99ed7 (gt-b14yam). TestPostLandWorkflowKeepsHistoryAndTags
# pins both.
#
# No concurrency block on purpose: a cancelled or replaced run would leave a
# failure or no status on a commit that did nothing wrong.
name: post-land
on:
  push:
    branches: [main]

jobs:
  check:
    runs-on: ci-base
    steps:
      - uses: actions/checkout@v4
        with:
          fetch-depth: 0   # the shell tier's version stamp (git describe) reads the history
      - name: fetch the tags
        run: git fetch --no-tags --prune origin "+refs/tags/*:refs/tags/*"
      - run: GT_TEST_DOCKER=0 bash scripts/post-land-shell.sh
`

// oneWorkflow is a files map holding a single workflow at the path a rig uses.
func oneWorkflow(file, body string) map[string][]byte {
	return map[string][]byte{file: []byte(body)}
}

// postLandWith is a single post-land.yml whose on: block is the given text. on
// must end in a newline.
func postLandWith(on string) map[string][]byte {
	return oneWorkflow(postLandFile, "name: post-land\n"+on+"jobs:\n  check:\n    runs-on: ci-base\n")
}

// TestWorkflowsPostContextMatches holds the trigger and naming forms that post
// the context on every push to main: the real post-land.yml, each form of on: push,
// a workflow named by its file, and a job whose name: does not change the context.
func TestWorkflowsPostContextMatches(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string][]byte
	}{
		{name: "the committed post-land.yml text",
			files: oneWorkflow(postLandFile, gastownPostLandWorkflow)},
		{name: "on: push",
			files: oneWorkflow(postLandFile, "name: post-land\non: push\njobs:\n  check:\n    runs-on: ci-base\n")},
		{name: "on: [push]",
			files: oneWorkflow(postLandFile, "name: post-land\non: [push]\njobs:\n  check:\n    runs-on: ci-base\n")},
		{name: "on: {push: {branches: [main]}}",
			files: oneWorkflow(postLandFile, "name: post-land\non: {push: {branches: [main]}}\njobs:\n  check:\n    runs-on: ci-base\n")},
		{name: "on: {push: null}",
			files: oneWorkflow(postLandFile, "name: post-land\non: {push: null}\njobs:\n  check:\n    runs-on: ci-base\n")},
		{name: "a push with no value runs on every branch",
			files: postLandWith("on:\n  push:\n")},
		{name: "a branch glob that matches main",
			files: postLandWith("on:\n  push:\n    branches: ['ma*']\n")},
		{name: "no name: takes the file name without its extension",
			files: oneWorkflow(postLandFile, "on: push\njobs:\n  check:\n    runs-on: ci-base\n")},
		{name: "a job name: does not change the job key in the context",
			files: oneWorkflow(postLandFile, "name: post-land\non: push\njobs:\n  check:\n    name: Check the land\n    runs-on: ci-base\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, why := WorkflowsPostContext(tc.files, postLandContext)
			if verdict != PostsOnEveryPush {
				t.Fatalf("WorkflowsPostContext(%q) verdict = %v, why = %q; want PostsOnEveryPush", postLandContext, verdict, why)
			}
		})
	}
}

// TestWorkflowsPostContextMisses holds each way a rig's context goes unposted, and
// the cause why names. A row with want set to PostsOnEveryPush is the one case where
// a good file still posts while a malformed neighbour is named in why.
func TestWorkflowsPostContextMisses(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   map[string][]byte
		want    PostVerdict
		wantWhy string
	}{
		{name: "no workflow files",
			files: map[string][]byte{}, wantWhy: "no workflow files"},
		{name: "a push filter that leaves out main",
			files:   postLandWith("on:\n  push:\n    branches: [dev]\n"),
			wantWhy: `runs on push to branches "dev", not main`},
		{name: "a push branches-ignore that names main",
			files:   postLandWith("on:\n  push:\n    branches-ignore: [main]\n"),
			wantWhy: "ignores main on push"},
		{name: "a branch pattern that is not valid",
			files:   postLandWith("on:\n  push:\n    branches: ['[']\n"),
			wantWhy: "not a valid pattern"},
		{name: "a branches-ignore pattern that is not valid",
			files:   postLandWith("on:\n  push:\n    branches-ignore: ['[']\n"),
			wantWhy: "branches-ignore filter that is not a valid pattern"},
		{name: "a renamed workflow",
			files:   oneWorkflow(postLandFile, "name: gate\non: push\njobs:\n  check:\n    runs-on: ci-base\n"),
			wantWhy: `is named "gate", not "post-land"`},
		{name: "a renamed job key",
			files:   oneWorkflow(postLandFile, "name: post-land\non: push\njobs:\n  gate:\n    runs-on: ci-base\n"),
			wantWhy: `has no job "check" (its jobs are "gate")`},
		{name: "a pull_request-only trigger",
			files:   oneWorkflow(postLandFile, "name: post-land\non: pull_request\njobs:\n  check:\n    runs-on: ci-base\n"),
			wantWhy: `is not triggered by push (on: "pull_request")`},
		{name: "a trigger list with no push",
			files:   oneWorkflow(postLandFile, "name: post-land\non: [pull_request]\njobs:\n  check:\n    runs-on: ci-base\n"),
			wantWhy: `is not triggered by push (on: "pull_request")`},
		{name: "a trigger map with no push",
			files:   oneWorkflow(postLandFile, "name: post-land\non:\n  pull_request:\njobs:\n  check:\n    runs-on: ci-base\n"),
			wantWhy: `is not triggered by push (on: "pull_request")`},
		{name: "no on: trigger at all",
			files:   oneWorkflow(postLandFile, "name: post-land\njobs:\n  check:\n    runs-on: ci-base\n"),
			wantWhy: "has no on: trigger"},
		{name: "a push: that is not a map of filters",
			files:   postLandWith("on:\n  push: main\n"),
			wantWhy: "not a map of filters"},
		{name: "a name: that is a list, not a string",
			files:   oneWorkflow("list.yml", "name: [post-land]\non: push\njobs:\n  check:\n    runs-on: ci-base\n"),
			wantWhy: "list.yml is not a readable workflow"},
		{name: "malformed YAML alone",
			files:   oneWorkflow("broken.yml", "name: post-land\non: [push\n"),
			wantWhy: "broken.yml is not a readable workflow"},
		{name: "malformed YAML next to a good file: the good file still posts",
			files: map[string][]byte{
				postLandFile: []byte("name: post-land\non: push\njobs:\n  check:\n    runs-on: ci-base\n"),
				"broken.yml": []byte("name: post-land\non: [push\n"),
			},
			want:    PostsOnEveryPush,
			wantWhy: "broken.yml is not a readable workflow"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, why := WorkflowsPostContext(tc.files, postLandContext)
			if verdict != tc.want {
				t.Fatalf("WorkflowsPostContext() verdict = %v, want %v (why = %q)", verdict, tc.want, why)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to name %q", why, tc.wantWhy)
			}
		})
	}
}

// TestWorkflowsPostContextBranchExclusion holds the Actions branch rules: a !
// pattern removes the branches it matches, a list of only ! patterns selects every
// other branch, and branches-ignore removes what it matches.
func TestWorkflowsPostContextBranchExclusion(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string][]byte
		want  PostVerdict
	}{
		{name: "a ** selection then !main leaves main out",
			files: postLandWith("on:\n  push:\n    branches: ['**', '!main']\n")},
		{name: "a list of only a ! pattern selects every branch but that one",
			files: postLandWith("on:\n  push:\n    branches: ['!release/**']\n"),
			want:  PostsOnEveryPush},
		{name: "main listed then removed by !main",
			files: postLandWith("on:\n  push:\n    branches: ['main', '!main']\n")},
		{name: "branches-ignore that names main",
			files: postLandWith("on:\n  push:\n    branches-ignore: [main]\n")},
		{name: "a ! pattern placed before main still leaves main selected",
			files: postLandWith("on:\n  push:\n    branches: ['!main', 'main']\n"),
			want:  PostsOnEveryPush},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, why := WorkflowsPostContext(tc.files, postLandContext)
			if verdict != tc.want {
				t.Fatalf("WorkflowsPostContext() verdict = %v, want %v (why = %q)", verdict, tc.want, why)
			}
		})
	}
}

// TestWorkflowsPostContextPathsFilter holds a paths or paths-ignore filter on push:
// the context is posted only for commits touching the filtered paths, so the
// verdict is PostsForSomeCommits, and a second workflow that posts on every push
// outranks it.
func TestWorkflowsPostContextPathsFilter(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		files   map[string][]byte
		want    PostVerdict
		wantWhy string
	}{
		{name: "paths: docs/**",
			files:   postLandWith("on:\n  push:\n    paths: ['docs/**']\n"),
			want:    PostsForSomeCommits,
			wantWhy: "only for commits touching the filtered paths"},
		{name: "paths-ignore: docs/**",
			files:   postLandWith("on:\n  push:\n    paths-ignore: ['docs/**']\n"),
			want:    PostsForSomeCommits,
			wantWhy: "only for commits touching the filtered paths"},
		{name: "a paths filter on a branch that leaves out main",
			files:   postLandWith("on:\n  push:\n    branches: [dev]\n    paths: ['docs/**']\n"),
			wantWhy: `runs on push to branches "dev", not main`},
		{name: "a paths-filtered workflow beside one that posts on every push",
			files: map[string][]byte{
				".forgejo/workflows/a.yml": []byte("name: post-land\non:\n  push:\n    paths: ['docs/**']\njobs:\n  check:\n    runs-on: ci-base\n"),
				".forgejo/workflows/b.yml": []byte("name: post-land\non: push\njobs:\n  check:\n    runs-on: ci-base\n"),
			},
			want:    PostsOnEveryPush,
			wantWhy: "b.yml posts"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, why := WorkflowsPostContext(tc.files, postLandContext)
			if verdict != tc.want {
				t.Fatalf("WorkflowsPostContext() verdict = %v, want %v (why = %q)", verdict, tc.want, why)
			}
			if !strings.Contains(why, tc.wantWhy) {
				t.Errorf("why = %q, want it to name %q", why, tc.wantWhy)
			}
		})
	}
}

// TestWorkflowsPostContextEmptyTrigger holds an on: with no events. It fires on no
// push, and why names it as an empty trigger rather than printing the empty value.
func TestWorkflowsPostContextEmptyTrigger(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		files map[string][]byte
	}{
		{name: "a bare on:",
			files: postLandWith("on:\n")},
		{name: "on: null",
			files: postLandWith("on: null\n")},
		{name: "on: []",
			files: postLandWith("on: []\n")},
		{name: "on: {}",
			files: postLandWith("on: {}\n")},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			verdict, why := WorkflowsPostContext(tc.files, postLandContext)
			if verdict != DoesNotPost {
				t.Fatalf("WorkflowsPostContext() verdict = %v, want DoesNotPost (why = %q)", verdict, why)
			}
			if !strings.Contains(why, "the `on:` trigger is empty") {
				t.Errorf("why = %q, want it to say the on: trigger is empty", why)
			}
			if strings.Contains(why, `on: ""`) {
				t.Errorf("why = %q, want no empty-string rendering of on:", why)
			}
		})
	}
}

// TestWorkflowsPostContextRejectsAMalformedContext drives the refusal for a context
// that is not "<name> / <job> (push)": no workflow can post it, and why says so.
func TestWorkflowsPostContextRejectsAMalformedContext(t *testing.T) {
	t.Parallel()
	files := postLandWith("on: push\n")
	for _, context := range []string{"post-land / check", "post-land check (push)", "(push)"} {
		t.Run(context, func(t *testing.T) {
			t.Parallel()
			verdict, why := WorkflowsPostContext(files, context)
			if verdict != DoesNotPost {
				t.Fatalf("WorkflowsPostContext(%q) verdict = %v, want a refusal", context, verdict)
			}
			if !strings.Contains(why, "is not a status context") {
				t.Errorf("why = %q, want it to say the context is not a status context", why)
			}
		})
	}
}
