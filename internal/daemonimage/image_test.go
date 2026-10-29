package daemonimage

import (
	"context"
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
// fresh directory holding the three binaries as the build context, and run.
func imageBuilder(t *testing.T) (Builder, *fakeRunner) {
	t.Helper()
	run := &fakeRunner{}
	b := validBuilder(run, t.TempDir())
	b.Dockerfile = repoFile(t, DockerfilePath)
	for _, name := range []string{"gt", "bd", "om"} {
		if err := os.WriteFile(filepath.Join(b.Out, name), []byte("binary"), 0o600); err != nil {
			t.Fatalf("writing the %s binary: %v", name, err)
		}
	}
	return b, run
}

// imageBinaries is the list BuildBinaries returns for b, in its order.
func imageBinaries(b Builder) []string {
	return []string{
		filepath.Join(b.Out, "gt"),
		filepath.Join(b.Out, "bd"),
		filepath.Join(b.Out, "om"),
	}
}

// repoFile returns the absolute path of a file of the repo this package lives
// in, named relative to its root.
func repoFile(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(filepath.Join("..", "..", rel))
	if err != nil {
		t.Fatalf("resolving %s: %v", rel, err)
	}
	return abs
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
		"GOCACHE=/cache/go", "DOCKER_HOST=tcp://elsewhere:2375")
	if _, err := b.BuildImage(context.Background(), testRev, imageBinaries(b)); err != nil {
		t.Fatalf("BuildImage: %v", err)
	}
	env := run.calls[0].env
	for _, want := range []string{"PATH=/usr/bin:/bin", "HOME=/home/builder"} {
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
// config, so no instruction downloads or installs anything over the network and
// the one RUN is the apt install of ca-certificates, git and tmux. The Dolt
// client arrives by COPY from the official Dolt image, whose registry layers the
// daemon verifies by digest, rather than by a download inside a RUN that no
// checksum would guard.
func TestDaemonDockerfileFetchesNothingAndInstallsOnlyThePackages(t *testing.T) {
	t.Parallel()
	var runs []string
	copiedDolt := false
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		line := in.cmd + " " + strings.Join(in.args, " ")
		for _, banned := range []string{"curl", "wget", "http://", "https://", "git clone", "tar -", "install -m"} {
			if strings.Contains(line, banned) {
				t.Errorf("%s has %q in %q; the image fetches and installs nothing but the three packages", DockerfilePath, banned, line)
			}
		}
		switch in.cmd {
		case "RUN":
			runs = append(runs, line)
		case "COPY":
			if src := withoutFlags(in.args); len(src) > 0 && filepath.Base(src[0]) == "dolt" {
				copiedDolt = true
			}
		}
	}
	if len(runs) != 1 {
		t.Fatalf("%s has %d RUN instructions, want the one package install: %q", DockerfilePath, len(runs), runs)
	}
	if got, want := installedPackages(runs[0]), []string{"ca-certificates", "git", "tmux"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s installs %q, want %q", DockerfilePath, got, want)
	}
	if !copiedDolt {
		t.Errorf("%s does not COPY the Dolt client in; it has no other source", DockerfilePath)
	}
}

// TestDaemonDockerfileCopiesTheThreeBinaries: the COPY sources are the three
// names BuildImage's context holds, so the image cannot ship a binary the
// deployer did not build or miss one it did.
func TestDaemonDockerfileCopiesTheThreeBinaries(t *testing.T) {
	t.Parallel()
	copied := map[string]bool{}
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd == "COPY" {
			if src := withoutFlags(in.args); len(src) > 0 {
				copied[filepath.Base(src[0])] = true
			}
		}
	}
	for _, name := range []string{"gt", "bd", "om"} {
		if !copied[name] {
			t.Errorf("%s COPYs no %s from the build context", DockerfilePath, name)
		}
	}
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
	var stages []stage
	for _, in := range ins {
		if in.cmd == "FROM" {
			stages = append(stages, newStage(t, in.args, args))
		}
	}
	if len(stages) != 2 {
		t.Fatalf("%s builds %d stages, want the Dolt client's and the image's", DockerfilePath, len(stages))
	}
	if want := "dolthub/dolt:" + templates.DoltVersion; stages[0].image != want {
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

// installedPackages returns the packages an apt-get install instruction names,
// sorted: the words after --no-install-recommends up to the next command.
func installedPackages(line string) []string {
	_, after, ok := strings.Cut(line, "--no-install-recommends ")
	if !ok {
		return nil
	}
	after, _, _ = strings.Cut(after, "&&")
	pkgs := slices.Clone(strings.Fields(after))
	slices.Sort(pkgs)
	return pkgs
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
