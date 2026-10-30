package cmd

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/daemonimage"
	"github.com/steveyegge/gastown/internal/polecat/bdbuild"
)

// imageFetchCeiling bounds the one download an image build makes, the claude CLI,
// which is a few hundred megabytes.
const imageFetchCeiling = 15 * time.Minute

// daemonImageFlags are the values `gt daemon image` parsed. Every one is
// required: nothing is inferred from the live town, so a build never reads a
// checkout or writes a directory the operator did not name.
type daemonImageFlags struct {
	rev      string // the gt commit the image is built at and tagged with
	out      string // the absolute directory the binaries and the build context live in
	gtSrc    string
	bdSrc    string
	omSrc    string
	bdCommit string
	apply    bool
}

// imageBuilder is the three steps of a build, in the order they run. A
// daemonimage.Builder is one; a test stands in a recording fake.
type imageBuilder interface {
	BuildBinaries(ctx context.Context, rev string) ([]string, error)
	FetchClaudeCLI(ctx context.Context) (string, error)
	BuildImage(ctx context.Context, rev string, bins []string) (string, error)
}

// daemonImageDeps is what the command takes from the host: how commands run, git,
// the network, the environment the builds start from, how a path resolves, and
// where output goes. The tests supply fakes, so none of them runs a build or
// starts Docker.
type daemonImageDeps struct {
	run          daemonimage.Runner
	git          bdbuild.Git
	fetch        daemonimage.Fetcher
	environ      []string
	evalSymlinks func(path string) (string, error)
	out          io.Writer
	// builder wraps the configured Builder for --apply; nil uses it as it is.
	builder func(daemonimage.Builder) imageBuilder
}

// realDaemonImageDeps is the host's.
func realDaemonImageDeps(out io.Writer) daemonImageDeps {
	return daemonImageDeps{
		run:          execImageRunner,
		git:          bdbuild.ExecGit,
		fetch:        httpImageFetch,
		environ:      os.Environ(),
		evalSymlinks: filepath.EvalSymlinks,
		out:          out,
	}
}

// execImageRunner runs one build command in dir with env as its whole
// environment, its output going to this process's streams.
func execImageRunner(ctx context.Context, dir string, env, argv []string) error {
	c := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // argv is built by daemonimage from fixed names and validated paths
	c.Dir = dir
	c.Env = env
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// httpImageFetch downloads url and returns its body. A response that is not 200
// is an error. daemonimage verifies what it returns against a pinned sha256.
func httpImageFetch(ctx context.Context, url string) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{Timeout: imageFetchCeiling}).Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}

