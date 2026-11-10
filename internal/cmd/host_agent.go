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
	"sync"
	"syscall"
	"time"

	"github.com/jonboulle/clockwork"
	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/atomicfile"
	"github.com/steveyegge/gastown/internal/diskmaint/disks"
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
	install      func(townRoot string) ([]string, error)
	out          io.Writer
}

// realHostAgentDeps is hostAgentDeps on this host.
func realHostAgentDeps() hostAgentDeps {
	return hostAgentDeps{
		findTownRoot: workspace.FindFromCwdOrError,
		run:          runHostAgent,
		install: func(townRoot string) ([]string, error) {
			return installHostAgent(townRoot, realHostAgentInstallDeps())
		},
		out: os.Stdout,
	}
}

var hostCmd = &cobra.Command{
	Use:     "host",
	GroupID: GroupServices,
	Short:   "Read the host and report it",
	Long: `Read the host and report what it is doing.

'gt host agent' writes the host's health to a file a container daemon reads: a
container cannot run memory_pressure, ps or top against the Mac, so a launchd
job reads them and writes .runtime/host/probes.json every 60 seconds. In
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
		Short: "Render the host agent's launchd plists",
		Long: `Render the host agent's launchd plist (label com.gastown.host-agent) and
one daily job for each Mac-only maintenance target, and write them to
~/Library/LaunchAgents.

It writes the files and stops there; loading a job is the operator's step,
since a job loaded here would need a live run to prove. A file whose content
is unchanged is left untouched, so a second install rewrites nothing.

It refuses a gt binary that go run leaves in the temp directory, and a plist
that already serves a different town: a label names one job on the host, and
rewriting it would repoint the town it serves.`,
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
	paths, err := d.install(townRoot)
	if err != nil {
		return err
	}
	for _, path := range paths {
		fmt.Fprintf(d.out, "wrote %s\n", path)
	}
	fmt.Fprintf(d.out, "\nThe jobs are written, not loaded. Load them with:\n")
	for _, path := range paths {
		fmt.Fprintf(d.out, "  launchctl bootstrap gui/%d %s\n", os.Getuid(), path)
	}
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
	// disk is the town volume's disk read each tick makes. Production leaves it
	// nil, which hostprobe.CollectWith reads through util.GetDiskSpace; a test
	// sets a fake, so no test starts diskutil (gt-yjzepk.15).
	disk hostprobe.DiskReader
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
	dockerDisk := newHostAgentDockerDisk()
	// disk stays nil: every tick reads the town volume through util.GetDiskSpace.
	loop := &hostAgentLoop{
		clock:    clockwork.NewRealClock(),
		interval: hostAgentInterval,
		path:     hostprobe.Path(townRoot),
		write:    hostAgentWrite,
		logf: func(format string, args ...any) {
			fmt.Fprintf(os.Stderr, format+"\n", args...)
		},
	}
	loop.collect = func(ctx context.Context, now time.Time) hostprobe.Probes {
		return hostAgentCollect(ctx, hostAgentRunner, townRoot, now, dockerDisk, templates.SupervisorJobState, loop.disk)
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

// hostAgentLaunchdTimeout bounds the launchd read, as hostprobe bounds each of
// its own field reads: a launchctl that hangs must not hold the whole reading.
const hostAgentLaunchdTimeout = 30 * time.Second

// hostAgentWithLaunchd fills the reading's launchd field from the supervisor
// reader, within hostAgentLaunchdTimeout. A container daemon cannot run
// launchctl, so this reading is how its startup guard learns whether the old
// launchd owner still runs. A job that cannot be read, or a read that outlives
// ctx, sets the field's Err and never reads as not running.
func hostAgentWithLaunchd(ctx context.Context, p hostprobe.Probes, read templates.SupervisorReader) hostprobe.Probes {
	ctx, cancel := context.WithTimeout(ctx, hostAgentLaunchdTimeout)
	defer cancel()
	done := make(chan templates.SupervisorState, 1)
	go func() { done <- read("launchd") }()
	select {
	case st := <-done:
		if st.Err != nil {
			p.Launchd = &hostprobe.Field[hostprobe.Launchd]{Err: fmt.Sprintf("launchd job read: %v", st.Err)}
			return p
		}
		p.Launchd = &hostprobe.Field[hostprobe.Launchd]{Value: hostprobe.Launchd{Loaded: st.Loaded, PID: st.PID}}
	case <-ctx.Done():
		p.Launchd = &hostprobe.Field[hostprobe.Launchd]{Err: "launchd job read: " + ctx.Err().Error()}
	}
	return p
}

// hostAgentDockerDiskTimeout bounds the docker exec, as hostprobe bounds each of
// its own field reads: a docker that hangs must not hold the whole reading.
const hostAgentDockerDiskTimeout = 30 * time.Second

// hostAgentWithDockerDisk fills the reading's Docker Desktop disk from the df of
// hostprobe.DockerDiskContainer, through the agent's runner. A container daemon
// cannot run docker against the host, so this field is how it learns the use of
// the disk a container it depends on lives on. A failed read or output that
// names no capacity sets the field's Err and leaves Value zero, so the daemon
// omits the disk rather than reading it as empty.
func hostAgentWithDockerDisk(ctx context.Context, p hostprobe.Probes, run hostprobe.Runner) hostprobe.Probes {
	ctx, cancel := context.WithTimeout(ctx, hostAgentDockerDiskTimeout)
	defer cancel()
	out, err := run(ctx, []string{"docker", "exec", hostprobe.DockerDiskContainer, "df", "-P", "/"})
	if err != nil {
		p.DockerDisk = hostprobe.Field[hostprobe.Disk]{Err: fmt.Sprintf("docker exec %s df: %v", hostprobe.DockerDiskContainer, err)}
		return p
	}
	pct, err := disks.ParseDF(string(out))
	if err != nil {
		p.DockerDisk = hostprobe.Field[hostprobe.Disk]{Err: fmt.Sprintf("docker exec %s df: %v", hostprobe.DockerDiskContainer, err)}
		return p
	}
	p.DockerDisk = hostprobe.Field[hostprobe.Disk]{Value: hostprobe.Disk{UsedPct: pct}}
	return p
}

// hostAgentDockerDisk is the Docker Desktop disk's latest finished reading. The
// tick does not wait for docker exec: a read runs beside the ticks and each tick
// publishes the last one that finished. So a docker that hangs cannot extend the
// write cycle, and its read ends at hostAgentDockerDiskTimeout with the error set.
// The published reading is one tick behind the disk; before the first read
// finishes it is hostprobe.DockerDiskNotCollected, never 0% used. A read still
// running past its deadline publishes an error, never the last finished value.
type hostAgentDockerDisk struct {
	mu       sync.Mutex
	inFlight bool
	started  time.Time // the tick that started the read in flight
	latest   hostprobe.Field[hostprobe.Disk]

	// onStored, when set, is called after a read's result is stored and the
	// mutex released, so a test waits on the store itself rather than sampling
	// the fields. It is nil in the agent (gt-qjzsjs).
	onStored func()
}

func newHostAgentDockerDisk() *hostAgentDockerDisk {
	return &hostAgentDockerDisk{latest: hostprobe.Field[hostprobe.Disk]{Err: hostprobe.DockerDiskNotCollected}}
}

// reading returns the latest finished reading and starts the next read unless
// one is still running, so a hung docker holds one goroutine, not one per tick.
// now is the tick's clock: a read in flight at its deadline is no reading at all.
func (d *hostAgentDockerDisk) reading(ctx context.Context, run hostprobe.Runner, now time.Time) hostprobe.Field[hostprobe.Disk] {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.inFlight {
		if now.Sub(d.started) >= hostAgentDockerDiskTimeout {
			return hostprobe.Field[hostprobe.Disk]{Err: fmt.Sprintf("docker exec %s df: still running %s after it started", hostprobe.DockerDiskContainer, hostAgentDockerDiskTimeout)}
		}
		return d.latest
	}
	d.inFlight = true
	d.started = now
	go d.refresh(ctx, run)
	return d.latest
}

// refresh runs one read and publishes it, clearing inFlight for the next tick.
func (d *hostAgentDockerDisk) refresh(ctx context.Context, run hostprobe.Runner) {
	got := hostAgentWithDockerDisk(ctx, hostprobe.Probes{}, run).DockerDisk
	d.mu.Lock()
	d.latest = got
	d.inFlight = false
	stored := d.onStored
	d.mu.Unlock()
	if stored != nil {
		stored()
	}
}

// hostAgentCollect is one tick's reading: the probes hostprobe.CollectWith reads
// through disk (nil reads through util.GetDiskSpace), the Docker Desktop disk
// from dockerDisk, and the launchd job from read.
func hostAgentCollect(ctx context.Context, run hostprobe.Runner, townRoot string, now time.Time, dockerDisk *hostAgentDockerDisk, read templates.SupervisorReader, disk hostprobe.DiskReader) hostprobe.Probes {
	p := hostprobe.CollectWith(ctx, run, disk, townRoot, now)
	p.DockerDisk = dockerDisk.reading(ctx, run, now)
	return hostAgentWithLaunchd(ctx, p, read)
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
	goos        string
	gtPath      func() (string, error)
	tempDir     func() string
	plistPath   func() (string, error)
	render      func(gtPath, townRoot string) (string, error)
	renderMaint func(gtPath, townRoot string, job templates.MacMaintJob) (string, error)
	readFile    func(path string) ([]byte, error)
	writeFile   func(path string, data []byte, perm os.FileMode) error
}

// realHostAgentInstallDeps is the host's own: this OS, this binary, the
// LaunchAgents path under the operator's home, and the embedded templates.
func realHostAgentInstallDeps() hostAgentInstallDeps {
	return hostAgentInstallDeps{
		goos:        runtime.GOOS,
		gtPath:      os.Executable,
		tempDir:     os.TempDir,
		plistPath:   templates.HostAgentPlistPath,
		render:      templates.HostAgentPlist,
		renderMaint: templates.MacMaintJobPlist,
		readFile:    os.ReadFile,
		// The plist is published by a rename, never written in place: launchd
		// reads a job the moment its file appears, and a half-written one is a
		// plist it cannot parse.
		writeFile: atomicfile.WriteFile,
	}
}

// installHostAgent renders the host agent's plist and one daily plist for each
// Mac-only maint target, writes them to the LaunchAgents directory and returns
// their paths, the agent's first. It does not load the jobs.
//
// It refuses four things. A host that is not macOS: the jobs are launchd jobs,
// and a plist under ~/Library/LaunchAgents on another OS is a file nothing
// reads. An empty town root, which would render jobs that write nowhere. A gt
// binary in the temp directory, which is what `go run` leaves behind and
// deletes, so every job would fail at its start once it is gone. And a plist
// already installed for another town: a label names one job on the host, so
// writing it for a second town would silently repoint the first town's job.
// Removing or replacing that job is the operator's decision, not this
// command's.
func installHostAgent(townRoot string, d hostAgentInstallDeps) ([]string, error) {
	if townRoot == "" {
		return nil, errors.New("host agent install: no town root")
	}
	if d.goos != "darwin" {
		return nil, fmt.Errorf("host agent install: the host agent is a launchd job and this host is %s", d.goos)
	}
	agentPath, err := d.plistPath()
	if err != nil {
		return nil, fmt.Errorf("host agent install: %w", err)
	}
	if err := hostAgentPlistFreeFor(agentPath, townRoot); err != nil {
		return nil, err
	}
	gtPath, err := d.gtPath()
	if err != nil {
		return nil, fmt.Errorf("host agent install: finding the gt binary: %w", err)
	}
	if err := hostAgentRefuseATemporaryBinary(gtPath, d.tempDir()); err != nil {
		return nil, err
	}
	agentContent, err := d.render(gtPath, townRoot)
	if err != nil {
		return nil, fmt.Errorf("host agent install: %w", err)
	}

	// Every job this install writes: the agent's plist first, then one file
	// per Mac-only maint target beside it. Each one is rendered and checked
	// before any is written, so a plist that serves another town refuses the
	// whole install rather than leaving half of it repointed.
	type jobFile struct {
		path    string
		content string
	}
	// The maint plists live in the agent plist's own directory, so install
	// needs no second host path to find them.
	launchAgentsDir := filepath.Dir(agentPath)
	jobs := []jobFile{{path: agentPath, content: agentContent}}
	for _, job := range templates.MacMaintJobs() {
		path := filepath.Join(launchAgentsDir, job.PlistName())
		if err := hostAgentPlistFreeFor(path, townRoot); err != nil {
			return nil, err
		}
		content, err := d.renderMaint(gtPath, townRoot, job)
		if err != nil {
			return nil, fmt.Errorf("host agent install: %w", err)
		}
		jobs = append(jobs, jobFile{path: path, content: content})
	}

	// launchd opens each plist's StandardOutPath and StandardErrorPath before
	// the program runs, and a missing directory fails the job with EX_CONFIG
	// (78), which launchctl cannot clear. The agent and the maint jobs log
	// under the probe file's directory, so creating it also gives the agent's
	// first write a directory to land in.
	dirs := []string{filepath.Dir(hostprobe.Path(townRoot))}
	for _, job := range jobs {
		dirs = append(dirs, filepath.Dir(job.path))
	}
	if err := hostAgentPrepareDirs(dirs...); err != nil {
		return nil, fmt.Errorf("host agent install: %w", err)
	}

	paths := make([]string, len(jobs))
	for i, job := range jobs {
		// 0644: launchd reads the file as the user, and a plist is not a secret.
		if err := hostAgentWriteIfChanged(d.readFile, d.writeFile, job.path, job.content); err != nil {
			return nil, fmt.Errorf("host agent install: %w", err)
		}
		paths[i] = job.path
	}
	return paths, nil
}

// hostAgentWriteIfChanged writes content to path unless the file already holds
// exactly it. launchd reads a job the moment its file changes, so a reinstall
// that would write the same plist leaves the file alone instead of replacing
// it under a running job.
//
// An existing file that cannot be read is reported rather than overwritten:
// install cannot tell what it holds, and the free-for check that runs before
// this one has already read it, so a failure here is a real one.
func hostAgentWriteIfChanged(read func(string) ([]byte, error), write func(string, []byte, os.FileMode) error, path, content string) error {
	existing, err := read(path)
	switch {
	case err == nil && string(existing) == content:
		return nil
	case err != nil && !os.IsNotExist(err):
		return fmt.Errorf("reading %s: %w", path, err)
	}
	if err := write(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// hostAgentRefuseATemporaryBinary refuses a gt path under tempDir. `go run`
// builds the binary in the temp directory and removes it when the run ends, so
// a plist naming one would start a program that is already gone; the operator
// installs from a gt built or installed where it stays.
//
// Both paths reach install absolute (os.Executable and os.TempDir), but not
// always spelled alike: macOS's temp directory is under /var, a symlink to
// /private/var, and either path may arrive by either name. Each is compared with
// its symlinks resolved when they resolve, and by its cleaned spelling when a
// lookup fails, so a failed lookup leaves the check unable to refuse rather
// than unable to install. A temp directory with no name of its own ("", ".",
// "/") matches nothing.
func hostAgentRefuseATemporaryBinary(gtPath, tempDir string) error {
	tempDir = hostAgentCanonPath(tempDir)
	if tempDir == "." || tempDir == string(filepath.Separator) {
		return nil
	}
	if strings.HasPrefix(hostAgentCanonPath(gtPath), tempDir+string(filepath.Separator)) {
		return fmt.Errorf("host agent install: %s is a temporary binary (go run builds there and deletes it); install from a built gt", gtPath)
	}
	return nil
}

// hostAgentCanonPath is p cleaned, with its symlinks resolved when they
// resolve.
func hostAgentCanonPath(p string) string {
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return filepath.Clean(r)
	}
	return filepath.Clean(p)
}

// hostAgentPrepareDirs creates every directory install writes into: the probe
// file's, where launchd opens the agent's log, and each plist's.
func hostAgentPrepareDirs(dirs ...string) error {
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}

// hostAgentPlistFreeFor refuses a plist at path that serves a different town.
// A missing file is free. A file that exists but cannot be read or names no
// town is refused too: not knowing which town a job serves is not the same as
// knowing it serves this one.
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
		return fmt.Errorf("host agent install: %s already serves another town; boot the job out and remove it first: launchctl bootout gui/%d %s",
			path, os.Getuid(), path)
	}
	return nil
}
