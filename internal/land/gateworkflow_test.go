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

// gitGlobalValueFlags are git's global options that take their value as the
// next word, so that word is not read as the subcommand. The =form of an option
// carries its value in the same word and needs no entry.
var gitGlobalValueFlags = map[string]bool{
	"-C": true, "-c": true, "--git-dir": true, "--work-tree": true, "--namespace": true,
	"--config-env": true,
}

// gitFetchWords returns the words after each git fetch in a run script. git's
// global options (-C dir, --no-pager, ...) may come before fetch, and a fetch
// may sit in a subshell, a command substitution, or backticks.
func gitFetchWords(run string) [][]string {
	var fetches [][]string
	for _, cmd := range shellCommands(run) {
		for i, w := range cmd {
			if w != "git" {
				continue
			}
			j := i + 1
			for j < len(cmd) && strings.HasPrefix(cmd[j], "-") {
				if gitGlobalValueFlags[cmd[j]] {
					j++
				}
				j++
			}
			if j < len(cmd) && cmd[j] == "fetch" {
				fetches = append(fetches, cmd[j+1:])
			}
		}
	}
	return fetches
}

// shellCommands splits a run script into commands, each a list of words. A
// backslash at a line end continues the line. Whitespace separates words, and
// a newline, && || ; | & ( ) or a backtick ends a command. Quotes group a word
// and are dropped, but a double-quoted $( or backtick still opens a command,
// and a quote opened inside it nests. A backslash escapes the next character,
// or inside double quotes only " \ $ and a backtick. A quote left open at the
// end of the run takes the rest of the run as its word.
func shellCommands(run string) [][]string {
	run = strings.ReplaceAll(run, "\\\n", " ")
	var cmds [][]string
	var words []string
	var word strings.Builder
	inWord := false
	// open holds the frames the scanner is inside: a single quote, a double
	// quote, a ( group or $( substitution, or a backtick substitution.
	var open []rune
	top := func() rune {
		if len(open) == 0 {
			return 0
		}
		return open[len(open)-1]
	}
	pop := func() { open = open[:len(open)-1] }
	endWord := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	endCmd := func() {
		endWord()
		if len(words) > 0 {
			cmds = append(cmds, words)
		}
		words = nil
	}
	rs := []rune(run)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch top() {
		case '\'':
			if r == '\'' {
				pop()
			} else {
				word.WriteRune(r)
			}
			continue
		case '"':
			switch {
			case r == '\\' && i+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[i+1]):
				i++
				word.WriteRune(rs[i])
			case r == '"':
				pop()
			case r == '(' && i > 0 && rs[i-1] == '$':
				open = append(open, '(')
				endCmd()
			case r == '`':
				open = append(open, '`')
				endCmd()
			default:
				word.WriteRune(r)
			}
			continue
		}
		switch {
		case r == '\\' && i+1 < len(rs):
			i++
			word.WriteRune(rs[i])
			inWord = true
		case r == '\'' || r == '"':
			open = append(open, r)
			inWord = true
		case r == '`':
			if top() == '`' {
				pop()
			} else {
				open = append(open, '`')
			}
			endCmd()
		case r == '(':
			open = append(open, '(')
			endCmd()
		case r == ')':
			if top() == '(' {
				pop()
			}
			endCmd()
		case r == ' ' || r == '\t' || r == '\r':
			endWord()
		case r == '\n' || r == ';' || r == '&' || r == '|':
			endCmd()
		default:
			word.WriteRune(r)
			inWord = true
		}
	}
	endCmd()
	return cmds
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
		{name: "a -C path before the subcommand does not hide --all",
			steps: []gateStep{{Run: "git -C repo fetch --all"}}, wantProblems: 1},
		{name: "a quoted -C path with a space does not hide --all",
			steps: []gateStep{{Run: `git -C "my repo" fetch --all`}}, wantProblems: 1},
		{name: "a -c config before the subcommand does not hide a bare fetch",
			steps: []gateStep{{Run: "git -c protocol.version=2 fetch origin"}}, wantProblems: 1},
		{name: "a --no-pager before the subcommand does not hide --all",
			steps: []gateStep{{Run: "git --no-pager fetch --all"}}, wantProblems: 1},
		{name: "a --git-dir before the subcommand does not hide --all",
			steps: []gateStep{{Run: "git --git-dir=.git fetch --all"}}, wantProblems: 1},
		{name: "a fetch in a subshell is read",
			steps: []gateStep{{Run: "(git fetch --all)"}}, wantProblems: 1},
		{name: "a fetch in a command substitution is read",
			steps: []gateStep{{Run: "x=$(git fetch --all)"}}, wantProblems: 1},
		{name: "a fetch in backticks is read",
			steps: []gateStep{{Run: "echo `git fetch --all`"}}, wantProblems: 1},
		{name: "a fetch in a double-quoted command substitution is read",
			steps: []gateStep{{Run: `x="$(git fetch --all)"`}}, wantProblems: 1},
		{name: "a -C path with a space in a double-quoted substitution does not hide --all",
			steps: []gateStep{{Run: `x="$(git -C "my repo" fetch --all)"`}}, wantProblems: 1},
		{name: "a fetch in double-quoted backticks is read",
			steps: []gateStep{{Run: "echo \"`git fetch --all`\""}}, wantProblems: 1},
		{name: "an escaped quote does not hide a fetch after the quoted word",
			steps: []gateStep{{Run: `echo "a\"" && git fetch --all`}}, wantProblems: 1},
		{name: "a fetch named in single quotes is text, not a command",
			steps: []gateStep{{Run: "echo 'git fetch --all'"}}},
		{name: "a fetch named in double quotes is text, not a command",
			steps: []gateStep{{Run: `echo "git fetch --all"`}}},
		{name: "a fetch after a background operator is read",
			steps: []gateStep{{Run: "make gate & git fetch --all"}}, wantProblems: 1},
		{name: "a -C path with a named branch is allowed",
			steps: []gateStep{{Run: "git -C repo fetch origin main"}}},
		{name: "a -C path with a status command is not a fetch",
			steps: []gateStep{{Run: "git -C repo status --all"}}},
		{name: "the deepening fetch with a -C path still deepens main",
			steps:       []gateStep{{Run: `git -C . fetch --no-tags --prune --depth=2147483647 origin "+refs/heads/main:refs/remotes/origin/main"`}},
			wantDeepens: true},
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
