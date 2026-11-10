package daemonimage

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/templates"
)

// imageBuilder is a Builder ready to build the image: the repo's Dockerfile, a
// fresh directory holding the binaries and the plugin binaries as the build
// context, and run.
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
	for _, p := range imagePlugins {
		dir := filepath.Join(b.Out, "plugins", p.name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("making the %s plugin directory: %v", p.name, err)
		}
		if err := os.WriteFile(filepath.Join(dir, p.name), []byte("binary"), 0o600); err != nil {
			t.Fatalf("writing the %s plugin binary: %v", p.name, err)
		}
	}
	for _, name := range mangoNames {
		dir := filepath.Join(b.Out, mangoDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("making the mango directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("binary"), 0o600); err != nil {
			t.Fatalf("writing the mango %s: %v", name, err)
		}
	}
	return b, run
}

// imageBinaryNames is the binaries the image copies from the root of its build
// context.
var imageBinaryNames = []string{"gt", "bd", "om", "claude", "hm", "devops-reporter", "release-reporter", "openshell", "docker", "kubectl"}

// imageBinaries is the list of the image's binaries, plugin binaries and mango
// files in b.Out, in the order the Dockerfile copies them from its context.
func imageBinaries(b Builder) []string {
	out := make([]string, 0, len(imageBinaryNames)+len(imagePlugins)+len(mangoNames))
	for _, name := range imageBinaryNames {
		out = append(out, filepath.Join(b.Out, name))
	}
	for _, p := range imagePlugins {
		out = append(out, filepath.Join(b.Out, "plugins", p.name, p.name))
	}
	for _, name := range mangoNames {
		out = append(out, filepath.Join(b.Out, mangoDir, name))
	}
	return out
}

// pluginCopy is one plugin binary's COPY operands: the path in the build
// context and the path in the image, written the way the Dockerfile writes them.
type pluginCopy struct{ src, dst string }

// pluginCopies is the COPY operands the Dockerfile must carry for imagePlugins,
// in the order BuildPlugins writes them.
func pluginCopies() []pluginCopy {
	copies := make([]pluginCopy, 0, len(imagePlugins))
	for _, p := range imagePlugins {
		copies = append(copies, pluginCopy{
			src: "plugins/" + p.name + "/" + p.name,
			dst: "/usr/local/lib/gt/plugins/" + p.name + "/" + p.name,
		})
	}
	return copies
}

