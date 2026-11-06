package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemonimage"
)

const (
	imageTestRev         = "0123456789abcdef0123456789abcdef01234567"
	imageTestBDCommit    = "fedcba9876543210fedcba9876543210fedcba98"
	imageTestOMCommit    = "00112233445566778899aabbccddeeff00112233"
	imageTestHMCommit    = "abcdef0123456789abcdef0123456789abcdef01"
	imageTestDevCommit   = "1234567890abcdef1234567890abcdef12345678"
	imageTestMangoCommit = "2233445566778899aabbccddeeff001122334455"
)

// imagePinsText is the pins file newImageRig writes, one line per source.
func imagePinsText() string {
	return "bd http://forgejo:3000/sloan/beads.git " + imageTestBDCommit + "\n" +
		"om http://forgejo:3000/sloan/organic-mechanic.git " + imageTestOMCommit + "\n" +
		"hm http://forgejo:3000/sloan/history-man.git " + imageTestHMCommit + "\n" +
		"devops http://forgejo:3000/sloan/devops.git " + imageTestDevCommit + "\n"
}

// imageRig is a set of real directories for the command's paths and deps that
// fail the test if a build seam is used: a dry run and a refusal run nothing.
// Its fake git answers only the pin-drift guard's origin/main read, with the
// pin itself each checkout holds.
type imageRig struct {
	flags daemonImageFlags
	deps  daemonImageDeps
	out   *bytes.Buffer
	calls []string
}

