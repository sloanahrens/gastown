package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/hostprobe"
	"github.com/steveyegge/gastown/internal/templates"
	"github.com/steveyegge/gastown/internal/workspace"
)

// hostAgentInterval is how often the agent rewrites the probe file. The
// container daemon refuses a file older than three intervals
// (internal/daemon/fileHostProbes), so this is also a third of its staleness
// budget and the two numbers have to stay in step.
const hostAgentInterval = 60 * time.Second

// hostAgentDeps is what the two subcommands take from the host: the town the
// command acts on, the loop, the install and the stream the install reports
// on. A test supplies fakes, so no unit test runs a loop against the host or
// writes to the operator's LaunchAgents.
type hostAgentDeps struct {
	findTownRoot func() (string, error)
	run          func(ctx context.Context, townRoot string) error
	install      func(townRoot string) (string, error)
	out          io.Writer
}

// realHostAgentDeps is hostAgentDeps on this host.
func realHostAgentDeps() hostAgentDeps {
	return hostAgentDeps{
		findTownRoot: workspace.FindFromCwdOrError,
		run:          runHostAgent,
		install: func(townRoot string) (string, error) {
			return installHostAgent(townRoot, realHostAgentInstallDeps())
		},
		out: os.Stdout,
	}
}

var hostCmd = &cobra.Command{
	Use:     "host",
	GroupID: GroupServices,
	Short:   "Write host health for the container daemon",
	Long: `Write the host's health to a file a container daemon reads.

A container cannot run memory_pressure, ps or top against the Mac, so a launchd
job here reads them and writes .runtime/host/probes.json every 60 seconds. In
container mode the daemon reads that file instead of probing the host itself.`,
	RunE: requireSubcommand,
}

var hostAgentCmd = &cobra.Command{
	Use:   "agent",
	Short: "The launchd job that writes the probe file",
	Long: `The host agent: one launchd job that reads the host and writes
.runtime/host/probes.json for a container daemon to read.`,
	RunE: requireSubcommand,
}

// newHostAgentRunCmd builds `gt host agent run`.
func newHostAgentRunCmd(d hostAgentDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Collect the host probes every 60 seconds",
		Long: `Collect the host probes and rewrite .runtime/host/probes.json every 60
seconds.

This is what the launchd job runs. It stays in the foreground and exits
cleanly (status 0) on SIGTERM or SIGINT: a clean stop is how the job is
stopped, and a non-zero exit would be read as a crash.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHostAgentCommand(cmd.Context(), d)
		},
	}
}

// newHostAgentInstallCmd builds `gt host agent install`.
func newHostAgentInstallCmd(d hostAgentDeps) *cobra.Command {
	return &cobra.Command{
		Use:   "install",
		Short: "Render the host agent's launchd plist",
		Long: `Render the host agent's launchd plist (label com.gastown.host-agent) and
write it to ~/Library/LaunchAgents/com.gastown.host-agent.plist.

It writes the file and stops there; loading the job is the operator's step,
since a job loaded here would need a live run to prove.

It refuses a plist that already runs the agent for a different town: the
label names one job on the host, and rewriting it would repoint the town it
serves.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runHostAgentInstallCommand(cmd.Context(), d)
		},
	}
}

func init() {
	hostAgentCmd.AddCommand(newHostAgentRunCmd(realHostAgentDeps()))
	hostAgentCmd.AddCommand(newHostAgentInstallCmd(realHostAgentDeps()))
	hostCmd.AddCommand(hostAgentCmd)
	rootCmd.AddCommand(hostCmd)
}

// hostAgentTownRoot resolves the town the command acts on.
func hostAgentTownRoot(find func() (string, error)) (string, error) {
	townRoot, err := find()
	if err != nil {
		return "", fmt.Errorf("not in a Gas Town workspace: %w", err)
	}
	return townRoot, nil
}

func runHostAgentCommand(parent context.Context, d hostAgentDeps) error {
	townRoot, err := hostAgentTownRoot(d.findTownRoot)
	if err != nil {
		return err
	}
	// SIGTERM is launchd's stop and SIGINT the operator's Ctrl-C; both end the
	// loop cleanly, since launchd respawns a crashed job but not a stopped one.
	ctx, stop := signal.NotifyContext(parent, os.Interrupt, syscall.SIGTERM)
	defer stop()
	return d.run(ctx, townRoot)
}

func runHostAgentInstallCommand(_ context.Context, d hostAgentDeps) error {
	townRoot, err := hostAgentTownRoot(d.findTownRoot)
	if err != nil {
		return err
	}
	path, err := d.install(townRoot)
	if err != nil {
		return err
	}
	fmt.Fprintf(d.out, "wrote %s\n\nThe job is written, not loaded. Load it with:\n  launchctl bootstrap gui/%d %s\n",
		path, os.Getuid(), path)
	return nil
}

