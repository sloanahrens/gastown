package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// workDir is where a child's directory is mounted in its container, and the working directory bd
// runs in there. Only that directory is mounted, so bd's walk up from its working directory finds
// no beads workspace above it (gt-p85pw.117).
const workDir = "/work"

// doltEnvPrefix is the one family of the recording environment a child inside the container gets:
// the names bd reads to reach its Dolt server. Nothing else crosses the boundary, so no BEADS_DIR,
// identity or other host value can steer a run (gt-p85pw.117).
const doltEnvPrefix = "BEADS_DOLT_"

// doltHostVar is the name bd reads the Dolt server's host from.
const doltHostVar = doltEnvPrefix + "SERVER_HOST"

// dockerBin is the docker command every child runs through.
const dockerBin = "docker"

// dockerRunner runs one docker command and returns what it wrote to stdout and its exit code. A
// non-zero exit is a result the run keeps; an error means docker never ran to an exit. main wires
// it to the docker binary; a unit test injects a fake, since a unit test may not start a process
// (gt-p85pw.117).
type dockerRunner func(argv []string) (stdout []byte, exit int, err error)

// container is the throwaway container every bd child runs in: docker run --rm, with the child's
// directory mounted at workDir and nothing else, so the town's workspace cannot be reached from
// inside it however the environment is set (gt-p85pw.117).
type container struct {
	image    string       // the image a child runs in
	bin      string       // the bd binary's path inside that image
	doltHost string       // the name the container reaches the throwaway Dolt server on
	base     []string     // the recording environment a child's endpoint names are read from
	docker   dockerRunner // how one docker command runs
}

// execChild returns the child bd the binary runs: every call inside the container opts names, whose
// only environment is the endpoint names of the recording environment, and the bd on PATH as the
// build the container's bd must be.
func execChild(opts options) childBD {
	c := container{
		image:    opts.bdImage,
		bin:      opts.bd,
		doltHost: opts.doltHost,
		base:     os.Environ(),
		docker:   execDocker,
	}
	return childBD{
		Run:         c.run,
		Env:         c.env,
		Where:       c.where,
		HostVersion: hostVersion,
	}
}

// execDocker runs one docker command, bounded by runLimit so a hung docker fails the recording
// instead of stalling it.
func execDocker(argv []string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	c := exec.CommandContext(ctx, dockerBin, argv...) //nolint:gosec // G204: argv is the container's fixed shape, and its arguments are the operator's own command line
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = os.Stderr
	c.WaitDelay = time.Second
	err := c.Run()
	return exitResult(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded))
}

// run runs one bd call inside the container, in dir mounted at workDir.
func (c container) run(dir string, args []string) ([]byte, int, error) {
	return c.docker(c.args(dir, c.env(dir), args))
}

// where asks bd which workspace it resolves in dir, inside the container.
func (c container) where(dir string, env []string) (string, error) {
	stdout, exit, err := c.docker(c.args(dir, env, []string{"where", "--json"}))
	return whereResult(stdout, exit, err)
}

// args returns the docker arguments one bd call is: run --rm, dir mounted at workDir and nothing
// else, the working directory there, env as the container's only environment, then the image, the
// bd binary and the call. A call with no directory (the help walk names none) mounts nothing and
// keeps the image's own working directory.
func (c container) args(dir string, env, args []string) []string {
	argv := []string{"run", "--rm"}
	if dir != "" {
		argv = append(argv, "-v", dir+":"+workDir, "-w", workDir)
	}
	for _, entry := range containerEnv(endpointEnv(env), c.doltHost) {
		argv = append(argv, "-e", entry)
	}
	argv = append(argv, c.image, c.bin)
	return append(argv, args...)
}

// env returns the environment a child inside the container gets: the recording environment's
// endpoint names, before the host is mapped to the name the container reaches this machine on.
func (c container) env(string) []string {
	return endpointEnv(c.base)
}

// endpointEnv returns the BEADS_DOLT_* names of base and no others: the only environment that
// crosses into the container (gt-p85pw.117).
func endpointEnv(base []string) []string {
	env := make([]string, 0, len(base))
	for _, entry := range base {
		if key, _, ok := strings.Cut(entry, "="); ok && strings.HasPrefix(key, doltEnvPrefix) {
			env = append(env, entry)
		}
	}
	return env
}

