package daemonimage

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/templates"
)

// imageBuilder is a Builder ready to build the image: the repo's Dockerfile, a
// fresh directory holding the four binaries as the build context, and run.
func imageBuilder(t *testing.T) (Builder, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{}
	b := validBuilder(run, t.TempDir())
	b.Dockerfile = repoFile(t, DockerfilePath)
	for _, name := range imageBinaryNames {
		if err := os.WriteFile(filepath.Join(b.Out, name), []byte("binary"), 0o600); err != nil {
			t.Fatalf("writing the %s binary: %v", name, err)
		}
	}
	return b, run
}

// imageBinaryNames is the binaries the image copies from its build context.
var imageBinaryNames = []string{"gt", "bd", "om", "claude"}

// imageBinaries is the list of the image's binaries in b.Out, in the order
// imageBinaryNames names them.
func imageBinaries(b Builder) []string {
	out := make([]string, 0, len(imageBinaryNames))
	for _, name := range imageBinaryNames {
		out = append(out, filepath.Join(b.Out, name))
	}
	return out
}

// repoFile returns the absolute path of a file of the repo this package lives
// in, named relative to its root. The root is the nearest directory above the
// package that holds this module's go.mod, so the path does not depend on how
// many directories deep the package sits.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("finding the package directory: %v", err)
	}
	for {
		body, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && strings.HasPrefix(string(body), "module github.com/steveyegge/gastown\n") {
			return filepath.Join(dir, rel)
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod for github.com/steveyegge/gastown above %s", dir)
		}
		dir = parent
	}
}

// TestBuildImageRecordsTheDockerBuild: the image is built by one docker call in
// the pinned context, from the named Dockerfile, with the binaries' directory as
// the context, and its tag is gt-daemon:<rev>.
func TestBuildImageRecordsTheDockerBuild(t *testing.T) {
	t.Parallel()
	b, run := imageBuilder(t)
	tag, err := b.BuildImage(context.Background(), testRev, imageBinaries(b))
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	if want := "gt-daemon:" + testRev; tag != want {
		t.Errorf("BuildImage tag = %q, want %q", tag, want)
	}
	if len(run.calls) != 1 {
		t.Fatalf("recorded %d calls, want the one docker build", len(run.calls))
	}
	want := []string{"docker", "--context", "desktop-linux", "build", "-f", b.Dockerfile, "-t", tag, b.Out}
	if got := run.calls[0].argv; !reflect.DeepEqual(got, want) {
		t.Errorf("build argv = %q, want %q", got, want)
	}
	if run.calls[0].dir != b.Out {
		t.Errorf("the build ran in %q, want the build context %q", run.calls[0].dir, b.Out)
	}
}

// TestBuildImageFailsWithAnEmptyTag: a docker build that fails returns its error
// and no tag, so no caller can deploy an image that was never made.
func TestBuildImageFailsWithAnEmptyTag(t *testing.T) {
	t.Parallel()
	b, run := imageBuilder(t)
	run.failDocker = true
	tag, err := b.BuildImage(context.Background(), testRev, imageBinaries(b))
	if err == nil {
		t.Fatalf("a failed docker build returned the tag %q and no error", tag)
	}
	if tag != "" {
		t.Errorf("a failed docker build returned the tag %q, want none", tag)
	}
	if !strings.Contains(err.Error(), "gt-daemon:"+testRev) {
		t.Errorf("the error %q does not name the image it could not build", err)
	}
}