// hostAgentLoop is the agent: collect the probes, write the file, wait one
// interval, repeat until the context ends. Every dependency is a seam, so a
// test drives it with a fake clock and records the writes instead of running
// a host command or waiting a minute.
type hostAgentLoop struct {
	clock    clockwork.Clock
	interval time.Duration
	// path is the probe file, hostprobe.Path(townRoot) in production.
	path    string
	collect func(ctx context.Context, now time.Time) hostprobe.Probes
	write   func(path string, p hostprobe.Probes) error
	logf    func(format string, args ...any)
}

// run is the agent's tick.
//
// The first write happens before the first wait, so a fresh job publishes the
// host at once instead of leaving the daemon to read a missing file for a
// minute. A failed write does not end the loop: the next tick rewrites the
// file, and the error goes to the job's log (the plist's StandardErrorPath),
// so a town the agent cannot write is visible there rather than as a crash
// loop launchd restarts on every throttle.
//
// A canceled context is a clean stop and returns nil: SIGTERM is how launchd
// stops the job. A tick whose collection was still running when the context
// ended is dropped rather than written, and so is a tick the clock woke after
// the stop, so nothing is written once the job has been told to stop.
func (l *hostAgentLoop) run(ctx context.Context) error {
	for {
		p := l.collect(ctx, l.clock.Now())
		if ctx.Err() != nil {
			return nil
		}
		if err := l.write(l.path, p); err != nil {
			l.logf("host agent: writing %s: %v", l.path, err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-l.clock.After(l.interval):
		}
	}
}

// runHostAgent is `gt host agent run` over the real host: the real clock, the
// real collectors, and hostprobe.Write under the town's probe path.
func runHostAgent(ctx context.Context, townRoot string) error {
	loop := &hostAgentLoop{
		clock:    clockwork.NewRealClock(),
		interval: hostAgentInterval,
		path:     hostprobe.Path(townRoot),
		collect: func(ctx context.Context, now time.Time) hostprobe.Probes {
			return hostprobe.Collect(ctx, hostAgentRunner, townRoot, now)
		},
		write: hostAgentWrite,
		logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
	return loop.run(ctx)
}

// hostAgentWrite publishes the probes, creating the directory they live in: a
// town that has never run the agent has no .runtime/host, and launchd has
// already opened the log there by the time this runs.
func hostAgentWrite(path string, p hostprobe.Probes) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return hostprobe.Write(path, p)
}

// hostAgentRunner is hostprobe.Runner over the real host: it runs the argv and
// returns its stdout. The context is the per-field deadline hostprobe.Collect
// sets, so a hung command is killed and fails only its own field. The error
// carries the command's failure, never its output: stdout is the reading, and
// a caller that echoed it into a field's Err would put the host's process
// table in the probe file (see hostprobe.Runner).
func hostAgentRunner(ctx context.Context, argv []string) ([]byte, error) {
	return runHostAgentArgv(ctx, argv, exec.CommandContext)
}

// hostAgentCommand builds the command to run: exec.CommandContext in
// production, and a test supplies one that fails at Start, so the error branch
// below is driven without starting anything.
type hostAgentCommand func(ctx context.Context, name string, args ...string) *exec.Cmd

// runHostAgentArgv runs argv through command and returns its stdout. Only
// stdout is the reading: the error is the command's own, never its output.
func runHostAgentArgv(ctx context.Context, argv []string, command hostAgentCommand) ([]byte, error) {
	out, err := command(ctx, argv[0], argv[1:]...).Output() //nolint:gosec // fixed argv built by hostprobe
	if err != nil {
		return nil, err
	}
	return out, nil
}

// hostAgentInstallDeps is what install takes from the host: the OS it runs on,
// the gt binary the job will run, the temp directory a go run binary sits in,
// where the plist goes, the renderer and the write. Production wires the real
// ones; a test points plistPath at a temporary directory and supplies the
// write, so no test can write to the operator's LaunchAgents.
type hostAgentInstallDeps struct {
	goos      string
	gtPath    func() (string, error)
	tempDir   func() string
	plistPath func() (string, error)
	render    func(gtPath, townRoot string) (string, error)
	writeFile func(path string, data []byte, perm os.FileMode) error
}

// realHostAgentInstallDeps is the host's own: this OS, this binary, the
// LaunchAgents path under the operator's home, and the embedded template.
func realHostAgentInstallDeps() hostAgentInstallDeps {
	return hostAgentInstallDeps{
		goos:      runtime.GOOS,
		gtPath:    os.Executable,
		tempDir:   os.TempDir,
		plistPath: templates.HostAgentPlistPath,
		render:    templates.HostAgentPlist,
		writeFile: os.WriteFile,
	}
}

// installHostAgent renders the host agent's plist for townRoot, writes it to
// the LaunchAgents path and returns that path. It does not load the job.
//
// It refuses four things. A host that is not macOS: the job is a launchd job,
// and a plist under ~/Library/LaunchAgents on another OS is a file nothing
// reads. An empty town root, which would render a job that writes nowhere. A
// gt binary in the temp directory, which is what `go run` leaves behind and
// deletes, so the job would fail at every start once it is gone. And a plist
// already installed for another town: the label names one job on the host, so
// writing it for a second town would silently repoint the first town's agent,
// whose daemon would then read a probe file nobody refreshes. Removing or
// replacing that job is the operator's decision, not this command's.
func installHostAgent(townRoot string, d hostAgentInstallDeps) (string, error) {
	if townRoot == "" {
		return "", errors.New("host agent install: no town root")
	}
	if d.goos != "darwin" {
		return "", fmt.Errorf("host agent install: the host agent is a launchd job and this host is %s", d.goos)
	}
	path, err := d.plistPath()
	if err != nil {
		return "", fmt.Errorf("host agent install: %w", err)
	}
	if err := hostAgentPlistFreeFor(path, townRoot); err != nil {
		return "", err
	}
	gtPath, err := d.gtPath()
	if err != nil {
		return "", fmt.Errorf("host agent install: finding the gt binary: %w", err)
	}
	if err := hostAgentRefuseATemporaryBinary(gtPath, d.tempDir()); err != nil {
		return "", err
	}
	content, err := d.render(gtPath, townRoot)
	if err != nil {
		return "", fmt.Errorf("host agent install: %w", err)
	}

	// launchd opens the plist's StandardOutPath and StandardErrorPath before
	// the program runs, and a missing directory fails the job with EX_CONFIG
	// (78), which launchctl cannot clear. Those paths are the probe file's
	// directory, so creating both here also gives the agent's first write a
	// directory to land in.
	if err := hostAgentPrepareDirs(hostprobe.Path(townRoot), path); err != nil {
		return "", fmt.Errorf("host agent install: %w", err)
	}
	// 0644: launchd reads the file as the user, and a plist is not a secret.
	if err := d.writeFile(path, []byte(content), 0o644); err != nil {
		return "", fmt.Errorf("host agent install: writing %s: %w", path, err)
	}
	return path, nil
}

// hostAgentRefuseATemporaryBinary refuses a gt path under tempDir. `go run`
// builds the binary in the temp directory and removes it when the run ends, so
// a plist naming one would start a program that is already gone; the operator
// installs from a gt built or installed where it stays.
//
// Both paths reach install absolute (os.Executable and os.TempDir), so this
// compares their cleaned spellings as strings and has no lookup to fail. A
// temp directory with no name of its own ("", ".", "/") matches nothing, which
// leaves the check unable to refuse rather than unable to install.
func hostAgentRefuseATemporaryBinary(gtPath, tempDir string) error {
	tempDir = filepath.Clean(tempDir)
	if tempDir == "." || tempDir == string(filepath.Separator) {
		return nil
	}
	if strings.HasPrefix(filepath.Clean(gtPath), tempDir+string(filepath.Separator)) {
		return fmt.Errorf("host agent install: %s is a temporary binary (go run builds there and deletes it); install from a built gt", gtPath)
	}
	return nil
}

// hostAgentPrepareDirs creates the directories install writes into: the probe
// file's, where launchd opens the job's log, and the plist's.
func hostAgentPrepareDirs(probePath, plistPath string) error {
	for _, dir := range []string{filepath.Dir(probePath), filepath.Dir(plistPath)} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}

// hostAgentPlistFreeFor refuses a host agent plist at path that serves a
// different town. A missing file is free. A file that exists but cannot be
// read or names no town is refused too: not knowing which town a job serves
// is not the same as knowing it serves this one.
func hostAgentPlistFreeFor(path, townRoot string) error {
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("host agent install: checking %s: %w", path, err)
	}
	isFor, err := templates.SupervisorFileIsFor(path, townRoot)
	if err != nil {
		return fmt.Errorf("host agent install: %w", err)
	}
	if !isFor {
		return fmt.Errorf("host agent install: %s already runs the host agent for another town; boot the job out and remove it first: launchctl bootout gui/%d %s",
			path, os.Getuid(), path)
	}
	return nil
}
