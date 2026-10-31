package session

import (
	"os"
	"path/filepath"
	"runtime"
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

// dockerfileEnv returns the image environment as the Dockerfile's last
// assignment of each key.
func dockerfileEnv(t *testing.T) map[string]string {
	t.Helper()
	env := map[string]string{}
	for _, in := range dockerfileInstructions(t) {
		rest, ok := strings.CutPrefix(in, "ENV ")
		if !ok {
			continue
		}
		for _, kv := range strings.Fields(rest) {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				continue
			}
			env[k] = strings.Trim(v, `"`)
		}
	}
	return env
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
	scripts := splitShellAnd(script)
	for _, seg := range scripts {
		for _, field := range strings.Fields(seg) {
			for _, key := range []string{"GOCACHE=", "GOMODCACHE="} {
				if v, ok := strings.CutPrefix(field, key); ok && v != "" {
					caches[strings.Trim(v, `"`)] = true
				}
			}
		}
	}
	var problems []string
	for cache := range caches {
		if !removed(scripts, cache) {
			problems = append(problems, "a RUN runs the go tool and leaves "+cache+" in the image: "+script)
		}
	}
	return problems
}

// runsGo reports whether the shell runs the go tool in command position, so
// that an argument or a path that merely names go does not count.
func runsGo(script string) bool {
	for _, seg := range splitShellAnd(script) {
		fields := strings.Fields(seg)
		for len(fields) > 0 && strings.Contains(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) > 0 && filepath.Base(fields[0]) == "go" {
			return true
		}
	}
	return false
}

// removed reports whether the shell removes dir: an rm whose own arguments name
// it, in any segment of the same instruction.
func removed(segments []string, dir string) bool {
	for _, seg := range segments {
		fields := strings.Fields(seg)
		if len(fields) == 0 || filepath.Base(fields[0]) != "rm" {
			continue
		}
		for _, arg := range fields[1:] {
			if arg == dir || strings.HasPrefix(arg, dir+"/") {
				return true
			}
		}
	}
	return false
}

// splitShellAnd splits a RUN's shell on && and ;, the boundaries at which one
// command ends and the next starts.
func splitShellAnd(script string) []string {
	var out []string
	for _, part := range strings.Split(script, "&&") {
		for _, seg := range strings.Split(part, ";") {
			if seg = strings.TrimSpace(seg); seg != "" {
				out = append(out, seg)
			}
		}
	}
	return out
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