// TestBuildImageRefusesBeforeDockerRuns covers every refusal: each comes before
// the docker call, so a refused build starts nothing and returns no tag.
func TestBuildImageRefusesBeforeDockerRuns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rev     string
		breakIt func(*testing.T, *Builder, *[]string)
		want    string
	}{
		{name: "no runner", rev: testRev, breakIt: func(_ *testing.T, b *Builder, _ *[]string) { b.Run = nil }, want: "no runner"},
		{name: "no rev", rev: " ", breakIt: func(*testing.T, *Builder, *[]string) {}, want: "no rev"},
		{name: "a rev that is an option", rev: "--upload-pack=sh", breakIt: func(*testing.T, *Builder, *[]string) {}, want: `rev "--upload-pack=sh" is not`},
		{name: "no build context", rev: testRev, breakIt: func(_ *testing.T, b *Builder, _ *[]string) { b.Out = "" }, want: "no build context"},
		{name: "a relative build context", rev: testRev, breakIt: func(_ *testing.T, b *Builder, _ *[]string) { b.Out = filepath.Join("rel", "out") }, want: `build context "rel/out" is not absolute`},
		{name: "no Dockerfile", rev: testRev, breakIt: func(_ *testing.T, b *Builder, _ *[]string) { b.Dockerfile = "" }, want: "no Dockerfile named"},
		{name: "a relative Dockerfile", rev: testRev, breakIt: func(_ *testing.T, b *Builder, _ *[]string) { b.Dockerfile = DockerfilePath }, want: "is not absolute"},
		{name: "a Dockerfile that is not on disk", rev: testRev, breakIt: func(t *testing.T, b *Builder, _ *[]string) {
			b.Dockerfile = filepath.Join(t.TempDir(), "Dockerfile")
		}, want: "is not a file"},
		{name: "a Dockerfile that is a directory", rev: testRev, breakIt: func(t *testing.T, b *Builder, _ *[]string) {
			b.Dockerfile = t.TempDir()
		}, want: "is not a file"},
		{name: "a binary the image does not copy", rev: testRev, breakIt: func(_ *testing.T, b *Builder, bins *[]string) {
			(*bins)[2] = filepath.Join(b.Out, "om-extra")
		}, want: "is not one of the image's binaries"},
		{name: "a binary outside the build context", rev: testRev, breakIt: func(t *testing.T, _ *Builder, bins *[]string) {
			(*bins)[1] = filepath.Join(t.TempDir(), "bd")
		}, want: "is outside the build context"},
		{name: "a binary listed twice", rev: testRev, breakIt: func(_ *testing.T, _ *Builder, bins *[]string) {
			(*bins)[1] = (*bins)[0]
		}, want: "listed twice"},
		{name: "a binary that is not on disk", rev: testRev, breakIt: func(t *testing.T, _ *Builder, bins *[]string) {
			if err := os.Remove((*bins)[0]); err != nil {
				t.Fatalf("removing the gt binary: %v", err)
			}
		}, want: "is not a file in"},
		{name: "a binary missing from the list", rev: testRev, breakIt: func(_ *testing.T, _ *Builder, bins *[]string) {
			*bins = (*bins)[:2]
		}, want: "no om binary"},
		{name: "the claude CLI missing from the list", rev: testRev, breakIt: func(_ *testing.T, _ *Builder, bins *[]string) {
			*bins = (*bins)[:3]
		}, want: "no claude binary"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			b, run := imageBuilder(t)
			bins := imageBinaries(b)
			tc.breakIt(t, &b, &bins)
			tag, err := b.BuildImage(context.Background(), tc.rev, bins)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("BuildImage = %q, %v; want an error naming %q", tag, err, tc.want)
			}
			if tag != "" {
				t.Errorf("a refused image listed the tag %q, want none", tag)
			}
			if len(run.calls) != 0 {
				t.Errorf("a refused image ran %d docker calls, want none", len(run.calls))
			}
		})
	}
}

