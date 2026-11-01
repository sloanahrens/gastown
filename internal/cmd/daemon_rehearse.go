package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
	"github.com/steveyegge/gastown/internal/daemonimage"
)

// The container-config rehearsal (gt-unkq3). The 2026-11-01 L3 attempt found its
// first container-only failure (no time zone database) inside the window. The
// rehearsal finds every such failure beforehand: it stages a scratch copy of the
// live town's settings (daemon.env values blanked), checks the image against that
// copy, starts the image's entrypoint on the copy to its first heartbeat, and
// lists ALL failures, not the first.
//
// Read-only against the live town: the only reads of it are the settings files
// and mayor/town.json, and every write, mount and container is the scratch
// copy's. The entrypoint container runs with no network, so it cannot reach the
// live Dolt server whatever the copied settings name.

// Rehearsal defaults and names.
const (
	rehearseDefaultBudget = 3 * time.Minute
	rehearsePoll          = 2 * time.Second
	rehearseAddrPattern   = `host\.docker\.internal:[0-9]+`
	// rehearseTailLines is how much of the entrypoint container's log a failed
	// heartbeat prints.
	rehearseTailLines = "40"
)

// rehearseBinaries are the programs the settings and the daemon exec; each must
// be on the image's PATH.
var rehearseBinaries = []string{"docker", "kubectl", "openshell", "git", "tmux", "dolt", "claude"}

var rehearseAddr = regexp.MustCompile(rehearseAddrPattern)

// rehearseFlags are the values `gt daemon rehearse` parsed.
type rehearseFlags struct {
	liveRoot string
	root     string // the scratch town; defaults under <live>/.runtime/rehearsal
	image    string
	budget   time.Duration
}

// rehearseDeps is what the rehearsal takes from the host. A test supplies fakes,
// so no unit test starts Docker, touches a town or waits on a real clock.
type rehearseDeps struct {
	run      daemonimage.Runner
	environ  []string
	out      io.Writer
	readFile func(path string) ([]byte, error)
	readDir  func(path string) ([]os.DirEntry, error)
	mkdirAll func(path string, perm os.FileMode) error
	write    func(path string, data []byte, perm os.FileMode) error
	now      func() time.Time
	sleep    func(time.Duration)
}

func realRehearseDeps(out io.Writer) rehearseDeps {
	return rehearseDeps{
		run: execImageRunner, environ: os.Environ(), out: out,
		readFile: os.ReadFile, readDir: os.ReadDir, mkdirAll: os.MkdirAll, write: os.WriteFile,
		now: time.Now, sleep: time.Sleep,
	}
}

// rehearsalCheck is one row of the rehearsal's table: what it inspects, whether
// a failure fails the rehearsal, and the check itself, which returns one line
// per failure.
type rehearsalCheck struct {
	name     string
	inspects string
	required bool
	run      func(ctx context.Context, r *rehearsal) []string
}

// rehearsalChecks is the table, in the order it prints.
var rehearsalChecks = []rehearsalCheck{
	{"config-validate", "gt config validate loads the scratch settings inside the image", true, checkConfigValidates},
	{"binaries", "every program the settings and the daemon exec is on the image's PATH", true, checkImageBinaries},
	{"paths", "every absolute path a setting names exists in the image or lies in the town bind", true, checkSettingPaths},
	{"addresses", "every host.docker.internal address a setting names accepts a TCP connect from the image", true, checkSettingAddresses},
	{"heartbeat", "the entrypoint on the scratch town records a heartbeat within the budget", true, checkFirstHeartbeat},
}

// rehearsal is one run: the flags, the host seams and the staged settings text
// the checks read.
type rehearsal struct {
	f        rehearseFlags
	d        rehearseDeps
	settings string // the staged settings files, concatenated; what the path and address checks scan
}

