package session

import (
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// dockerfileText reads deploy/polecat/Dockerfile relative to this source file.
func dockerfileText(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "polecat", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// dockerfileInstructions returns the polecat Dockerfile's instructions with
// continuation lines joined, one string each.
func dockerfileInstructions(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(strings.ReplaceAll(dockerfileText(t), "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// dockerfileWorkdir returns the image's working directory: the workspace the
// policy's include_workdir grant makes writable for a sandbox.
func dockerfileWorkdir(t *testing.T) string {
	t.Helper()
	var workdir string
	for _, in := range dockerfileInstructions(t) {
		if v, ok := strings.CutPrefix(in, "WORKDIR "); ok {
			workdir = strings.TrimSpace(v)
		}
	}
	if workdir == "" {
		t.Fatal("deploy/polecat/Dockerfile sets no WORKDIR")
	}
	return workdir
}

// imageGoCacheProblems reports what a Dockerfile builds into the image that
// would carry Go build or module cache files (gt-nly1g):
//
//   - GOCACHE or GOMODCACHE in the image environment, whichever path it names:
//     the sandbox's profile decides the caches, and an image value shadows it;
//   - an instruction that copies the staged seed sources into the image, or
//     bind-mounts them so its RUN writes a cache;
//   - a RUN that runs the go tool and leaves a cache directory it wrote. A
//     layer that keeps those files is in the image, whatever the instruction is
//     spelled like, so the check reads each RUN's commands instead of looking
//     for one RUN's text.
func imageGoCacheProblems(dockerfile string) []string {
	var problems []string
	var instructions []string
	for _, line := range strings.Split(strings.ReplaceAll(dockerfile, "\\\n", " "), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			instructions = append(instructions, line)
		}
	}
	for _, in := range instructions {
		if rest, ok := strings.CutPrefix(in, "ENV "); ok {
			for _, kv := range strings.Fields(rest) {
				if k, _, ok := strings.Cut(kv, "="); ok && (k == "GOCACHE" || k == "GOMODCACHE") {
					problems = append(problems, "the image environment sets "+k+": the sandbox profile sets the Go caches, so the image must set neither")
				}
			}
		}
		if copiedSeedSources(in) {
			problems = append(problems, "the instruction brings the staged seed sources into the image: "+in)
		}
		if rest, ok := strings.CutPrefix(in, "RUN "); ok {
			problems = append(problems, runGoCacheProblems(rest)...)
		}
	}
	return problems
}

// copiedSeedSources reports whether an instruction copies seed/ from the build
// context or bind-mounts it, either of which would let the image build a cache
// from the staged sources.
func copiedSeedSources(in string) bool {
	if mount, ok := strings.CutPrefix(in, "RUN "); ok {
		for _, field := range strings.Fields(mount) {
			if !strings.Contains(field, "--mount=") {
				continue
			}
			for _, opt := range strings.Split(field, ",") {
				if opt == "source=seed" {
					return true
				}
			}
		}
		return false
	}
	for _, cmd := range []string{"COPY ", "ADD "} {
		if rest, ok := strings.CutPrefix(in, cmd); ok {
			fields := strings.Fields(rest)
			for _, src := range fields[:len(fields)-1] {
				if src == "seed" || strings.HasPrefix(src, "seed/") {
					return true
				}
			}
		}
	}
	return false
}

// runGoCacheProblems reports the cache directories a RUN runs the go tool in
// and does not remove in that same instruction. A later RUN that deletes them
// would only add a layer on top, so the files still ship.
func runGoCacheProblems(script string) []string {
	if !runsGo(script) {
		return nil
	}
	// Where go writes: the directories the instruction names, or the image
	// user's defaults (/root/.cache and /root/go for root).
	caches := map[string]bool{"/root/.cache": true, "/root/go": true}
	commands := shellCommands(script)
	for _, words := range commands {
		for _, word := range words {
			for _, key := range []string{"GOCACHE=", "GOMODCACHE="} {
				if v, ok := strings.CutPrefix(word, key); ok && v != "" {
					caches[v] = true
				}
			}
		}
	}
	var problems []string
	for _, cache := range slices.Sorted(maps.Keys(caches)) {
		if !removed(commands, cache) {
			problems = append(problems, "a RUN runs the go tool and leaves "+cache+" in the image: "+script)
		}
	}
	return problems
}

// runsGo reports whether the shell runs the go tool in command position, so
// that an argument or a path that merely names go does not count.
func runsGo(script string) bool {
	for _, words := range shellCommands(script) {
		if words = commandWords(words); len(words) > 0 && filepath.Base(words[0]) == "go" {
			return true
		}
	}
	return false
}

// removed reports whether the shell removes dir: an rm whose own arguments name
// it, in any command of the same instruction.
func removed(commands [][]string, dir string) bool {
	for _, words := range commands {
		if words = commandWords(words); len(words) == 0 || filepath.Base(words[0]) != "rm" {
			continue
		}
		for _, arg := range words[1:] {
			if arg == dir || strings.HasPrefix(arg, dir+"/") {
				return true
			}
		}
	}
	return false
}

// shellCommands parses a RUN's shell into the commands it runs, each as its
// words with the quotes dropped. A separator (&&, ; or a newline) ends a
// command unless it sits inside a quote, so a script a shell is handed with -c
// stays one word and is parsed as a script of its own: a go call behind sh -c
// or bash -lc runs like any other.
func shellCommands(script string) [][]string {
	var commands [][]string
	for _, seg := range shellSegments(script) {
		words := shellWords(seg)
		if len(words) == 0 {
			continue
		}
		commands = append(commands, words)
		if inner, ok := shellScript(words); ok {
			commands = append(commands, shellCommands(inner)...)
		}
	}
	return commands
}

// shellSegments splits a shell script at its command boundaries, leaving a
// separator inside quotes in the word it belongs to.
func shellSegments(script string) []string {
	var out []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if seg := strings.TrimSpace(cur.String()); seg != "" {
			out = append(out, seg)
		}
		cur.Reset()
	}
	for _, r := range script {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == '&' || r == ';' || r == '\n':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return out
}

// shellWords splits a command segment into its words, dropping the quotes
// around a quoted word so the words read as the shell expands them and keeping
// the spaces inside one.
func shellWords(seg string) []string {
	var words []string
	var cur strings.Builder
	var quote rune
	flush := func() {
		if cur.Len() > 0 {
			words = append(words, cur.String())
			cur.Reset()
		}
	}
	for _, r := range seg {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return words
}

// shellScript returns the script a shell command runs with -c, as in
// bash -lc 'go build ./...', so its commands can be read like any other.
func shellScript(words []string) (string, bool) {
	if len(words) < 3 || !shells[filepath.Base(words[0])] {
		return "", false
	}
	for i := 1; i < len(words)-1; i++ {
		if !strings.HasPrefix(words[i], "-") || strings.HasPrefix(words[i], "--") {
			return "", false
		}
		if strings.HasSuffix(words[i], "c") {
			return words[i+1], true
		}
	}
	return "", false
}

// shells are the interpreters whose -c script runs another command.
var shells = map[string]bool{"sh": true, "bash": true, "dash": true, "zsh": true, "ash": true, "ksh": true}

// shellWrappers are the words a shell may put in front of a command without
// changing which program runs, so go after export, env, command or exec counts.
var shellWrappers = map[string]bool{"export": true, "env": true, "command": true, "exec": true, "time": true}

// commandWords is a command's words with its leading variable assignments and
// wrappers removed, leaving the program the command runs first.
func commandWords(words []string) []string {
	for len(words) > 0 && (strings.Contains(words[0], "=") || shellWrappers[words[0]]) {
		words = words[1:]
	}
	return words
}

// The image carries no Go build or module cache (gt-nly1g). A sandbox's entries
// live on the shared cache claim `make polecat-caches` fills, or, with no claim,
// in the sandbox's own layer; the backend sets GOCACHE and GOMODCACHE. Caches
// baked into the image would cost every node gigabytes and go stale on every
// rebuild.
func TestPolecatImageCarriesNoGoCaches(t *testing.T) {
	t.Parallel()
	for _, problem := range imageGoCacheProblems(dockerfileText(t)) {
		t.Error(problem)
	}
}

// The check must go red on each way a cache gets into the image: an ENV cache
// path, a bind-mounted or copied seed, and a go command whose cache files stay
// in the layer. Without these the test would pass over a Dockerfile that seeds.
func TestImageGoCacheProblemsRejectsACacheTheImageWouldCarry(t *testing.T) {
	t.Parallel()
	const installs = "RUN GOBIN=/usr/local/bin /usr/local/go/bin/go install example.invalid/tool@v1 && rm -rf /root/go /root/.cache\n"
	cases := []struct {
		name       string
		dockerfile string
		want       string // text the problem must name; "" wants no problem
	}{
		{"a go install that removes its caches", installs, ""},
		{"the environment names a build cache", "ENV GOCACHE=/tmp/gocache\n" + installs, "GOCACHE"},
		{"the environment names a module cache", "ENV GOFLAGS= GOMODCACHE=/tmp/gomodcache\n" + installs, "GOMODCACHE"},
		{"a seed bind mount", "RUN --mount=type=bind,source=seed,target=/seed go build ./...\n", "seed sources"},
		{"a copied seed directory", "COPY seed /seed\n", "seed sources"},
		{"a go build that keeps its build cache", "RUN cd /src && go build ./...\n", "/root/.cache"},
		{"a go build that keeps its module cache", "RUN cd /src && go build ./...\n", "/root/go"},
		{"a cache named inside the RUN", "RUN GOCACHE=/gocache go build ./... && rm -rf /root/go /root/.cache\n", "/gocache"},
		{"a later RUN does not clean another's layer", "RUN go install example.invalid/tool@v1\nRUN rm -rf /root/go /root/.cache\n", "/root/.cache"},
		{"a go build behind a shell's -c", "RUN bash -lc 'go build ./...'\n", "/root/.cache"},
		{"a go build after an export", "RUN export PATH=\"/usr/local/go/bin:$PATH\" && go build ./...\n", "/root/.cache"},
		{"a go build behind env", "RUN env GOFLAGS= go build ./...\n", "/root/.cache"},
		{"a go build behind command", "RUN command go install example.invalid/tool@v1\n", "/root/go"},
		{"a cache inside the shell's script", "RUN sh -c 'GOCACHE=/gocache go build ./...; rm -rf /root/go /root/.cache'\n", "/gocache"},
		{"go only named in an argument", "RUN bash -c 'echo go build ./...'\n", ""},
		{"go only named in a printf", "RUN printf 'go build ./...'\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			problems := imageGoCacheProblems(tc.dockerfile)
			if tc.want == "" {
				if len(problems) != 0 {
					t.Errorf("imageGoCacheProblems = %v, want none", problems)
				}
				return
			}
			for _, problem := range problems {
				if strings.Contains(problem, tc.want) {
					return
				}
			}
			t.Errorf("imageGoCacheProblems = %v, want a problem naming %s", problems, tc.want)
		})
	}
}