// containerEnv returns the endpoint env a child inside the container gets: a loopback host replaced
// by doltHost, since 127.0.0.1 inside the container is the container itself, and doltHost named
// when the recording names no host at all (gt-p85pw.117).
func containerEnv(env []string, doltHost string) []string {
	mapped := make([]string, 0, len(env)+1)
	named := false
	for _, entry := range env {
		key, value, _ := strings.Cut(entry, "=")
		if key == doltHostVar {
			named = true
			if isLocalHost(value) {
				value = doltHost
			}
		}
		mapped = append(mapped, key+"="+value)
	}
	if !named {
		mapped = append(mapped, doltHostVar+"="+doltHost)
	}
	return mapped
}

// bdVersionLineRE matches the fields of a `bd version` line the container's bd must share with the
// host's: the release, the build id, the commit and the schema level. A build from a checkout
// stamps its branch before the commit ("master@688ff7d249e9") and a cross-compiled build of the
// same commit does not, so the branch is matched and not compared (gt-p85pw.117).
var bdVersionLineRE = regexp.MustCompile(`\bbd version (\S+) \(([^:\s)]+):\s*(?:([^@\s)]+)@)?([^)\s]+)\)\s+schema<=(\d+)`)

// bdBuild is the part of a `bd version` line two bds must share for one golden to describe both.
type bdBuild struct {
	release string
	build   string
	commit  string
	schema  int
}

// parseBDBuild reads the compared fields of a `bd version` line. A line without them is an error
// rather than a zero build, so a bd the recorder cannot read is refused instead of compared.
func parseBDBuild(line []byte) (bdBuild, error) {
	m := bdVersionLineRE.FindSubmatch(line)
	if m == nil {
		return bdBuild{}, errors.New(`bd version printed no line of the form "bd version 1.2.2 (688ff7d2: master@688ff7d249e9) schema<=66", so the recorder cannot tell which build it is`)
	}
	schema, err := strconv.Atoi(string(m[5]))
	if err != nil {
		return bdBuild{}, fmt.Errorf("bd version printed the schema level %q, which is not a number", m[5])
	}
	return bdBuild{release: string(m[1]), build: string(m[2]), commit: string(m[4]), schema: schema}, nil
}

// String names the build as bd prints it, for a refusal that says which two builds it compared.
func (b bdBuild) String() string {
	return fmt.Sprintf("%s (build %s, commit %s, schema<=%d)", b.release, b.build, b.commit, b.schema)
}

// hostVersion returns the version line of the bd on PATH: the build the container's bd must be for
// the goldens to describe the fork bd the shim targets (gt-p85pw.117).
func hostVersion() ([]byte, error) {
	return execHost("bd", []string{"version"})
}

// execHost runs one bd command on this machine and returns its stdout. A non-zero exit is an error,
// since the host bd is read only to be compared with the container's.
func execHost(bin string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is the bd the operator installs, args are fixed
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = os.Stderr
	c.WaitDelay = time.Second
	err := c.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, fmt.Errorf("%s %s ran past %s", bin, strings.Join(args, " "), runLimit)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", bin, strings.Join(args, " "), err)
	}
	return stdout.Bytes(), nil
}

// proveBDBuild refuses a container whose bd is not the build the host's bd is: a golden recorded
// from another build answers differently from the bd the shim is checked against (gt-p85pw.117).
// Both calls read only, so a refusal here writes nothing.
func proveBDBuild(child childBD) error {
	line, err := child.HostVersion()
	if err != nil {
		return fmt.Errorf("read the version of the host bd: %w", err)
	}
	want, err := parseBDBuild(line)
	if err != nil {
		return fmt.Errorf("the host bd: %w", err)
	}
	stdout, exit, err := child.Run("", []string{"version"})
	if err != nil {
		return fmt.Errorf("run bd version in the container: %w", err)
	}
	if exit != 0 {
		return fmt.Errorf("bd version in the container exited %d", exit)
	}
	got, err := parseBDBuild(stdout)
	if err != nil {
		return fmt.Errorf("the container's bd: %w", err)
	}
	if got != want {
		return fmt.Errorf("the container's bd is %s but the host's is %s; every golden must come from the build the shim targets, so name the image and binary built from it", got, want)
	}
	return nil
}
