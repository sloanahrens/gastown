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
// checkout or writes a directory the operator did not name. The bd, om, hm and
// devops commits come from the pins file; the mango commit is a flag of its own,
// because mango is not one of the pinned sources.
type daemonImageFlags struct {
	rev         string // the gt commit the image is built at and tagged with
	out         string // the absolute directory the binaries and the build context live in
	gtSrc       string
	bdSrc       string
	omSrc       string
	hmSrc       string
	devopsSrc   string
	mangoSrc    string
	mangoCommit string
	pins        string // the absolute image-pins file naming the bd, om, hm and devops commits
	apply       bool
}

// imageBuilder is the build's steps, in the order they run. A
// daemonimage.Builder is one; a test stands in a recording fake.
type imageBuilder interface {
	BuildBinaries(ctx context.Context, rev string) ([]string, error)
	BuildPlugins(ctx context.Context, rev string) ([]string, error)
	BuildTools(ctx context.Context) ([]string, error)
	BuildMango(ctx context.Context) ([]string, error)
	FetchClaudeCLI(ctx context.Context) (string, error)
	FetchClientTools(ctx context.Context) ([]string, error)
	BuildImage(ctx context.Context, rev string, bins []string) (string, error)
}

// daemonImageDeps is what the command takes from the host: how commands run, git,
// the network, the environment the builds start from, how a path resolves, and
// where output goes. The tests supply fakes, so none of them runs a build or
// starts Docker.
type daemonImageDeps struct {
	run          daemonimage.Runner
	git          bdbuild.Git
	goTool       bdbuild.Go
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
		goTool:       bdbuild.ExecGo,
		fetch:        imageFetcher(&http.Client{Timeout: imageFetchCeiling}),
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

// httpDoer is the one call a fetch makes of an HTTP client; a test stands in a
// fake, so no unit test reaches a network.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// imageFetcher is the daemonimage.Fetcher over client: it downloads url and
// returns its body, and a response that is not 200 is an error. daemonimage
// verifies what it returns against a pinned sha256.
func imageFetcher(client httpDoer) daemonimage.Fetcher {
	return func(ctx context.Context, url string) (io.ReadCloser, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
		}
		return resp.Body, nil
	}
}

// newDaemonImageCmd builds `gt daemon image` over run, which receives the flags
// the command parsed.
func newDaemonImageCmd(run func(ctx context.Context, out io.Writer, f daemonImageFlags) error) *cobra.Command {
	var f daemonImageFlags
	cmd := &cobra.Command{
		Use:   "image",
		Short: "Build the daemon's container image (a dry run unless --apply)",
		Long: `Build gt-daemon:<rev>, the image the town's daemon runs in.

It builds gt, bd and om for linux/arm64 in clean worktrees of the pinned commits,
the plugins the container daemon runs (app-promote and town-metrics),
builds hm and the devops reporters (devops-reporter, release-reporter) from
their pinned checkouts, builds mango for linux/arm64 in a container from
--mango-src at --mango-commit (its go-api links DuckDB through cgo, so it is not
cross-compiled here), downloads the pinned claude CLI, and builds the image with
docker --context desktop-linux. The bd, om, hm and devops commits are read from
--pins, the image-pins file the repository carries; the --*-src checkouts must
hold them, and the hm and devops checkouts must be at their commits and clean.
The Dockerfile is read from --gt-src's working tree.

Every flag is required and none has a default read from the live town. Without
--apply it validates them, prints the pins and the commands it would run and runs
none, writes nothing and reaches no network. With --apply the pin-drift guard reads each
source checkout's origin/main before anything builds and refuses a pin that is
not an ancestor of it, more than 40 commits behind it, or more than 7 days older
than its tip. A path that is a canonical clone (.../mayor/rig) is refused for
--out and every source: the build adds worktrees to its sources.`,
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
	fl.StringVar(&f.omSrc, "om-src", "", "absolute om checkout")
	fl.StringVar(&f.hmSrc, "hm-src", "", "absolute hm checkout")
	fl.StringVar(&f.devopsSrc, "devops-src", "", "absolute devops checkout the two reporters are built from")
	fl.StringVar(&f.mangoSrc, "mango-src", "", "absolute mango checkout the image's mango is built from")
	fl.StringVar(&f.mangoCommit, "mango-commit", "", "mango commit to build the image's mango at (7+ hex characters)")
	fl.StringVar(&f.pins, "pins", "", "absolute image-pins file naming the bd, om, hm and devops commits")
	fl.BoolVar(&f.apply, "apply", false, "run the build; without it only print the plan")
	return cmd
}

var daemonImageCmd = newDaemonImageCmd(func(ctx context.Context, out io.Writer, f daemonImageFlags) error {
	return runDaemonImage(ctx, f, realDaemonImageDeps(out))
})

func init() {
	daemonCmd.AddCommand(daemonImageCmd)
}

// runDaemonImage validates the flags, reads the pins file, prints the plan, and
// with --apply runs the pin-drift guard and then the build: the binaries, the
// plugins, the tools, mango, the claude CLI, the client tools, the image, in
// that order, stopping at the first failure.
func runDaemonImage(ctx context.Context, f daemonImageFlags, d daemonImageDeps) error {
	if err := validateDaemonImageFlags(f, d.evalSymlinks); err != nil {
		return err
	}
	pins, err := readImagePins(f.pins)
	if err != nil {
		return err
	}
	b := newDaemonImageBuilder(f, d, pins)
	if f.apply {
		notes, err := daemonimage.CheckPins(pins, pinsMainOf(f), f.pins, d.git)
		if err != nil {
			return err
		}
		for _, note := range notes {
			fmt.Fprintln(d.out, note)
		}
	}
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
	plugins, err := build.BuildPlugins(ctx, f.rev)
	if err != nil {
		return err
	}
	bins = append(bins, plugins...)
	hmTools, err := build.BuildTools(ctx)
	if err != nil {
		return err
	}
	bins = append(bins, hmTools...)
	mango, err := build.BuildMango(ctx)
	if err != nil {
		return err
	}
	bins = append(bins, mango...)
	claude, err := build.FetchClaudeCLI(ctx)
	if err != nil {
		return err
	}
	tools, err := build.FetchClientTools(ctx)
	if err != nil {
		return err
	}
	tag, err := build.BuildImage(ctx, f.rev, append(append(bins, claude), tools...))
	if err != nil {
		return err
	}
	fmt.Fprintln(d.out, tag)
	return nil
}

// readImagePins reads and parses the pins file at path. A file that cannot be
// read and one the parser refuses are both errors naming the path, so the
// operator knows which file to fix.
func readImagePins(path string) ([]daemonimage.Pin, error) {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("--pins %q cannot be read: %w", path, err)
	}
	pins, err := daemonimage.ParsePins(body)
	if err != nil {
		return nil, fmt.Errorf("--pins %q: %w", path, err)
	}
	return pins, nil
}