// newDaemonImageCmd builds `gt daemon image` over run, which receives the flags
// the command parsed.
func newDaemonImageCmd(run func(ctx context.Context, out io.Writer, f daemonImageFlags) error) *cobra.Command {
	var f daemonImageFlags
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Build the daemon's container image (a dry run unless --apply)",
		Long: `Build gt-daemon:<rev>, the image the town's daemon runs in.

It builds gt, bd and om for linux/arm64 in clean worktrees of the commits named,
downloads the pinned claude CLI, and builds the image with docker --context
desktop-linux. The Dockerfile is read from --gt-src's working tree.

Every flag is required and none has a default read from the live town. Without
--apply it validates them, prints the commands it would run and runs none, writes
nothing and reaches no network. A path that is a canonical clone (.../mayor/rig)
is refused for --out and every source: the build adds worktrees to its sources.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	fl := cmd.Flags()
	fl.StringVar(&f.rev, "rev", "", "gt commit to build and tag the image with (7+ hex characters)")
	fl.StringVar(&f.out, "out", "", "existing absolute directory for the binaries and the build context")
	fl.StringVar(&f.gtSrc, "gt-src", "", "absolute gt checkout")
	fl.StringVar(&f.bdSrc, "bd-src", "", "absolute beads checkout")
	fl.StringVar(&f.bdCommit, "bd-commit", "", "beads commit to build bd at (7+ hex characters)")
	fl.StringVar(&f.omSrc, "om-src", "", "absolute om checkout (built from its HEAD)")
	fl.BoolVar(&f.apply, "apply", false, "run the build; without it only print the plan")
	return cmd
}

var daemonImageCmd = newDaemonImageCmd(func(ctx context.Context, out io.Writer, f daemonImageFlags) error {
	return runDaemonImage(ctx, f, realDaemonImageDeps(out))
})

func init() {
	daemonCmd.AddCommand(daemonImageCmd)
}

// runDaemonImage validates the flags, prints the plan, and with --apply runs the
// build: the binaries, the claude CLI, the image, in that order, stopping at the
// first failure.
func runDaemonImage(ctx context.Context, f daemonImageFlags, d daemonImageDeps) error {
	if err := validateDaemonImageFlags(f, d.evalSymlinks); err != nil {
		return err
	}
	b := newDaemonImageBuilder(f, d)
	plan, err := b.Plan(f.rev)
	if err != nil {
		return err
	}
	for _, line := range plan {
		fmt.Fprintln(d.out, line)
	}
	if !f.apply {
		fmt.Fprintln(d.out, "dry run: nothing was run. Pass --apply to build.")
		return nil
	}
	var build imageBuilder = b
	if d.builder != nil {
		build = d.builder(b)
	}
	bins, err := build.BuildBinaries(ctx, f.rev)
	if err != nil {
		return err
	}
	claude, err := build.FetchClaudeCLI(ctx)
	if err != nil {
		return err
	}
	tag, err := build.BuildImage(ctx, f.rev, append(bins, claude))
	if err != nil {
		return err
	}
	fmt.Fprintln(d.out, tag)
	return nil
}

// newDaemonImageBuilder is the Builder the flags describe, on the host's seams.
func newDaemonImageBuilder(f daemonImageFlags, d daemonImageDeps) daemonimage.Builder {
	return daemonimage.Builder{
		Run:        d.run,
		Git:        d.git,
		Fetch:      d.fetch,
		Environ:    d.environ,
		Out:        f.out,
		GTSrc:      f.gtSrc,
		BDSrc:      f.bdSrc,
		BDCommit:   f.bdCommit,
		OMSrc:      f.omSrc,
		Dockerfile: filepath.Join(f.gtSrc, daemonimage.DockerfilePath),
	}
}

// validateDaemonImageFlags refuses, before anything runs, a missing flag, a
// relative path, a path that is not an existing directory, and a path that is
// or lies inside a canonical clone. The commit shapes are the Builder's to
// refuse; the plan asks it first.
func validateDaemonImageFlags(f daemonImageFlags, evalSymlinks func(string) (string, error)) error {
	for _, req := range []struct{ flag, value string }{
		{"--rev", f.rev}, {"--out", f.out}, {"--gt-src", f.gtSrc},
		{"--bd-src", f.bdSrc}, {"--bd-commit", f.bdCommit}, {"--om-src", f.omSrc},
	} {
		if strings.TrimSpace(req.value) == "" {
			return fmt.Errorf("%s is required: nothing is inferred from the live town", req.flag)
		}
	}
	for _, p := range []struct{ flag, value string }{
		{"--out", f.out}, {"--gt-src", f.gtSrc}, {"--bd-src", f.bdSrc}, {"--om-src", f.omSrc},
	} {
		if !filepath.IsAbs(p.value) {
			return fmt.Errorf("%s %q is not absolute: a build runs in a worktree, not in this directory", p.flag, p.value)
		}
		resolved, err := evalSymlinks(p.value)
		if err != nil {
			return fmt.Errorf("%s %q cannot be resolved: %w", p.flag, p.value, err)
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			return fmt.Errorf("%s %q is not an existing directory", p.flag, p.value)
		}
		if insideCanonicalClone(resolved) {
			return fmt.Errorf("%s %q is or lies inside a canonical clone (.../mayor/rig): the build adds worktrees to its sources and the canonical clone is never touched", p.flag, p.value)
		}
	}
	return nil
}

// insideCanonicalClone reports whether path is a rig's canonical clone,
// <town>/<rig>/mayor/rig, or lies under one.
func insideCanonicalClone(path string) bool {
	for p := filepath.Clean(path); ; p = filepath.Dir(p) {
		if filepath.Base(p) == "rig" && filepath.Base(filepath.Dir(p)) == "mayor" {
			return true
		}
		if parent := filepath.Dir(p); parent == p {
			return false
		}
	}
}