// TestBuildImageRunsWithTheDockerEnvironment: the build gets the variables that
// find docker and its config, and none of the compile settings or a shell's
// choice of engine.
func TestBuildImageRunsWithTheDockerEnvironment(t *testing.T) {
	t.Parallel()
	b, run := imageBuilder(t)
	b.Environ = append(append([]string(nil), testEnv...),
		"GOCACHE=/cache/go", "DOCKER_HOST=tcp://elsewhere:2375", "DOCKER_CONFIG=/home/builder/.docker")
	if _, err := b.BuildImage(context.Background(), testRev, imageBinaries(b)); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	env := run.calls[0].env
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/builder", "DOCKER_CONFIG=/home/builder/.docker"} {
		if !contains(env, want) {
			t.Errorf("build env = %q, want %s carried through", env, want)
		}
	}
	for _, key := range []string{"GOFLAGS", "GOWORK", "GOCACHE", "DOCKER_HOST"} {
		if hasKey(env, key) {
			t.Errorf("build env = %q, want %s left out", env, key)
		}
	}
}

// TestDaemonDockerfileHasNoBuildStep: the image is config only. A compile or a
// build tool in it would mean the image held a build of its own rather than the
// binaries the deployer built and proved. The check reads instructions, not the
// file's text, so a comment naming a build tool is not a failure.
func TestDaemonDockerfileHasNoBuildStep(t *testing.T) {
	t.Parallel()
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		line := in.cmd + " " + strings.Join(in.args, " ")
		if strings.Contains(line, "go build") {
			t.Errorf("%s runs a go build in %q; the binaries are COPYed in", DockerfilePath, line)
		}
		for _, arg := range in.args {
			if arg == "make" {
				t.Errorf("%s runs make in %q; nothing is built inside the image", DockerfilePath, line)
			}
		}
	}
}

// TestDaemonDockerfileFetchesNothingAndInstallsOnlyThePackages: the image is
// config, so no instruction downloads anything over the network and the only RUN
// that is not a git identity command installs exactly ca-certificates, git and
// tmux. The Dolt client arrives by COPY from the official Dolt image, whose
// registry layers the daemon verifies by digest, rather than by a download inside
// a RUN that no checksum would guard.
func TestDaemonDockerfileFetchesNothingAndInstallsOnlyThePackages(t *testing.T) {
	t.Parallel()
	var installs []string
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		line := in.cmd + " " + strings.Join(in.args, " ")
		for _, banned := range []string{"curl", "wget", "http://", "https://", "git clone", "tar -", "install -m"} {
			if strings.Contains(line, banned) {
				t.Errorf("%s has %q in %q; the image fetches and installs nothing but the three packages", DockerfilePath, banned, line)
			}
		}
		if in.cmd != "RUN" {
			continue
		}
		if _, _, ok := gitConfigGlobal(in); ok {
			continue
		}
		installs = append(installs, line)
	}
	if len(installs) != 1 {
		t.Fatalf("%s has %d RUN instructions that are not a git identity command, want the one package install: %q", DockerfilePath, len(installs), installs)
	}
	got, problem := aptInstalls(installs[0])
	if problem != "" {
		t.Fatalf("%s: %s", DockerfilePath, problem)
	}
	if want := []string{"ca-certificates", "git", "tmux"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s installs %q, want %q", DockerfilePath, got, want)
	}
}

// TestDaemonDockerfileSetsTheGenericGitIdentity: the image carries the git
// identity dolt's preflight copies, because a container has no host identity to
// read. Both values are the daemon's, never a person's, each is set once, and
// both are set in the final stage, the one the container runs.
func TestDaemonDockerfileSetsTheGenericGitIdentity(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t, readDockerfile(t))
	lastStage := 0
	for _, in := range ins {
		if in.cmd == "FROM" {
			lastStage++
		}
	}
	want := map[string]string{"user.name": "gt-daemon", "user.email": "gt-daemon@gastown.local"}
	got := map[string]string{}
	stage := 0
	for _, in := range ins {
		if in.cmd == "FROM" {
			stage++
			continue
		}
		key, value, ok := gitConfigGlobal(in)
		if !ok {
			continue
		}
		if _, seen := got[key]; seen {
			t.Errorf("%s sets git %s twice; the image carries one value per key", DockerfilePath, key)
		}
		got[key] = value
		if stage != lastStage {
			t.Errorf("%s sets git %s in stage %d, want the final stage %d, the one the container runs", DockerfilePath, key, stage, lastStage)
		}
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s sets git %s to %q, want %q", DockerfilePath, key, got[key], value)
		}
	}
	for key := range got {
		if _, ok := want[key]; !ok {
			t.Errorf("%s sets git %s, want only user.name and user.email", DockerfilePath, key)
		}
	}
}

