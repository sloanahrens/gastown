package land

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
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

// The fetch that deepens origin/main with full history: the refspec and the
// depth the gate's workflow passes to git fetch.
const (
	gateDeepenMainRef = "+refs/heads/main:refs/remotes/origin/main"
	gateDeepenDepth   = "--depth=2147483647"
)

// fetchValueFlags are git fetch's flags that take their value as the next
// word, so that word is not read as the remote or a refspec.
var fetchValueFlags = map[string]bool{
	"--depth": true, "--deepen": true, "--shallow-since": true, "--shallow-exclude": true,
	"--jobs": true, "-j": true, "--negotiation-tip": true, "--server-option": true, "-o": true,
}

// TestGateWorkflowFetchesMainNotAllBranches holds the committed gate workflow's
// fetch to the refs the gate reads: the candidate and main with history, and no
// other head. git describe needs the candidate's history back to a tag, which a
// depth-1 checkout does not have (gt-ebco0).
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
	problems, deepensMain := gateFetchProblems(job.Steps)
	for _, p := range problems {
		t.Error(p)
	}
	if !deepensMain {
		t.Fatal("no step fetches origin/main with full history, which git describe and the landing's ancestry reads need")
	}
}

// gateFetchProblems names each fetch a gate step makes that takes every branch,
// and each checkout that fetches every head. It also reports whether a step
// deepens origin/main. A fetch is read word by word, not matched as text, so a
// bare `git fetch` (the remote's default refspec), --all, or a refs/heads glob
// with or without its leading + is caught.
func gateFetchProblems(steps []gateStep) (problems []string, deepensMain bool) {
	for _, step := range steps {
		if depth, set := step.With["fetch-depth"]; set && fmt.Sprint(depth) == "0" {
			problems = append(problems, fmt.Sprintf("the %s checkout fetches every head (fetch-depth: 0)", step.Uses))
		}
		for _, words := range gitFetchWords(step.Run) {
			if fetchesEveryHead(words) {
				problems = append(problems, fmt.Sprintf("the step fetching %q creates a ref for every branch", strings.Join(words, " ")))
			}
			if slices.Contains(words, gateDeepenMainRef) && slices.Contains(words, gateDeepenDepth) {
				deepensMain = true
			}
		}
	}
	return problems, deepensMain
}

// gitFetchWords returns the words after each git fetch in a run script. A
// backslash at a line end continues the line, and && || ; | end a command.
// Quotes are dropped from each word; the refspecs here hold no spaces.
func gitFetchWords(run string) [][]string {
	run = strings.ReplaceAll(run, "\\\n", " ")
	for _, sep := range []string{"&&", "||", ";", "|"} {
		run = strings.ReplaceAll(run, sep, "\n")
	}
	var fetches [][]string
	for _, line := range strings.Split(run, "\n") {
		words := strings.Fields(line)
		for i := range words {
			words[i] = strings.Trim(words[i], `"'`)
		}
		for i := 1; i < len(words); i++ {
			if words[i-1] == "git" && words[i] == "fetch" {
				fetches = append(fetches, words[i+1:])
				break
			}
		}
	}
	return fetches
}

// fetchesEveryHead reports whether a git fetch's words take a ref for every
// branch of a remote: --all or --mirror, no refspec at all (the remote's default
// refspec), or a refspec whose source is * or a refs/heads/ glob.
func fetchesEveryHead(words []string) bool {
	var positional []string
	for i := 0; i < len(words); i++ {
		w := words[i]
		switch {
		case w == "--all" || w == "--mirror":
			return true
		case fetchValueFlags[w]:
			i++
		case strings.HasPrefix(w, "-"):
			// a flag with its value in the same word
		default:
			positional = append(positional, w)
		}
	}
	// The first positional is the remote; the rest are refspecs.
	if len(positional) < 2 {
		return true
	}
	for _, spec := range positional[1:] {
		src, _, _ := strings.Cut(strings.TrimPrefix(spec, "+"), ":")
		if src == "*" || (strings.Contains(src, "refs/heads/") && strings.Contains(src, "*")) {
			return true
		}
	}
	return false
}

// TestGateFetchProblemsNamesEveryHeadFetch holds the guard to the fetches it
// must refuse and the one shape it must allow. The bare fetch and --all have no
// glob in their text, so a guard that matches text passes them (gt-xrovw).
func TestGateFetchProblemsNamesEveryHeadFetch(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name         string
		steps        []gateStep
		wantProblems int
		wantDeepens  bool
	}{
		{name: "the gate's own deepening fetch",
			steps:       []gateStep{{Run: "git fetch --no-tags --prune --depth=2147483647 origin \"+refs/heads/main:refs/remotes/origin/main\""}},
			wantDeepens: true},
		{name: "a bare fetch takes the remote's default refspec",
			steps: []gateStep{{Run: "git fetch origin"}}, wantProblems: 1},
		{name: "a fetch with no remote",
			steps: []gateStep{{Run: "git fetch"}}, wantProblems: 1},
		{name: "--all takes every remote",
			steps: []gateStep{{Run: "git fetch --all"}}, wantProblems: 1},
		{name: "a value flag before the remote does not name a refspec",
			steps: []gateStep{{Run: "git fetch --depth 50 origin"}}, wantProblems: 1},
		{name: "a glob over every head",
			steps: []gateStep{{Run: `git fetch origin "+refs/heads/*:refs/remotes/origin/*"`}}, wantProblems: 1},
		{name: "a heads glob without the plus",
			steps: []gateStep{{Run: "git fetch origin refs/heads/*"}}, wantProblems: 1},
		{name: "a continued fetch of every head",
			steps: []gateStep{{Run: "git fetch --no-tags origin \\\n  \"+refs/heads/*:refs/remotes/origin/*\""}}, wantProblems: 1},
		{name: "a fetch after a separator is read",
			steps: []gateStep{{Run: "make gate && git fetch origin"}}, wantProblems: 1},
		{name: "a checkout with every head",
			steps: []gateStep{{Uses: "actions/checkout@v4", With: map[string]any{"fetch-depth": 0}}}, wantProblems: 1},
		{name: "a tag glob is not a head",
			steps: []gateStep{{Run: `git fetch --no-tags --prune origin "+refs/tags/*:refs/tags/*"`}}},
		{name: "a fetch of one named branch",
			steps: []gateStep{{Run: "git fetch origin main"}}},
		{name: "a run with no fetch",
			steps: []gateStep{{Run: "make gate"}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems, deepens := gateFetchProblems(tc.steps)
			if len(problems) != tc.wantProblems {
				t.Errorf("problems = %q, want %d", problems, tc.wantProblems)
			}
			if deepens != tc.wantDeepens {
				t.Errorf("deepensMain = %v, want %v", deepens, tc.wantDeepens)
			}
		})
	}
}
