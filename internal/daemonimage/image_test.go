package daemonimage

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
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
// binaries the deployer built and proved.
func TestDaemonDockerfileHasNoBuildStep(t *testing.T) {
	t.Parallel()
	body := readDockerfile(t)
	if strings.Contains(body, "go build") {
		t.Errorf("%s runs a go build; the binaries are COPYed in", DockerfilePath)
	}
	if line := regexp.MustCompile(`(?m)^.*\bmake\b.*$`).FindString(body); line != "" {
		t.Errorf("%s runs %q; nothing is built inside the image", DockerfilePath, strings.TrimSpace(line))
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
			copied[in.args[0]] = true
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
func TestDaemonDockerfileMatchesTheImageTable(t *testing.T) {
	t.Parallel()
	body := readDockerfile(t)
	var from []string
	args := map[string]string{}
	for _, in := range dockerfileInstructions(t, body) {
		switch in.cmd {
		case "FROM":
			from = append(from, strings.Join(in.args, " "))
		case "ARG":
			if key, value, ok := strings.Cut(in.args[0], "="); ok {
				args[key] = value
			}
		}
	}
	if want := []string{templates.DaemonBaseImage}; !reflect.DeepEqual(from, want) {
		t.Errorf("%s builds FROM %q, want %q (templates.DaemonBaseImage)", DockerfilePath, from, want)
	}
	if got := args["DOLT_VERSION"]; got != templates.DoltVersion {
		t.Errorf("%s installs Dolt %q, want %q (templates.DoltVersion)", DockerfilePath, got, templates.DoltVersion)
	}
}

// dockerfileInstruction is one parsed instruction line of a Dockerfile.
type dockerfileInstruction struct {
	cmd  string
	args []string
}

// dockerfileInstructions parses the Dockerfile text: comments and blank lines
// dropped, and each instruction's command upper-cased. It fails the test on a
// text it reads no instruction from, so a parser that silently read nothing
// cannot pass a comparison against nothing.
func dockerfileInstructions(t *testing.T, body string) []dockerfileInstruction {
	t.Helper()
	var out []dockerfileInstruction
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmd, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		out = append(out, dockerfileInstruction{cmd: strings.ToUpper(cmd), args: strings.Fields(rest)})
	}
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