// gitConfigGlobal reads a `git config --global` RUN as the key it sets and its
// value, and reports whether the RUN is exactly that one command. A second
// command in the RUN — another config, an install — makes it read as no identity
// line, so nothing can do its work beside one.
func gitConfigGlobal(in dockerfileInstruction) (key, value string, ok bool) {
	if in.cmd != "RUN" || len(in.args) != 5 {
		return "", "", false
	}
	if !slices.Equal(in.args[:3], []string{"git", "config", "--global"}) {
		return "", "", false
	}
	return in.args[3], in.args[4], true
}

// TestDaemonDockerfileCopiesOnlyTheBinariesAndDolt: every COPY is one of two
// kinds. The Dolt client is copied from the dolt stage, and nothing else is.
// Each of gt, bd, om and claude is copied from the build context by its bare
// name, once each, and no other file is. A COPY from any other source, or a
// second copy of a name, fails, so the image cannot ship a binary the deployer
// did not build or fetch, or miss one it did.
func TestDaemonDockerfileCopiesOnlyTheBinariesAndDolt(t *testing.T) {
	t.Parallel()
	doltCopies := 0
	contextCopies := map[string]int{}
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd != "COPY" {
			continue
		}
		from, src, dst := copyOperands(in.args)
		if from == "" {
			if !slices.Contains(imageBinaryNames, src) || dst != "/usr/local/bin/"+src {
				t.Errorf("%s COPY %q reads %q from the build context; want one of gt, bd, om or claude, by its bare name, to /usr/local/bin", DockerfilePath, in.args, src)
				continue
			}
			contextCopies[src]++
			continue
		}
		if from != "dolt" || src != "/usr/local/bin/dolt" || dst != "/usr/local/bin/dolt" {
			t.Errorf("%s COPY %q reads %q from stage %q; the only stage copy is the Dolt client from the dolt stage", DockerfilePath, in.args, src, from)
			continue
		}
		doltCopies++
	}
	if doltCopies != 1 {
		t.Errorf("%s COPYs the Dolt client %d times from the dolt stage, want once", DockerfilePath, doltCopies)
	}
	for _, name := range imageBinaryNames {
		if contextCopies[name] != 1 {
			t.Errorf("%s COPYs %s from the build context %d times, want once", DockerfilePath, name, contextCopies[name])
		}
	}
}

// TestDaemonDockerfileCopiesTheClaudeCLIAndNothingFetchesIt: the claude CLI
// arrives by one COPY from the build context, which the deployer's FetchClaudeCLI
// has verified. No other instruction names it, and none downloads or installs
// it, so the image can never run a binary the pin did not check.
func TestDaemonDockerfileCopiesTheClaudeCLIAndNothingFetchesIt(t *testing.T) {
	t.Parallel()
	var copies []string
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		line := in.cmd + " " + strings.Join(in.args, " ")
		if !strings.Contains(strings.ToLower(line), "claude") {
			continue
		}
		if in.cmd != "COPY" {
			t.Errorf("%s has %q naming the claude CLI; the only instruction that names it is its COPY", DockerfilePath, line)
			continue
		}
		copies = append(copies, line)
	}
	want := []string{"COPY --chmod=0755 claude /usr/local/bin/claude"}
	if !reflect.DeepEqual(copies, want) {
		t.Errorf("%s names the claude CLI in %q, want exactly %q", DockerfilePath, copies, want)
	}
}