func newImageRig(t *testing.T) *imageRig {
	t.Helper()
	root := t.TempDir()
	mk := func(name string) string {
		p := filepath.Join(root, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	gtSrc := mk("gt")
	if err := os.MkdirAll(filepath.Join(gtSrc, "deploy", "daemon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gtSrc, daemonimage.DockerfilePath), []byte("FROM scratch\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gtSrc, "deploy", "daemon", daemonimage.KnownHostsName), []byte("x\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r := &imageRig{out: &bytes.Buffer{}}
	r.flags = daemonImageFlags{
		rev: imageTestRev, out: mk("out"), gtSrc: gtSrc, bdSrc: mk("beads"),
		omSrc: mk("om"), hmSrc: mk("hm"), devopsSrc: mk("devops"),
		mangoSrc: mk("mango"), mangoCommit: imageTestMangoCommit,
		pins: filepath.Join(root, "image-pins"),
	}
	if err := os.WriteFile(r.flags.pins, []byte(imagePinsText()), 0o600); err != nil {
		t.Fatal(err)
	}
	mainBySrc := map[string]string{
		r.flags.bdSrc: imageTestBDCommit, r.flags.omSrc: imageTestOMCommit,
		r.flags.hmSrc: imageTestHMCommit, r.flags.devopsSrc: imageTestDevCommit,
	}
	r.deps = daemonImageDeps{
		run: func(context.Context, string, []string, []string) error {
			t.Error("a build command ran")
			return nil
		},
		git: func(dir string, args ...string) ([]byte, error) {
			if len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" && args[2] == "origin/main^{commit}" {
				if sha, ok := mainBySrc[dir]; ok {
					return []byte(sha + "\n"), nil
				}
			}
			t.Error("git ran")
			return nil, nil
		},
		fetch: func(context.Context, string) (io.ReadCloser, error) {
			t.Error("the network was reached")
			return nil, nil
		},
		environ:      []string{"PATH=/usr/bin"},
		evalSymlinks: filepath.EvalSymlinks,
		out:          r.out,
	}
	return r
}

// fakeImageBuilder records the steps in order and fails the one named.
type fakeImageBuilder struct {
	calls  *[]string
	failAt string
	bins   []string
}

func (f fakeImageBuilder) step(name string) error {
	*f.calls = append(*f.calls, name)
	if f.failAt == name {
		return errors.New(name + " failed")
	}
	return nil
}

func (f fakeImageBuilder) BuildBinaries(_ context.Context, rev string) ([]string, error) {
	if err := f.step("binaries " + rev); err != nil {
		return nil, err
	}
	return []string{"/out/gt", "/out/bd", "/out/om"}, nil
}

func (f fakeImageBuilder) BuildPlugins(_ context.Context, rev string) ([]string, error) {
	if err := f.step("plugins " + rev); err != nil {
		return nil, err
	}
	return []string{"/out/plugins/app-promote/app-promote", "/out/plugins/town-metrics/town-metrics"}, nil
}

func (f fakeImageBuilder) BuildTools(context.Context) ([]string, error) {
	if err := f.step("hm tools"); err != nil {
		return nil, err
	}
	return []string{"/out/hm", "/out/devops-reporter", "/out/release-reporter"}, nil
}

func (f fakeImageBuilder) BuildMango(context.Context) ([]string, error) {
	if err := f.step("mango"); err != nil {
		return nil, err
	}
	return []string{"/out/mango/mango", "/out/mango/mango.sha"}, nil
}

func (f fakeImageBuilder) FetchClaudeCLI(context.Context) (string, error) {
	if err := f.step("claude"); err != nil {
		return "", err
	}
	return "/out/claude", nil
}

func (f fakeImageBuilder) FetchClientTools(context.Context) ([]string, error) {
	if err := f.step("tools"); err != nil {
		return nil, err
	}
	return []string{"/out/openshell", "/out/docker", "/out/kubectl"}, nil
}

func (f fakeImageBuilder) BuildImage(_ context.Context, rev string, bins []string) (string, error) {
	if err := f.step("image " + rev + " " + strings.Join(bins, ",")); err != nil {
		return "", err
	}
	return daemonimage.ImageTag(rev), nil
}

// Without --apply the command prints the plan and runs nothing: no command, no
// git, no download, and the output directory holds no file afterwards.
func TestDaemonImageDryRunPrintsThePlanAndRunsNothing(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)

	if err := runDaemonImage(context.Background(), r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonImage: %v", err)
	}

	out := r.out.String()
	for _, want := range []string{
		"pins: " + r.flags.pins,
		"pin bd: http://forgejo:3000/sloan/beads.git at " + imageTestBDCommit,
		"pin om: http://forgejo:3000/sloan/organic-mechanic.git at " + imageTestOMCommit,
		"pin hm: http://forgejo:3000/sloan/history-man.git at " + imageTestHMCommit,
		"pin devops: http://forgejo:3000/sloan/devops.git at " + imageTestDevCommit,
		"build gt: go build -trimpath -ldflags",
		"-o " + filepath.Join(r.flags.out, "gt") + " ./cmd/gt",
		"build bd: go build -trimpath",
		"./cmd/bd",
		"build om: go build -trimpath -o " + filepath.Join(r.flags.out, "om") + " ./cmd/om",
		"build hm: go build -trimpath -o " + filepath.Join(r.flags.out, "hm") + " .",
		"build devops-reporter: go build -trimpath -o " + filepath.Join(r.flags.out, "devops-reporter") + " ./cmd/reporter",
		"build release-reporter: go build -trimpath -o " + filepath.Join(r.flags.out, "release-reporter") + " ./cmd/release-reporter",
		"check release-reporter: go version -m " + filepath.Join(r.flags.out, "release-reporter"),
		"build mango: docker --context " + daemonimage.DockerContext + " run --rm --platform linux/arm64",
		"CGO_ENABLED=1 GOOS=linux go build -trimpath -buildvcs=false",
		"fetch claude: GET ",
		"image: docker --context " + daemonimage.DockerContext + " build -f " + filepath.Join(r.flags.gtSrc, daemonimage.DockerfilePath) + " -t gt-daemon:" + imageTestRev + " " + r.flags.out,
		"dry run: nothing was run",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	entries, err := os.ReadDir(r.flags.out)
	if err != nil || len(entries) != 0 {
		t.Errorf("--out holds %d entries after a dry run (err %v), want none", len(entries), err)
	}
}

// With --apply the steps run in order, the plugins and the claude CLI join the
// binaries the image is built from, a failing step stops the rest, and the tag
// is printed.
func TestDaemonImageApplyRunsTheStepsInOrder(t *testing.T) {
	t.Parallel()
	const plugins = "/out/plugins/app-promote/app-promote,/out/plugins/town-metrics/town-metrics"
	const hmTools = "/out/hm,/out/devops-reporter,/out/release-reporter"
	const mango = "/out/mango/mango,/out/mango/mango.sha"
	const image = "image " + imageTestRev + " /out/gt,/out/bd,/out/om," + plugins + "," + hmTools + "," + mango + ",/out/claude,/out/openshell,/out/docker,/out/kubectl"
	all := []string{"binaries " + imageTestRev, "plugins " + imageTestRev, "hm tools", "mango", "claude", "tools", image}
	for _, tc := range []struct {
		name      string
		failAt    string
		want      []string
		wantTag   bool
		wantError string
	}{
		{"all succeed", "", all, true, ""},
		{"the binaries fail", "binaries " + imageTestRev, all[:1], false, "failed"},
		{"the plugins fail", "plugins " + imageTestRev, all[:2], false, "plugins " + imageTestRev + " failed"},
		{"the hm tools fail", "hm tools", all[:3], false, "hm tools failed"},
		{"the mango build fails", "mango", all[:4], false, "mango failed"},
		{"the claude CLI fails", "claude", all[:5], false, "claude failed"},
		{"the client tools fail", "tools", all[:6], false, "tools failed"},
		{"the image build fails", image, all, false, image + " failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newImageRig(t)
			r.flags.apply = true
			var calls []string
			r.deps.builder = func(daemonimage.Builder) imageBuilder {
				return fakeImageBuilder{calls: &calls, failAt: tc.failAt}
			}

			err := runDaemonImage(context.Background(), r.flags, r.deps)

			if strings.Join(calls, "|") != strings.Join(tc.want, "|") {
				t.Errorf("steps = %v, want %v", calls, tc.want)
			}
			if tc.wantError == "" && err != nil {
				t.Fatalf("runDaemonImage: %v", err)
			}
			if tc.wantError != "" && (err == nil || !strings.Contains(err.Error(), tc.wantError)) {
				t.Fatalf("error = %v, want it to contain %q", err, tc.wantError)
			}
			if got := strings.Contains(r.out.String(), "gt-daemon:"+imageTestRev+"\n"); got != tc.wantTag {
				t.Errorf("tag printed = %v, want %v (output %q)", got, tc.wantTag, r.out.String())
			}
		})
	}
}

// Every refusal comes before any step runs (the rig's deps fail the test if one
// does), with --apply as well as without. Each row removes one thing; a test
// fails if any one refusal is removed.
func TestDaemonImageRefusesBeforeAnything(t *testing.T) {
	t.Parallel()
	canon := func(t *testing.T) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "gastown", "mayor", "rig")
		if err := os.MkdirAll(filepath.Join(p, "sub"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct {
		name string
		edit func(t *testing.T, f *daemonImageFlags)
		want string
	}{
		{"no --rev", func(_ *testing.T, f *daemonImageFlags) { f.rev = "" }, "--rev is required"},
		{"no --out", func(_ *testing.T, f *daemonImageFlags) { f.out = "" }, "--out is required"},
		{"no --gt-src", func(_ *testing.T, f *daemonImageFlags) { f.gtSrc = "" }, "--gt-src is required"},
		{"no --bd-src", func(_ *testing.T, f *daemonImageFlags) { f.bdSrc = "" }, "--bd-src is required"},
		{"no --om-src", func(_ *testing.T, f *daemonImageFlags) { f.omSrc = "" }, "--om-src is required"},
		{"no --hm-src", func(_ *testing.T, f *daemonImageFlags) { f.hmSrc = "" }, "--hm-src is required"},
		{"no --devops-src", func(_ *testing.T, f *daemonImageFlags) { f.devopsSrc = "" }, "--devops-src is required"},
		{"no --mango-src", func(_ *testing.T, f *daemonImageFlags) { f.mangoSrc = "" }, "--mango-src is required"},
		{"no --mango-commit", func(_ *testing.T, f *daemonImageFlags) { f.mangoCommit = "" }, "--mango-commit is required"},
		{"no --pins", func(_ *testing.T, f *daemonImageFlags) { f.pins = "" }, "--pins is required"},
		{"a relative --out", func(_ *testing.T, f *daemonImageFlags) { f.out = "out" }, "--out \"out\" is not absolute"},
		{"a relative --gt-src", func(_ *testing.T, f *daemonImageFlags) { f.gtSrc = "gt" }, "--gt-src \"gt\" is not absolute"},
		{"a relative --bd-src", func(_ *testing.T, f *daemonImageFlags) { f.bdSrc = "beads" }, "--bd-src \"beads\" is not absolute"},
		{"a relative --om-src", func(_ *testing.T, f *daemonImageFlags) { f.omSrc = "om" }, "--om-src \"om\" is not absolute"},
		{"a relative --hm-src", func(_ *testing.T, f *daemonImageFlags) { f.hmSrc = "hm" }, "--hm-src \"hm\" is not absolute"},
		{"a relative --devops-src", func(_ *testing.T, f *daemonImageFlags) { f.devopsSrc = "devops" }, "--devops-src \"devops\" is not absolute"},
		{"a relative --mango-src", func(_ *testing.T, f *daemonImageFlags) { f.mangoSrc = "mango" }, "--mango-src \"mango\" is not absolute"},
		{"a relative --pins", func(_ *testing.T, f *daemonImageFlags) { f.pins = "image-pins" }, "--pins \"image-pins\" is not absolute"},
		{"a missing pins file", func(t *testing.T, f *daemonImageFlags) {
			f.pins = filepath.Join(t.TempDir(), "absent")
		}, "is not an existing file"},
		{"a directory for --pins", func(t *testing.T, f *daemonImageFlags) { f.pins = t.TempDir() }, "is not an existing file"},
		{"a pins file the parser refuses", func(t *testing.T, f *daemonImageFlags) {
			p := filepath.Join(t.TempDir(), "image-pins")
			if err := os.WriteFile(p, []byte("bd http://forgejo:3000/sloan/beads.git short\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			f.pins = p
		}, "line 1"},
		{"a rev that is not hex", func(_ *testing.T, f *daemonImageFlags) { f.rev = "main" }, "is not a 7+ character hex commit"},
		{"a missing directory", func(t *testing.T, f *daemonImageFlags) { f.bdSrc = filepath.Join(t.TempDir(), "absent") }, "cannot be resolved"},
		{"a file for --out", func(t *testing.T, f *daemonImageFlags) {
			p := filepath.Join(t.TempDir(), "file")
			if err := os.WriteFile(p, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			f.out = p
		}, "is not an existing directory"},
		{"the canonical clone as --out", func(t *testing.T, f *daemonImageFlags) { f.out = canon(t) }, "canonical clone"},
		{"the canonical clone as --gt-src", func(t *testing.T, f *daemonImageFlags) { f.gtSrc = canon(t) }, "canonical clone"},
		{"the canonical clone as --bd-src", func(t *testing.T, f *daemonImageFlags) { f.bdSrc = canon(t) }, "canonical clone"},
		{"the canonical clone as --om-src", func(t *testing.T, f *daemonImageFlags) { f.omSrc = canon(t) }, "canonical clone"},
		{"the canonical clone as --hm-src", func(t *testing.T, f *daemonImageFlags) { f.hmSrc = canon(t) }, "canonical clone"},
		{"the canonical clone as --devops-src", func(t *testing.T, f *daemonImageFlags) { f.devopsSrc = canon(t) }, "canonical clone"},
		{"the canonical clone as --mango-src", func(t *testing.T, f *daemonImageFlags) { f.mangoSrc = canon(t) }, "canonical clone"},
		{"a directory inside the canonical clone", func(t *testing.T, f *daemonImageFlags) { f.omSrc = filepath.Join(canon(t), "sub") }, "canonical clone"},
		{"a symlink to the canonical clone", func(t *testing.T, f *daemonImageFlags) {
			link := filepath.Join(t.TempDir(), "link")
			if err := os.Symlink(canon(t), link); err != nil {
				t.Fatalf("symlink: %v", err)
			}
			f.gtSrc = link
		}, "canonical clone"},
	} {
		for _, apply := range []bool{false, true} {
			name := tc.name + " dry"
			if apply {
				name = tc.name + " apply"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				r := newImageRig(t)
				r.flags.apply = apply
				tc.edit(t, &r.flags)
				r.deps.builder = func(daemonimage.Builder) imageBuilder {
					t.Error("the builder was reached after a refusal")
					return fakeImageBuilder{calls: &[]string{}}
				}

				err := runDaemonImage(context.Background(), r.flags, r.deps)

				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.want)
				}
				if r.out.Len() != 0 {
					t.Errorf("a refusal printed a plan:\n%s", r.out.String())
				}
			})
		}
	}
}

// The docker build argv names the explicit context and the process environment
// that reaches docker has neither DOCKER_CONTEXT nor DOCKER_HOST, whatever the
// command's own environment holds: the operator's shell cannot swap the engine.
func TestDaemonImageDockerBuildIgnoresTheShellsDockerEnvironment(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	for _, name := range []string{"gt", "bd", "om", "claude", "hm", "devops-reporter", "release-reporter", "openshell", "docker", "kubectl"} {
		if err := os.WriteFile(filepath.Join(r.flags.out, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(r.flags.out, "mango"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"mango", "mango.sha"} {
		if err := os.WriteFile(filepath.Join(r.flags.out, "mango", name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	pluginBinary := func(name string) string {
		dir := filepath.Join(r.flags.out, "plugins", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	var gotArgv, gotEnv []string
	r.deps.run = func(_ context.Context, _ string, env, argv []string) error {
		gotArgv, gotEnv = argv, env
		return nil
	}
	r.deps.environ = []string{"PATH=/usr/bin", "DOCKER_CONTEXT=other", "DOCKER_HOST=tcp://elsewhere:2375", "HOME=/home/x"}
	pins, err := readImagePins(r.flags.pins)
	if err != nil {
		t.Fatalf("readImagePins: %v", err)
	}
	b := newDaemonImageBuilder(r.flags, r.deps, pins)

	tag, err := b.BuildImage(context.Background(), imageTestRev, []string{
		filepath.Join(r.flags.out, "gt"), filepath.Join(r.flags.out, "bd"),
		filepath.Join(r.flags.out, "om"), filepath.Join(r.flags.out, "claude"),
		filepath.Join(r.flags.out, "hm"), filepath.Join(r.flags.out, "devops-reporter"), filepath.Join(r.flags.out, "release-reporter"),
		filepath.Join(r.flags.out, "openshell"), filepath.Join(r.flags.out, "docker"), filepath.Join(r.flags.out, "kubectl"),
		pluginBinary("app-promote"), pluginBinary("town-metrics"),
		filepath.Join(r.flags.out, "mango", "mango"), filepath.Join(r.flags.out, "mango", "mango.sha"),
	})
	if err != nil {
		t.Fatalf("BuildImage: %v", err)
	}

	if tag != "gt-daemon:"+imageTestRev {
		t.Errorf("tag = %q", tag)
	}
	if len(gotArgv) < 3 || gotArgv[0] != "docker" || gotArgv[1] != "--context" || gotArgv[2] != daemonimage.DockerContext {
		t.Errorf("docker argv = %v, want it to start docker --context %s", gotArgv, daemonimage.DockerContext)
	}
	for _, kv := range gotEnv {
		if strings.HasPrefix(kv, "DOCKER_CONTEXT=") || strings.HasPrefix(kv, "DOCKER_HOST=") {
			t.Errorf("docker ran with %s from the shell", strings.SplitN(kv, "=", 2)[0])
		}
	}
}

// The command parses its flags and hands them over, with --apply off by default.
func TestDaemonImageFlagsAreWired(t *testing.T) {
	t.Parallel()
	var got daemonImageFlags
	c := newDaemonImageCmd(func(_ context.Context, _ io.Writer, f daemonImageFlags) error {
		got = f
		return nil
	})
	var _ *cobra.Command = c
	c.SetArgs([]string{"--rev", "abc1234", "--out", "/o", "--gt-src", "/g", "--bd-src", "/b", "--om-src", "/m", "--hm-src", "/h", "--devops-src", "/d", "--mango-src", "/mg", "--mango-commit", "aabbccdd", "--pins", "/p"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := daemonImageFlags{rev: "abc1234", out: "/o", gtSrc: "/g", bdSrc: "/b", omSrc: "/m", hmSrc: "/h", devopsSrc: "/d", mangoSrc: "/mg", mangoCommit: "aabbccdd", pins: "/p"}
	if got != want {
		t.Errorf("flags = %+v, want %+v (apply off by default)", got, want)
	}
	c2 := newDaemonImageCmd(func(_ context.Context, _ io.Writer, f daemonImageFlags) error { got = f; return nil })
	c2.SetArgs([]string{"--apply"})
	c2.SilenceUsage, c2.SilenceErrors = true, true
	if err := c2.Execute(); err != nil || !got.apply {
		t.Errorf("--apply: err %v, apply %v, want it set", err, got.apply)
	}
}

// With --pins the command hands the Builder the four commits from the file: om
// is built at its pinned commit, never at its checkout's HEAD, and the three
// commit flags it replaced are unknown flags.
func TestDaemonImagePinsFlag(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	r.flags.apply = true
	var built daemonimage.Builder
	r.deps.builder = func(b daemonimage.Builder) imageBuilder {
		built = b
		return fakeImageBuilder{calls: &[]string{}}
	}

	if err := runDaemonImage(context.Background(), r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonImage: %v", err)
	}

	for _, tc := range []struct{ name, got, want string }{
		{"bd", built.BDCommit, imageTestBDCommit},
		{"om", built.OMCommit, imageTestOMCommit},
		{"hm", built.HMCommit, imageTestHMCommit},
		{"devops", built.DevopsCommit, imageTestDevCommit},
	} {
		if tc.got != tc.want {
			t.Errorf("%s commit = %q, want the pin %q", tc.name, tc.got, tc.want)
		}
	}
	omLine := ""
	for _, line := range strings.Split(r.out.String(), "\n") {
		if strings.HasPrefix(line, "build om: ") {
			omLine = line
		}
	}
	if !strings.Contains(omLine, "at "+imageTestOMCommit) {
		t.Errorf("plan om line = %q, want it to build at the pinned commit %s", omLine, imageTestOMCommit)
	}
	for _, flag := range []string{"--bd-commit", "--hm-commit", "--devops-commit"} {
		c := newDaemonImageCmd(func(context.Context, io.Writer, daemonImageFlags) error { return nil })
		c.SetArgs([]string{flag, imageTestBDCommit})
		c.SilenceUsage, c.SilenceErrors = true, true
		if err := c.Execute(); err == nil || !strings.Contains(err.Error(), "unknown flag") {
			t.Errorf("%s: err %v, want the flag to be unknown", flag, err)
		}
	}
}

// A stale pin stops --apply before the plan is printed or a build step runs:
// the pin-drift guard's error names the pin, and the builder is never reached.
func TestDaemonImageRefusesAStalePinBeforeBuilding(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	r.flags.apply = true
	bySrc := map[string]string{
		r.flags.bdSrc: imageTestBDCommit, r.flags.omSrc: imageTestOMCommit,
		r.flags.hmSrc: imageTestHMCommit, r.flags.devopsSrc: imageTestDevCommit,
	}
	otherMain := strings.Repeat("9", 40)
	r.deps.git = func(dir string, args ...string) ([]byte, error) {
		if len(args) == 3 && args[0] == "rev-parse" && args[1] == "--verify" && args[2] == "origin/main^{commit}" {
			if dir == r.flags.bdSrc {
				return []byte(otherMain + "\n"), nil
			}
			return []byte(bySrc[dir] + "\n"), nil
		}
		if dir == r.flags.bdSrc && len(args) > 0 && args[0] == "merge-base" {
			return nil, errors.New("exit status 1")
		}
		return nil, errors.New("unexpected git call")
	}
	r.deps.builder = func(daemonimage.Builder) imageBuilder {
		t.Error("the builder was reached after a stale pin")
		return fakeImageBuilder{calls: &[]string{}}
	}

	err := runDaemonImage(context.Background(), r.flags, r.deps)

	if err == nil || !strings.Contains(err.Error(), "pin-drift guard:") || !strings.Contains(err.Error(), "not an ancestor of main") {
		t.Fatalf("runDaemonImage error = %v, want the pin-drift guard's refusal", err)
	}
	if out := r.out.String(); out != "" {
		t.Errorf("a stale pin printed a plan:\n%s", out)
	}
}

// A dry run names the pins file and each pin, and runs nothing, writes nothing
// and reaches no network: the rig's seams fail the test if they are used.
func TestDaemonImagePlanNamesThePins(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	r.deps.git = func(string, ...string) ([]byte, error) {
		t.Error("a dry run ran git")
		return nil, nil
	}

	if err := runDaemonImage(context.Background(), r.flags, r.deps); err != nil {
		t.Fatalf("runDaemonImage: %v", err)
	}

	out := r.out.String()
	for _, want := range []string{
		"pins: " + r.flags.pins,
		"pin bd: http://forgejo:3000/sloan/beads.git at " + imageTestBDCommit,
		"pin om: http://forgejo:3000/sloan/organic-mechanic.git at " + imageTestOMCommit,
		"pin hm: http://forgejo:3000/sloan/history-man.git at " + imageTestHMCommit,
		"pin devops: http://forgejo:3000/sloan/devops.git at " + imageTestDevCommit,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	entries, err := os.ReadDir(r.flags.out)
	if err != nil || len(entries) != 0 {
		t.Errorf("--out holds %d entries after a dry run (err %v), want none", len(entries), err)
	}
}

// A pins file that cannot be read is refused when it is read, not treated as
// an empty pin set.
func TestReadImagePinsRefusesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "absent")
	if _, err := readImagePins(path); err == nil || !strings.Contains(err.Error(), "cannot be read") {
		t.Fatalf("readImagePins = %v, want an error naming the unreadable file", err)
	}
}

// A pin with no --*-src checkout is an error, never a guess at a directory.
func TestPinsMainOfRefusesAPinWithNoCheckout(t *testing.T) {
	t.Parallel()
	f := daemonImageFlags{omSrc: "/om", hmSrc: "/hm", devopsSrc: "/devops"}
	for _, pin := range []daemonimage.Pin{{Name: "bd"}, {Name: "tracker"}} {
		if _, err := pinsMainOf(f)(pin); err == nil || !strings.Contains(err.Error(), "no source checkout named") {
			t.Errorf("pinsMainOf(%s) = %v, want the no-checkout refusal", pin.Name, err)
		}
	}
}

// doerFunc is an HTTP client that is a function.
type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(req *http.Request) (*http.Response, error) { return f(req) }

// The fetcher returns the body of a 200 and an error for any other status, a
// client that fails and a URL that cannot be a request. No network is reached:
// the client is a fake.
func TestImageFetcher(t *testing.T) {
	t.Parallel()
	respond := func(code int, body string) doerFunc {
		return func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader(body))}, nil
		}
	}

	body, err := imageFetcher(respond(http.StatusOK, "payload"))(context.Background(), "https://example.invalid/ok")
	if err != nil {
		t.Fatalf("200: %v", err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "payload" {
		t.Errorf("body = %q", got)
	}
	if _, err := imageFetcher(respond(http.StatusNotFound, "no"))(context.Background(), "https://example.invalid/absent"); err == nil || !strings.Contains(err.Error(), "Not Found") {
		t.Errorf("404 = %v, want an error naming the status", err)
	}
	failing := doerFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("connection refused") })
	if _, err := imageFetcher(failing)(context.Background(), "https://example.invalid/x"); err == nil {
		t.Error("a failing client was not an error")
	}
	if _, err := imageFetcher(respond(http.StatusOK, ""))(context.Background(), "https://bad host/"); err == nil {
		t.Error("a URL that cannot be a request was not an error")
	}
}