// mangoCopies are the COPY operands the Dockerfile must carry for mangoNames,
// in the order BuildMango publishes them.
func mangoCopies() []pluginCopy {
	copies := make([]pluginCopy, 0, len(mangoNames))
	for _, name := range mangoNames {
		copies = append(copies, pluginCopy{
			src: mangoDir + "/" + name,
			dst: "/usr/local/lib/gt/mango/" + name,
		})
	}
	return copies
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
		{name: "a known_hosts file missing beside the Dockerfile", rev: testRev, breakIt: func(t *testing.T, b *Builder, _ *[]string) {
			dir := t.TempDir()
			body, err := os.ReadFile(b.Dockerfile)
			if err != nil {
				t.Fatalf("reading the Dockerfile: %v", err)
			}
			if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), body, 0o644); err != nil {
				t.Fatalf("writing the Dockerfile copy: %v", err)
			}
			b.Dockerfile = filepath.Join(dir, "Dockerfile")
		}, want: "is not readable"},
		{name: "a known_hosts path the build context cannot hold", rev: testRev, breakIt: func(t *testing.T, b *Builder, _ *[]string) {
			if err := os.Mkdir(filepath.Join(b.Out, KnownHostsName), 0o755); err != nil {
				t.Fatalf("making a directory where the staged file goes: %v", err)
			}
		}, want: "staging ssh_known_hosts"},
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
		{name: "the mango files missing from the list", rev: testRev, breakIt: func(_ *testing.T, _ *Builder, bins *[]string) {
			*bins = (*bins)[:len(*bins)-len(mangoNames)]
		}, want: "no mango binary"},
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
// that is not a git identity command installs exactly ca-certificates, git,
// openssh-client, tmux, curl, lsof and tzdata. The Dolt client arrives by COPY from the
// official Dolt image, whose registry layers the daemon verifies by digest, rather
// than by a download inside a RUN that no checksum would guard. curl is named only
// as a package: a RUN that fetched with it would be a second install RUN and fail
// the count below. tzdata is in the list because the compose file sets TZ and
// Ubuntu ships no zone database, so without it the log reads UTC (gt-gg9hv.34).
func TestDaemonDockerfileFetchesNothingAndInstallsOnlyThePackages(t *testing.T) {
	t.Parallel()
	var installs []string
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		line := in.cmd + " " + strings.Join(in.args, " ")
		for _, banned := range []string{"wget", "http://", "https://", "git clone", "tar -", "install -m"} {
			if strings.Contains(line, banned) {
				t.Errorf("%s has %q in %q; the image fetches and installs nothing but the listed packages", DockerfilePath, banned, line)
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
	if want := []string{"ca-certificates", "curl", "git", "lsof", "openssh-client", "tmux", "tzdata"}; !reflect.DeepEqual(got, want) {
		t.Errorf("%s installs %q, want %q", DockerfilePath, got, want)
	}
}

// TestDaemonDockerfileInstallsOpenSSHClient: the image's one package-install
// RUN lists openssh-client, so a git push over SSH — app-promote's to GitHub
// with the promote keys — runs from the container.
func TestDaemonDockerfileInstallsOpenSSHClient(t *testing.T) {
	t.Parallel()
	args := daemonInstallRun(t).args
	for _, pkg := range []string{"ca-certificates", "git", "openssh-client", "tmux"} {
		if !slices.Contains(args, pkg) {
			t.Errorf("%s install line %q does not list %s", DockerfilePath, strings.Join(args, " "), pkg)
		}
	}
}

// TestDaemonDockerfileKeepsNoInstallRecommends: the install keeps the two things
// that keep the image small: --no-install-recommends, so apt adds no extras, and
// the package-list cleanup in the same RUN, so no stale index ships. Adding a
// package must not drop either.
func TestDaemonDockerfileKeepsNoInstallRecommends(t *testing.T) {
	t.Parallel()
	args := daemonInstallRun(t).args
	line := strings.Join(args, " ")
	if !slices.Contains(args, "--no-install-recommends") {
		t.Errorf("%s install line %q drops --no-install-recommends, so apt pulls the recommended extras", DockerfilePath, line)
	}
	if !strings.Contains(line, "rm -rf /var/lib/apt/lists/*") {
		t.Errorf("%s install line %q leaves the apt package lists in the image", DockerfilePath, line)
	}
}

// daemonInstallRun returns the image's one package-install RUN, the instruction
// that carries apt-get install. The file carries exactly one, so a second, or
// none, fails the test rather than letting a check read an arbitrary RUN.
func daemonInstallRun(t *testing.T) dockerfileInstruction {
	t.Helper()
	var runs []dockerfileInstruction
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd != "RUN" {
			continue
		}
		for i := 0; i+1 < len(in.args); i++ {
			if in.args[i] == "apt-get" && in.args[i+1] == "install" {
				runs = append(runs, in)
				break
			}
		}
	}
	if len(runs) != 1 {
		t.Fatalf("%s has %d apt-get install RUNs, want the one package install", DockerfilePath, len(runs))
	}
	return runs[0]
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

// TestDaemonDockerfileCopiesOnlyTheBinariesAndDolt: every COPY is one of three
// kinds. The Dolt client is copied from the dolt stage, and nothing else is.
// Each of the image binaries (imageBinaryNames) is copied from the build context
// by its bare name to /usr/local/bin, once each; each plugin binary is copied
// from its plugin directory to the image's plugin directory, once each; and each
// of mangoNames is copied from mango/ to /usr/local/lib/gt/mango, once each. A
// COPY from any other source, or a second copy of a name, fails, so the image
// cannot ship a binary the deployer did not build or fetch, or miss one it did.
func TestDaemonDockerfileCopiesOnlyTheBinariesAndDolt(t *testing.T) {
	t.Parallel()
	wantOther := map[string]string{}
	for _, p := range append(pluginCopies(), mangoCopies()...) {
		wantOther[p.src] = p.dst
	}
	doltCopies := 0
	knownHostsCopies := 0
	contextCopies := map[string]int{}
	otherCopied := map[string]int{}
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd != "COPY" {
			continue
		}
		from, src, dst := copyOperands(in.args)
		if from == "" {
			if src == KnownHostsName && dst == "/etc/ssh/ssh_known_hosts" {
				knownHostsCopies++
				continue
			}
			if !slices.Contains(imageBinaryNames, src) {
				if want, ok := wantOther[src]; ok && dst == want {
					otherCopied[src]++
					continue
				}
				t.Errorf("%s COPY %q reads %q from the build context; want one of gt, bd, om or claude by its bare name to /usr/local/bin, a plugin's binary to the image's plugin directory, or a mango file to /usr/local/lib/gt/mango", DockerfilePath, in.args, src)
				continue
			}
			if dst != "/usr/local/bin/"+src {
				t.Errorf("%s COPY %q reads %s to %q; want it to /usr/local/bin/%s", DockerfilePath, in.args, src, dst, src)
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
	if knownHostsCopies != 1 {
		t.Errorf("%s COPYs %s to /etc/ssh/ssh_known_hosts %d times, want once", DockerfilePath, KnownHostsName, knownHostsCopies)
	}
	for _, name := range imageBinaryNames {
		if contextCopies[name] != 1 {
			t.Errorf("%s COPYs %s from the build context %d times, want once", DockerfilePath, name, contextCopies[name])
		}
	}
	for _, p := range append(pluginCopies(), mangoCopies()...) {
		if otherCopied[p.src] != 1 {
			t.Errorf("%s COPYs %s from the build context %d times, want once", DockerfilePath, p.src, otherCopied[p.src])
		}
	}
}

// TestDaemonDockerfileCopiesThePluginBinaries: the image ships each plugin
// binary at /usr/local/lib/gt/plugins/<name>/<name>, by one COPY of mode 0755
// from the plugin's build in the build context.
func TestDaemonDockerfileCopiesThePluginBinaries(t *testing.T) {
	t.Parallel()
	var got []string
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd != "COPY" {
			continue
		}
		_, src, _ := copyOperands(in.args)
		for _, p := range pluginCopies() {
			if src == p.src {
				got = append(got, in.cmd+" "+strings.Join(in.args, " "))
			}
		}
	}
	var want []string
	for _, p := range pluginCopies() {
		want = append(want, "COPY --chmod=0755 "+p.src+" "+p.dst)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s copies the plugin binaries as %q, want %q", DockerfilePath, got, want)
	}
}

// TestDaemonDockerfileCopiesTheMangoBinary: the image ships mango and its commit
// stamp at /usr/local/lib/gt/mango, by one COPY of mode 0755 from the build
// context each.
func TestDaemonDockerfileCopiesTheMangoBinary(t *testing.T) {
	t.Parallel()
	var got []string
	for _, in := range dockerfileInstructions(t, readDockerfile(t)) {
		if in.cmd != "COPY" {
			continue
		}
		_, src, _ := copyOperands(in.args)
		for _, p := range mangoCopies() {
			if src == p.src {
				got = append(got, in.cmd+" "+strings.Join(in.args, " "))
			}
		}
	}
	var want []string
	for _, p := range mangoCopies() {
		want = append(want, "COPY --chmod=0755 "+p.src+" "+p.dst)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s copies the mango files as %q, want %q", DockerfilePath, got, want)
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

// TestDaemonDockerfileRunsAsRootWhereItsIdentityIs: the git identity RUNs write
// root's config, and the container runs as root, so the two agree only while the
// final stage has no USER instruction. A USER before the identity RUNs would make
// them write another user's home; one after would leave the identity in root's
// home for a container that runs as someone else.
func TestDaemonDockerfileRunsAsRootWhereItsIdentityIs(t *testing.T) {
	t.Parallel()
	if problems := imageUserProblems(t, readDockerfile(t)); len(problems) != 0 {
		t.Errorf("%s: %s", DockerfilePath, strings.Join(problems, "; "))
	}
}

// TestDaemonDockerfileUserInstructionFailsTheRootCheck: a USER line inserted into
// the final stage, before or after the identity RUNs, is refused by the same
// check the real file passes, so that check cannot pass on any placement.
func TestDaemonDockerfileUserInstructionFailsTheRootCheck(t *testing.T) {
	t.Parallel()
	body := readDockerfile(t)
	for _, tc := range []struct {
		name, anchor string
	}{
		{name: "before the identity RUNs", anchor: "RUN git config --global user.name gt-daemon\n"},
		{name: "after the identity RUNs", anchor: "ENTRYPOINT ["},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if n := strings.Count(body, tc.anchor); n != 1 {
				t.Fatalf("%s has %d lines at %q, want the one the USER line goes before", DockerfilePath, n, tc.anchor)
			}
			mutated := strings.Replace(body, tc.anchor, "USER gt-daemon\n"+tc.anchor, 1)
			if problems := imageUserProblems(t, mutated); len(problems) == 0 {
				t.Errorf("a USER line %s passed the root check, want it refused", tc.name)
			}
		})
	}
}

// imageUserProblems returns a problem for each USER instruction in the image's
// final stage, the stage the container runs. Earlier stages build other images,
// so their USER lines never reach the container.
func imageUserProblems(t *testing.T, body string) []string {
	t.Helper()
	ins := dockerfileInstructions(t, body)
	last := -1
	for i, in := range ins {
		if in.cmd == "FROM" {
			last = i
		}
	}
	if last < 0 {
		t.Fatalf("%s has no FROM, so no final stage", DockerfilePath)
	}
	var problems []string
	for _, in := range ins[last+1:] {
		if in.cmd == "USER" {
			problems = append(problems, fmt.Sprintf("USER %s runs the container as another user, but the git identity RUNs write root's config", strings.Join(in.args, " ")))
		}
	}
	return problems
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

// TestDaemonImageCarriesWhatTheAuditExecs: every program a runs-in-container row
// of the audit table execs is on the image's PATH. The image's programs are read
// from the build file, so a row naming a program the image does not carry fails
// here before a rehearsal runs it.
func TestDaemonImageCarriesWhatTheAuditExecs(t *testing.T) {
	t.Parallel()
	carried, problems := imageCarriedPrograms(t, readDockerfile(t))
	for _, problem := range problems {
		t.Errorf("%s: %s", DockerfilePath, problem)
	}
	var runs []config.DaemonStep
	for _, s := range config.DaemonSteps() {
		if s.Class == config.DaemonStepRuns {
			runs = append(runs, s)
		}
	}
	if len(runs) == 0 {
		t.Fatal("the audit table has no runs-in-container row, so nothing is checked")
	}
	for _, problem := range missingAuditPrograms(runs, carried) {
		t.Error(problem)
	}
}

// TestMissingAuditProgramsNamesTheStep: a runs-in-container row that execs make,
// on an image without make, is reported with make and the row's name.
func TestMissingAuditProgramsNamesTheStep(t *testing.T) {
	t.Parallel()
	rows := []config.DaemonStep{{Name: "fixture_step", Kind: config.DaemonStepPatrol, Class: config.DaemonStepRuns, Needs: []string{"make"}}}
	got := missingAuditPrograms(rows, map[string]bool{"git": true})
	if len(got) != 1 || !strings.Contains(got[0], "make") || !strings.Contains(got[0], "fixture_step") {
		t.Fatalf("missingAuditPrograms = %q, want one failure naming make and fixture_step", got)
	}
}

// TestImageCarriedProgramsRefusesWhatItCannotRead: an install of a package the
// test maps to no program, or a COPY with no destination, is reported as a
// problem rather than read as carrying nothing.
func TestImageCarriedProgramsRefusesWhatItCannotRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		body string
		want string
	}{
		{name: "an unmapped package", body: "RUN apt-get update && apt-get install -y --no-install-recommends mystery-pkg && rm -rf /var/lib/apt/lists/*\n", want: "mystery-pkg"},
		{name: "a COPY with no destination", body: "COPY gt\n", want: `COPY "gt"`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, problems := imageCarriedPrograms(t, tc.body)
			if !slices.ContainsFunc(problems, func(p string) bool { return strings.Contains(p, tc.want) }) {
				t.Errorf("problems = %q, want one naming %q", problems, tc.want)
			}
		})
	}
}

// TestImageCarriedProgramsCopyToADirectoryCarriesTheSourceName: a COPY whose
// destination ends in a slash puts the source under its own name in that
// directory, so COPY to /usr/local/bin/ carries the source's basename.
func TestImageCarriedProgramsCopyToADirectoryCarriesTheSourceName(t *testing.T) {
	t.Parallel()
	carried, problems := imageCarriedPrograms(t, "COPY --chmod=0755 gt /usr/local/bin/\n")
	if len(problems) != 0 {
		t.Fatalf("problems = %q, want none", problems)
	}
	if !carried["gt"] {
		t.Errorf("carried = %v, want gt, which the COPY puts at /usr/local/bin/gt", carried)
	}
}

// aptPackagePrograms maps each package the one install RUN installs to the
// programs it puts on the PATH. A nil list is a package with no program.
var aptPackagePrograms = map[string][]string{
	"ca-certificates": nil,
	"curl":            {"curl"},
	"git":             {"git"},
	"lsof":            {"lsof"},
	"openssh-client":  {"ssh"},
	"tmux":            {"tmux"},
	"tzdata":          nil,
}

// basePrograms are the programs the ubuntu:24.04 base image carries, so the
// Dockerfile installs neither.
var basePrograms = []string{"bash", "ps"}

// imageCarriedPrograms returns the programs the image built from body carries:
// the base programs, the programs of each package the apt install RUN installs,
// and the basename of each COPY whose destination is in /usr/local/bin. A package
// the map does not name, or a COPY it cannot read, is returned as a problem.
func imageCarriedPrograms(t *testing.T, body string) (map[string]bool, []string) {
	t.Helper()
	carried := map[string]bool{}
	for _, p := range basePrograms {
		carried[p] = true
	}
	var problems []string
	for _, in := range dockerfileInstructions(t, body) {
		switch in.cmd {
		case "RUN":
			if !slices.Contains(in.args, "apt-get") {
				continue
			}
			pkgs, problem := aptInstalls("RUN " + strings.Join(in.args, " "))
			if problem != "" {
				problems = append(problems, problem)
				continue
			}
			for _, pkg := range pkgs {
				progs, known := aptPackagePrograms[pkg]
				if !known {
					problems = append(problems, fmt.Sprintf("installs %s, which the test maps to no program", pkg))
				}
				for _, p := range progs {
					carried[p] = true
				}
			}
		case "COPY":
			_, src, dst := copyOperands(in.args)
			if src == "" || dst == "" {
				problems = append(problems, fmt.Sprintf("COPY %q has no source and destination", strings.Join(in.args, " ")))
				continue
			}
			if strings.HasSuffix(dst, "/") {
				dst = path.Join(dst, path.Base(src))
			}
			if path.Dir(dst) == "/usr/local/bin" {
				carried[path.Base(dst)] = true
			}
		}
	}
	return carried, problems
}

// missingAuditPrograms names each program the rows exec that carried does not hold,
// with the rows that need it, sorted by program.
func missingAuditPrograms(rows []config.DaemonStep, carried map[string]bool) []string {
	needed := map[string][]string{}
	for _, s := range rows {
		for _, p := range s.Needs {
			if !carried[p] {
				needed[p] = append(needed[p], s.Name)
			}
		}
	}
	progs := make([]string, 0, len(needed))
	for p := range needed {
		progs = append(progs, p)
	}
	slices.Sort(progs)
	out := make([]string, 0, len(progs))
	for _, p := range progs {
		out = append(out, fmt.Sprintf("%s is not on the image's PATH, needed by %s", p, strings.Join(needed[p], ", ")))
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