// TestDaemonDockerfileHasNoRepeatedCommentLine: a comment block pasted twice is
// left dangling above the next instruction, so no comment line may appear twice.
// A bare "#" separator is exempt.
func TestDaemonDockerfileHasNoRepeatedCommentLine(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for _, line := range strings.Split(readDockerfile(t), "\n") {
		text := strings.TrimSpace(line)
		if !strings.HasPrefix(text, "#") || text == "#" {
			continue
		}
		if seen[text] {
			t.Errorf("%s repeats the comment line %q; a pasted comment block is left dangling", DockerfilePath, text)
		}
		seen[text] = true
	}
}

// copyOperands returns a COPY's --from stage, and its source and destination
// operands, with its --flag entries dropped. A COPY with no --from reads the
// build context, so its from is empty.
func copyOperands(args []string) (from, src, dst string) {
	for _, arg := range args {
		if name, ok := strings.CutPrefix(arg, "--from="); ok {
			from = name
		}
	}
	ops := withoutFlags(args)
	if len(ops) != 2 {
		return "?", "", ""
	}
	return from, ops[0], ops[1]
}

// TestDaemonDockerfileMatchesTheImageTable: the base image and the Dolt version
// this file names are internal/templates/images.go's, so the table a caller
// reads and the image built here can never name different versions.
//
// The file has two stages: the Dolt client comes from the official Dolt image at
// DoltVersion, and the image the daemon runs (the last stage) is built FROM
// DaemonBaseImage.
func TestDaemonDockerfileMatchesTheImageTable(t *testing.T) {
	t.Parallel()
	ins := dockerfileInstructions(t, readDockerfile(t))
	args := map[string]string{}
	for _, in := range ins {
		if in.cmd == "ARG" {
			if key, value, ok := strings.Cut(in.args[0], "="); ok {
				args[key] = value
			}
		}
	}
	if got := args["DOLT_VERSION"]; got != templates.DoltVersion {
		t.Errorf("%s installs Dolt %q, want %q (templates.DoltVersion)", DockerfilePath, got, templates.DoltVersion)
	}
	if got := args["DOLT_DIGEST"]; got != templates.DoltImageDigest {
		t.Errorf("%s pins the Dolt image at %q, want %q (templates.DoltImageDigest)", DockerfilePath, got, templates.DoltImageDigest)
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(templates.DoltImageDigest) {
		t.Errorf("templates.DoltImageDigest = %q, want sha256: and 64 lower-case hex digits", templates.DoltImageDigest)
	}
	var stages []stage
	for _, in := range ins {
		if in.cmd == "FROM" {
			stages = append(stages, newStage(t, in.args, args))
		}
	}
	if len(stages) != 2 {
		t.Fatalf("%s builds %d stages, want the Dolt client's and the image's", DockerfilePath, len(stages))
	}
	if want := "dolthub/dolt:" + templates.DoltVersion + "@" + templates.DoltImageDigest; stages[0].image != want {
		t.Errorf("%s takes the Dolt client from %q, want %q", DockerfilePath, stages[0].image, want)
	}
	if stages[0].alias != "dolt" {
		t.Errorf("the first stage is named %q, want dolt, the name the COPY reads", stages[0].alias)
	}
	if want := templates.DaemonBaseImage; stages[1].image != want {
		t.Errorf("%s builds FROM %q, want %q (templates.DaemonBaseImage)", DockerfilePath, stages[1].image, want)
	}
}

// stage is one FROM of the Dockerfile: the image it starts from and the alias a
// COPY --from names it by.
type stage struct {
	image, alias string
}

// newStage reads a FROM's image and alias, resolving the ARG references the file
// defines. It fails the test on a reference the file does not define, so an
// image the test could not resolve is never compared as if it were.
func newStage(t *testing.T, args []string, values map[string]string) stage {
	t.Helper()
	image := dockerArgRef.ReplaceAllStringFunc(args[0], func(ref string) string {
		name := strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(ref, "$"), "{"), "}")
		if value, ok := values[name]; ok {
			return value
		}
		return ref
	})
	if strings.Contains(image, "$") {
		t.Fatalf("%s starts a stage from %q, an image the file defines no version of", DockerfilePath, image)
	}
	st := stage{image: image}
	if len(args) > 2 && strings.EqualFold(args[1], "AS") {
		st.alias = args[2]
	}
	return st
}

