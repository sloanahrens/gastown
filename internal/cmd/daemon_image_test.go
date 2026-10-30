package cmd

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemonimage"
)

const (
	imageTestRev      = "0123456789abcdef0123456789abcdef01234567"
	imageTestBDCommit = "fedcba9876543210fedcba9876543210fedcba98"
)

// imageRig is a set of real directories for the command's paths and deps that
// fail the test if a build seam is used: a dry run and a refusal run nothing.
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
	r := &imageRig{out: &bytes.Buffer{}}
	r.flags = daemonImageFlags{
		rev: imageTestRev, out: mk("out"), gtSrc: gtSrc, bdSrc: mk("beads"),
		bdCommit: imageTestBDCommit, omSrc: mk("om"),
	}
	r.deps = daemonImageDeps{
		run: func(context.Context, string, []string, []string) error {
			t.Error("a build command ran")
			return nil
		},
		git: func(string, ...string) ([]byte, error) {
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

// fakeImageBuilder records the three steps in order and fails the one named.
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

func (f fakeImageBuilder) FetchClaudeCLI(context.Context) (string, error) {
	if err := f.step("claude"); err != nil {
		return "", err
	}
	return "/out/claude", nil
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
		"build gt: go build -trimpath -ldflags",
		"-o " + filepath.Join(r.flags.out, "gt") + " ./cmd/gt",
		"build bd: go build -trimpath",
		"./cmd/bd",
		"build om: go build -trimpath -o " + filepath.Join(r.flags.out, "om") + " ./cmd/om",
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

// With --apply the steps run in order, the claude CLI joins the binaries the
// image is built from, a failing step stops the rest, and the tag is printed.
func TestDaemonImageApplyRunsTheStepsInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		failAt    string
		want      []string
		wantTag   bool
		wantError string
	}{
		{"all succeed", "", []string{"binaries " + imageTestRev, "claude", "image " + imageTestRev + " /out/gt,/out/bd,/out/om,/out/claude"}, true, ""},
		{"the binaries fail", "binaries " + imageTestRev, []string{"binaries " + imageTestRev}, false, "failed"},
		{"the claude CLI fails", "claude", []string{"binaries " + imageTestRev, "claude"}, false, "claude failed"},
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
	t.Run("the image build fails", func(t *testing.T) {
		t.Parallel()
		r := newImageRig(t)
		r.flags.apply = true
		var calls []string
		r.deps.builder = func(daemonimage.Builder) imageBuilder {
			return fakeImageBuilder{calls: &calls, failAt: "image " + imageTestRev + " /out/gt,/out/bd,/out/om,/out/claude"}
		}
		if err := runDaemonImage(context.Background(), r.flags, r.deps); err == nil {
			t.Fatal("a failed image build reported success")
		}
		if strings.Contains(r.out.String(), "gt-daemon:") && !strings.Contains(r.out.String(), "image: docker") {
			t.Errorf("a tag was printed for a failed build:\n%s", r.out.String())
		}
	})
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
		{"no --bd-commit", func(_ *testing.T, f *daemonImageFlags) { f.bdCommit = "" }, "--bd-commit is required"},
		{"no --om-src", func(_ *testing.T, f *daemonImageFlags) { f.omSrc = "" }, "--om-src is required"},
		{"a relative --out", func(_ *testing.T, f *daemonImageFlags) { f.out = "out" }, "--out \"out\" is not absolute"},
		{"a relative --gt-src", func(_ *testing.T, f *daemonImageFlags) { f.gtSrc = "gt" }, "--gt-src \"gt\" is not absolute"},
		{"a relative --bd-src", func(_ *testing.T, f *daemonImageFlags) { f.bdSrc = "beads" }, "--bd-src \"beads\" is not absolute"},
		{"a relative --om-src", func(_ *testing.T, f *daemonImageFlags) { f.omSrc = "om" }, "--om-src \"om\" is not absolute"},
		{"a rev that is not hex", func(_ *testing.T, f *daemonImageFlags) { f.rev = "main" }, "is not a 7+ character hex commit"},
		{"a bd commit that is not hex", func(_ *testing.T, f *daemonImageFlags) { f.bdCommit = "main" }, "is not a 7+ character hex commit"},
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

// A path that is not itself a rig's canonical clone is not refused for its name
// alone: only mayor/rig is.
func TestInsideCanonicalClone(t *testing.T) {
	t.Parallel()
	for path, want := range map[string]bool{
		"/town/gastown/mayor/rig":      true,
		"/town/gastown/mayor/rig/sub":  true,
		"/town/gastown/crew/seagull":   false,
		"/town/gastown/mayor":          false,
		"/town/rig":                    false,
		"/elsewhere/mayor/rigs":        false,
		"/":                            false,
		"/town/gastown/refinery/rig":   false,
		"/scratch.noindex/l1/build":    false,
		"/town/gastown/mayor/rig/a/b/": true,
	} {
		if got := insideCanonicalClone(path); got != want {
			t.Errorf("insideCanonicalClone(%q) = %v, want %v", path, got, want)
		}
	}
}

// The docker build argv names the explicit context and the process environment
// that reaches docker has neither DOCKER_CONTEXT nor DOCKER_HOST, whatever the
// command's own environment holds: the operator's shell cannot swap the engine.
func TestDaemonImageDockerBuildIgnoresTheShellsDockerEnvironment(t *testing.T) {
	t.Parallel()
	r := newImageRig(t)
	for _, name := range []string{"gt", "bd", "om", "claude"} {
		if err := os.WriteFile(filepath.Join(r.flags.out, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var gotArgv, gotEnv []string
	r.deps.run = func(_ context.Context, _ string, env, argv []string) error {
		gotArgv, gotEnv = argv, env
		return nil
	}
	r.deps.environ = []string{"PATH=/usr/bin", "DOCKER_CONTEXT=other", "DOCKER_HOST=tcp://elsewhere:2375", "HOME=/home/x"}
	b := newDaemonImageBuilder(r.flags, r.deps)

	tag, err := b.BuildImage(context.Background(), imageTestRev, []string{
		filepath.Join(r.flags.out, "gt"), filepath.Join(r.flags.out, "bd"),
		filepath.Join(r.flags.out, "om"), filepath.Join(r.flags.out, "claude"),
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
	c.SetArgs([]string{"--rev", "abc1234", "--out", "/o", "--gt-src", "/g", "--bd-src", "/b", "--bd-commit", "def5678", "--om-src", "/m"})
	c.SilenceUsage, c.SilenceErrors = true, true
	if err := c.Execute(); err != nil {
		t.Fatalf("execute: %v", err)
	}
	want := daemonImageFlags{rev: "abc1234", out: "/o", gtSrc: "/g", bdSrc: "/b", bdCommit: "def5678", omSrc: "/m"}
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

// httpImageFetch returns the body of a 200 and an error for anything else, an
// unreachable server and a URL that cannot be a request.
func TestHTTPImageFetch(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/ok" {
			_, _ = io.WriteString(w, "payload")
			return
		}
		http.NotFound(w, req)
	}))
	defer srv.Close()

	body, err := httpImageFetch(context.Background(), srv.URL+"/ok")
	if err != nil {
		t.Fatalf("200: %v", err)
	}
	got, _ := io.ReadAll(body)
	_ = body.Close()
	if string(got) != "payload" {
		t.Errorf("body = %q", got)
	}
	if _, err := httpImageFetch(context.Background(), srv.URL+"/absent"); err == nil || !strings.Contains(err.Error(), "404") {
		t.Errorf("404 = %v, want an error naming the status", err)
	}
	if _, err := httpImageFetch(context.Background(), "http://127.0.0.1:1/"); err == nil {
		t.Error("an unreachable server was not an error")
	}
	if _, err := httpImageFetch(context.Background(), "http://bad host/"); err == nil {
		t.Error("a URL that cannot be a request was not an error")
	}
}