// pinsMainOf maps each pin to the --*-src checkout the build takes as that
// source: the checkout whose origin/main the pin-drift guard reads. A pin with
// no checkout is an error, never a guess.
func pinsMainOf(f daemonImageFlags) daemonimage.MainOf {
	srcs := map[string]string{
		"bd": f.bdSrc, "om": f.omSrc, "hm": f.hmSrc, "devops": f.devopsSrc,
	}
	return func(p daemonimage.Pin) (string, error) {
		dir, ok := srcs[p.Name]
		if !ok || dir == "" {
			return "", fmt.Errorf("no source checkout named for the %s pin; pass --%s-src", p.Name, p.Name)
		}
		return dir, nil
	}
}

// newDaemonImageBuilder is the Builder the flags and the pins describe, on the
// host's seams.
func newDaemonImageBuilder(f daemonImageFlags, d daemonImageDeps, pins []daemonimage.Pin) daemonimage.Builder {
	commits := make(map[string]string, len(pins))
	for _, p := range pins {
		commits[p.Name] = p.Commit
	}
	return daemonimage.Builder{
		Run:          d.run,
		Git:          d.git,
		Go:           d.goTool,
		Fetch:        d.fetch,
		Environ:      d.environ,
		Out:          f.out,
		GTSrc:        f.gtSrc,
		BDSrc:        f.bdSrc,
		BDCommit:     commits["bd"],
		OMSrc:        f.omSrc,
		OMCommit:     commits["om"],
		HMSrc:        f.hmSrc,
		HMCommit:     commits["hm"],
		DevopsSrc:    f.devopsSrc,
		DevopsCommit: commits["devops"],
		MangoSrc:     f.mangoSrc,
		MangoCommit:  f.mangoCommit,
		Pins:         pins,
		PinsPath:     f.pins,
		Dockerfile:   filepath.Join(f.gtSrc, daemonimage.DockerfilePath),
	}
}

// validateDaemonImageFlags refuses, before anything runs, a missing flag, a
// relative path, a path that is not an existing directory, a pins path that is
// not an existing file, and a source path that is or lies inside a canonical
// clone. The commit shapes and the pins file's syntax are the Builder's and the
// parser's to refuse; the plan asks them first.
func validateDaemonImageFlags(f daemonImageFlags, evalSymlinks func(string) (string, error)) error {
	for _, req := range []struct{ flag, value string }{
		{"--rev", f.rev}, {"--out", f.out}, {"--gt-src", f.gtSrc},
		{"--bd-src", f.bdSrc}, {"--om-src", f.omSrc},
		{"--hm-src", f.hmSrc}, {"--devops-src", f.devopsSrc},
		{"--mango-src", f.mangoSrc}, {"--mango-commit", f.mangoCommit}, {"--pins", f.pins},
	} {
		if strings.TrimSpace(req.value) == "" {
			return fmt.Errorf("%s is required: nothing is inferred from the live town", req.flag)
		}
	}
	for _, p := range []struct{ flag, value string }{
		{"--out", f.out}, {"--gt-src", f.gtSrc}, {"--bd-src", f.bdSrc}, {"--om-src", f.omSrc},
		{"--hm-src", f.hmSrc}, {"--devops-src", f.devopsSrc}, {"--mango-src", f.mangoSrc},
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
		if daemonimage.IsCanonicalClone(resolved) {
			return fmt.Errorf("%s %q is or lies inside a canonical clone (.../mayor/rig): the build adds worktrees to its sources and the canonical clone is never touched", p.flag, p.value)
		}
	}
	if !filepath.IsAbs(f.pins) {
		return fmt.Errorf("--pins %q is not absolute: name the image-pins file outright", f.pins)
	}
	if info, err := os.Stat(f.pins); err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("--pins %q is not an existing file", f.pins)
	}
	return nil
}