// dockerArgRef matches an ARG reference in an instruction operand, in either the
// ${NAME} or the $NAME form.
var dockerArgRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}|\$([A-Za-z_][A-Za-z0-9_]*)`)

// aptInstalls returns the packages a RUN installs, sorted, and a problem naming
// the first command it runs that the image may not. The RUN is read command by
// command, as the shell runs its && chain: apt-get update, the one apt-get
// install, and the apt list cleanup are the only commands allowed, so a second
// install or another package manager cannot hide later in the RUN.
func aptInstalls(run string) (pkgs []string, problem string) {
	installs := 0
	for _, seg := range strings.Split(strings.TrimPrefix(run, "RUN "), "&&") {
		words := strings.Fields(seg)
		switch {
		case slices.Equal(words, []string{"apt-get", "update"}):
		case slices.Equal(words, []string{"rm", "-rf", "/var/lib/apt/lists/*"}):
		case len(words) > 1 && words[0] == "apt-get" && words[1] == "install":
			installs++
			for _, w := range words[2:] {
				if !strings.HasPrefix(w, "-") {
					pkgs = append(pkgs, w)
				}
			}
		default:
			return nil, fmt.Sprintf("runs %q, which is not apt-get update, the apt-get install, or the list cleanup", strings.TrimSpace(seg))
		}
	}
	if installs != 1 {
		return nil, fmt.Sprintf("runs %d apt-get installs in one RUN, want one", installs)
	}
	slices.Sort(pkgs)
	return pkgs, ""
}

// withoutFlags returns an instruction's operands with its --flag entries
// dropped, so COPY --chmod=0755 gt /usr/local/bin/gt reads as its source gt and
// its destination.
func withoutFlags(args []string) []string {
	out := make([]string, 0, len(args))
	for _, arg := range args {
		if !strings.HasPrefix(arg, "--") {
			out = append(out, arg)
		}
	}
	return out
}

// dockerfileInstruction is one parsed instruction line of a Dockerfile.
type dockerfileInstruction struct {
	cmd  string
	args []string
}

// dockerfileInstructions parses the Dockerfile text into instructions: comments
// and blank lines dropped, backslash continuations joined, and each
// instruction's command upper-cased. It fails the test on a text it reads no
// instruction from, so a parser that silently read nothing cannot pass a
// comparison against nothing.
func dockerfileInstructions(t *testing.T, body string) []dockerfileInstruction {
	t.Helper()
	var out []dockerfileInstruction
	var logical []string
	flush := func() {
		if len(logical) == 0 {
			return
		}
		fields := strings.Fields(strings.Join(logical, " "))
		logical = nil
		if len(fields) == 0 {
			return
		}
		out = append(out, dockerfileInstruction{cmd: strings.ToUpper(fields[0]), args: fields[1:]})
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasSuffix(line, "\\") {
			logical = append(logical, strings.TrimSpace(strings.TrimSuffix(line, "\\")))
			continue
		}
		logical = append(logical, line)
		flush()
	}
	flush()
	if len(out) == 0 {
		t.Fatalf("%s parsed as no instructions", DockerfilePath)
	}
	return out
}

// readDockerfile returns the image's Dockerfile as the build reads it.
func readDockerfile(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(repoFile(t, DockerfilePath))
	if err != nil {
		t.Fatalf("reading %s: %v", DockerfilePath, err)
	}
	return string(body)
}