func newDaemonRehearseCmd(run func(ctx context.Context, out io.Writer, f rehearseFlags) error) *cobra.Command {
	var f rehearseFlags
	cmd := &cobra.Command{
		Use:   "rehearse",
		Short: "Rehearse the daemon container's configuration on a scratch copy of the live settings",
		Long: `Stages a scratch copy of the live town's settings (daemon.env keeps its keys and
loses every value), checks the daemon image against it, starts the image's
entrypoint on the copy to its first heartbeat, and prints a table of every check
with its result. It lists all failures and exits non-zero if any required check
failed.

Read-only against the live town: it reads the settings files and mayor/town.json
and writes only under --root. The entrypoint container has no network, so it
cannot reach the live Dolt server; a heartbeat that fails for want of Dolt is
named in the container log tail it prints.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cmd.OutOrStdout(), f)
		},
	}
	cmd.Flags().StringVar(&f.liveRoot, "live-root", "", "absolute path of the live town (read only)")
	cmd.Flags().StringVar(&f.root, "root", "", "absolute scratch town root (default <live-root>/.runtime/rehearsal/town)")
	cmd.Flags().StringVar(&f.image, "image", "", "daemon image tag, gt-daemon:<rev>")
	cmd.Flags().DurationVar(&f.budget, "budget", rehearseDefaultBudget, "how long to wait for the first heartbeat")
	return cmd
}

var daemonRehearseCmd = newDaemonRehearseCmd(func(ctx context.Context, out io.Writer, f rehearseFlags) error {
	return runDaemonRehearse(ctx, f, realRehearseDeps(out))
})

func init() {
	daemonCmd.AddCommand(daemonRehearseCmd)
}

// runDaemonRehearse stages the scratch copy, runs every check, prints the table
// and fails if a required check did. Every refusal comes before anything is
// written.
func runDaemonRehearse(ctx context.Context, f rehearseFlags, d rehearseDeps) error {
	if f.liveRoot == "" || f.image == "" || !filepath.IsAbs(f.liveRoot) {
		return errors.New("--live-root (absolute) and --image are required")
	}
	if f.root == "" {
		f.root = filepath.Join(f.liveRoot, ".runtime", "rehearsal", "town")
	}
	if !filepath.IsAbs(f.root) || pathWithin(f.root, filepath.Join(f.liveRoot, "settings")) ||
		pathWithin(f.root, filepath.Join(f.liveRoot, "mayor")) || filepath.Clean(f.root) == filepath.Clean(f.liveRoot) {
		return fmt.Errorf("--root %q must be an absolute directory that is not the live town or its settings or mayor directory", f.root)
	}
	r := &rehearsal{f: f, d: d}
	if err := r.stage(); err != nil {
		return err
	}
	failed := 0
	for _, c := range rehearsalChecks {
		fails := c.run(ctx, r)
		status := "ok"
		if len(fails) > 0 {
			status = "FAIL"
			if c.required {
				failed++
			}
		}
		_, _ = fmt.Fprintf(d.out, "%-16s %-5s %s\n", c.name, status, c.inspects)
		for _, line := range fails {
			_, _ = fmt.Fprintf(d.out, "    %s\n", line)
		}
	}
	if failed > 0 {
		return fmt.Errorf("the container rehearsal failed %d required check(s); the table above lists each", failed)
	}
	_, _ = fmt.Fprintln(d.out, "the container rehearsal passed every required check")
	return nil
}

// stage writes the scratch copy: the live settings files (daemon.env with its
// values blanked) and mayor/town.json, all under r.f.root.
func (r *rehearsal) stage() error {
	src := filepath.Join(r.f.liveRoot, "settings")
	entries, err := r.d.readDir(src)
	if err != nil {
		return fmt.Errorf("read the live settings directory: %w", err)
	}
	files := map[string][]byte{}
	var text strings.Builder
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		data, err := r.d.readFile(filepath.Join(src, e.Name()))
		if err != nil {
			return fmt.Errorf("read the live setting %s: %w", e.Name(), err)
		}
		if e.Name() == "daemon.env" {
			data = blankEnvValues(data)
		}
		files[filepath.Join("settings", e.Name())] = data
		text.Write(data)
		text.WriteByte('\n')
	}
	town, err := r.d.readFile(filepath.Join(r.f.liveRoot, "mayor", "town.json"))
	if err != nil {
		return fmt.Errorf("read the live mayor/town.json: %w", err)
	}
	files[filepath.Join("mayor", "town.json")] = town
	r.settings = text.String()
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		dst := filepath.Join(r.f.root, n)
		if err := r.d.mkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return fmt.Errorf("create %s: %w", filepath.Dir(dst), err)
		}
		if err := r.d.write(dst, files[n], 0o600); err != nil {
			return fmt.Errorf("write %s: %w", dst, err)
		}
	}
	return nil
}

// blankEnvValues keeps every key of an env file and drops every value, so no
// secret is copied. Comments and blank lines are kept; "export KEY=value"
// becomes "export KEY=".
func blankEnvValues(data []byte) []byte {
	lines := strings.Split(string(data), "\n")
	for i, l := range lines {
		if t := strings.TrimSpace(l); t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if k, _, ok := strings.Cut(l, "="); ok {
			lines[i] = k + "="
		}
	}
	return []byte(strings.Join(lines, "\n"))
}

// docker runs one docker command; the context is the Docker Desktop one every
// image command uses.
func (r *rehearsal) docker(ctx context.Context, args ...string) error {
	argv := append([]string{"docker", "--context", daemonimage.DockerContext}, args...)
	return r.d.run(ctx, r.f.root, daemonimage.DockerEnv(r.d.environ), argv)
}

// inImage runs argv in a fresh container of the image with the scratch town
// bound read-only at its own path and GT_TOWN_ROOT naming it; network is the
// docker --network value.
func (r *rehearsal) inImage(ctx context.Context, network string, argv ...string) error {
	args := []string{"run", "--rm", "--network", network, "-v", r.f.root + ":" + r.f.root + ":ro",
		"-w", r.f.root, "-e", "GT_TOWN_ROOT=" + r.f.root, "--entrypoint", argv[0], r.f.image}
	return r.docker(ctx, append(args, argv[1:]...)...)
}

func checkConfigValidates(ctx context.Context, r *rehearsal) []string {
	if err := r.inImage(ctx, "none", "gt", "config", "validate"); err != nil {
		return []string{"gt config validate: " + err.Error()}
	}
	return nil
}

func checkImageBinaries(ctx context.Context, r *rehearsal) []string {
	var fails []string
	for _, b := range rehearseBinaries {
		if err := r.inImage(ctx, "none", "bash", "-c", "command -v "+b); err != nil {
			fails = append(fails, b+": not on the image's PATH")
		}
	}
	return fails
}

// settingPaths returns the distinct absolute paths the staged settings name, in
// order, skipping the town root and its children (the bind).
func (r *rehearsal) settingPaths() []string {
	seen := map[string]bool{}
	var paths []string
	for _, tok := range strings.FieldsFunc(r.settings, func(c rune) bool { return strings.ContainsRune("\"' ,=:\n\t[]{}", c) }) {
		if !strings.HasPrefix(tok, "/") || len(tok) < 2 || seen[tok] || pathWithin(tok, r.f.root) {
			continue
		}
		seen[tok] = true
		paths = append(paths, tok)
	}
	sort.Strings(paths)
	return paths
}

func checkSettingPaths(ctx context.Context, r *rehearsal) []string {
	var fails []string
	for _, p := range r.settingPaths() {
		if err := r.inImage(ctx, "none", "test", "-e", p); err != nil {
			fails = append(fails, p+": named by a setting, absent from the image and not in the town bind")
		}
	}
	return fails
}

func checkSettingAddresses(ctx context.Context, r *rehearsal) []string {
	var fails []string
	seen := map[string]bool{}
	for _, a := range rehearseAddr.FindAllString(r.settings, -1) {
		if seen[a] {
			continue
		}
		seen[a] = true
		host, port, _ := strings.Cut(a, ":")
		probe := "exec 3<>/dev/tcp/" + host + "/" + port
		if err := r.inImage(ctx, "bridge", "timeout", "5", "bash", "-c", probe); err != nil {
			fails = append(fails, a+": does not accept a TCP connect from the image")
		}
	}
	return fails
}

// checkFirstHeartbeat starts the image's own entrypoint on the scratch town with
// no network, waits at most the budget for state.json to record a heartbeat, and
// removes the container. On failure it prints the tail of the container's log.
func checkFirstHeartbeat(ctx context.Context, r *rehearsal) []string {
	name := "gt-rehearsal-" + filepath.Base(filepath.Dir(r.f.root))
	_ = r.docker(ctx, "rm", "-f", name) // a leftover from an earlier run; absent is the normal case
	defer func() { _ = r.docker(context.WithoutCancel(ctx), "rm", "-f", name) }()
	if err := r.docker(ctx, "run", "-d", "--name", name, "--network", "none",
		"-v", r.f.root+":"+r.f.root, "-e", config.ContainerModeEnv+"="+config.SupervisorContainer,
		"-e", "GT_TOWN_ROOT="+r.f.root, r.f.image); err != nil {
		return []string{"the entrypoint container did not start: " + err.Error()}
	}
	if r.waitForHeartbeat() {
		return nil
	}
	_ = r.docker(ctx, "logs", "--tail", rehearseTailLines, name)
	return []string{fmt.Sprintf("no heartbeat in %s; the container log tail is printed above", r.f.budget)}
}

// waitForHeartbeat polls the scratch town's state.json until it records a
// heartbeat or the budget is spent, and reports which.
func (r *rehearsal) waitForHeartbeat() bool {
	deadline := r.d.now().Add(r.f.budget)
	state := filepath.Join(r.f.root, "daemon", "state.json")
	for {
		if data, err := r.d.readFile(state); err == nil {
			var s daemon.State
			if json.Unmarshal(data, &s) == nil && !s.LastHeartbeat.IsZero() {
				return true
			}
		}
		if !r.d.now().Before(deadline) {
			return false
		}
		r.d.sleep(rehearsePoll)
	}
}
