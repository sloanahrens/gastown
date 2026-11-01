package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// workDir is where a child's directory is mounted in its container, and the working directory bd
// runs in there. Only that directory is mounted, so bd's walk up from its working directory finds
// no beads workspace above it (gt-p85pw.117).
const workDir = "/work"

// doltEnvPrefix is the family of the recording environment's names bd reads to reach its Dolt
// server. Those names and the recorder's pinned identity are the only ones a child inside the
// container gets, so no BEADS_DIR or other host value can steer a run (gt-p85pw.117).
const doltEnvPrefix = "BEADS_DOLT_"

// doltHostVar is the name bd reads the Dolt server's host from.
const doltHostVar = doltEnvPrefix + "SERVER_HOST"

// storeSelectors are the names of a store that is not the workspace the recorder made. They are
// dropped from a child's environment rather than passed in: a copy's own .beads metadata names its
// database, so a selector from the operator's shell would aim a run at another one (gt-p85pw.92).
var storeSelectors = []string{"BEADS_DOLT_SERVER_DATABASE"}

// pinnedIdentity is the environment every child gets, so the actor and the git identity bd records
// are the recorder's own rather than the operator's: a golden's owner and created_by then read the
// same on every machine (gt-p85pw.103).
var pinnedIdentity = [][2]string{
	{"BEADS_ACTOR", "bdshim-record"},
	{"BD_ACTOR", "bdshim-record"},
	{"GIT_AUTHOR_NAME", "bdshim-record"},
	{"GIT_AUTHOR_EMAIL", "bdshim-record@example.invalid"},
	{"GIT_COMMITTER_NAME", "bdshim-record"},
	{"GIT_COMMITTER_EMAIL", "bdshim-record@example.invalid"},
}

// testServerOptIn is the one name besides the pinned identity the recorder adds to a child's
// environment: bd refuses to make a database on a server it cannot vouch for unless
// BEADS_TEST_SERVER=1 is set, and a recording's scratch database is made on a throwaway server by
// design (gt-czb3c). The value is the recorder's own constant, never the operator's, so an
// operator's BEADS_TEST_SERVER never decides whether a container call may create its database.
var testServerOptIn = [2]string{"BEADS_TEST_SERVER", "1"}

// dockerBin is the docker command every child runs through.
const dockerBin = "docker"

// dockerRunner runs one docker command, with env as the docker client's environment, and returns
// what the command wrote to stdout and its exit code. A non-zero exit is a result the run keeps; an
// error means docker never ran to an exit. main wires it to the docker binary; a unit test injects a
// fake, since a unit test may not start a process (gt-p85pw.117).
type dockerRunner func(env []string, argv []string) (stdout []byte, exit int, err error)

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

// execChild returns the childBD whose every call runs bd in a container from opts.bdImage, with only
// the directory the call works in mounted at workDir. The container's environment is the BEADS_DOLT_
// names of this process's environment, less the store selector, plus the recorder's pinned identity.
// A bd exit code is a result the caller keeps. A docker command that never ran to an exit, or ran
// past runLimit, is an error. Docker's own failure to start a container also exits 125, which reads as
// an exit code, so the guard's version call refuses it before any golden is written.
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

// errDockerTimedOut is what a docker command that ran past runLimit returns. Killing the docker client
// does not stop the container it started, so the caller removes that container by name (gt-p85pw.117).
var errDockerTimedOut = fmt.Errorf("docker ran past %s", runLimit)

// execDocker runs one docker command, bounded by runLimit so a hung docker fails the recording
// instead of stalling it. env is the docker client's environment, where docker reads the value of
// each -e name the command carries. Each call gets its own context, so a removal after a timeout is
// not cut short by the expired one.
func execDocker(env []string, argv []string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	c := exec.CommandContext(ctx, dockerBin, argv...) //nolint:gosec // G204: argv is the container's fixed shape, and its arguments are the operator's own command line
	c.Env = dockerClientEnv(os.Environ(), env)
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = os.Stderr
	c.WaitDelay = time.Second
	err := c.Run()
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return nil, 0, errDockerTimedOut
	}
	return exitResult(stdout.Bytes(), err, false)
}

// dockerClientEnv returns the environment the docker client runs with: the recorder's own, with
// every name env sets replaced by env's value. docker reads a `-e NAME` value from the client's
// environment, so an endpoint value or a secret stays out of the argv another process can read
// (gt-p85pw.117).
func dockerClientEnv(base, env []string) []string {
	kept := make([]string, 0, len(base)+len(env))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if _, set := envValue(env, key); set {
			continue
		}
		kept = append(kept, entry)
	}
	return append(kept, env...)
}

// run runs one bd call inside the container, in dir mounted at workDir.
func (c container) run(dir string, args []string) ([]byte, int, error) {
	env := containerEnv(c.env(dir), c.doltHost)
	return c.call(dir, env, args)
}

// where asks bd which workspace it resolves in dir, inside the container.
func (c container) where(dir string, env []string) (string, error) {
	env = containerEnv(env, c.doltHost)
	stdout, exit, err := c.call(dir, env, []string{"where", "--json"})
	return whereResult(stdout, exit, err)
}

// call runs one bd call in a new container named for this call. A call that runs past runLimit has
// its docker client killed, which leaves the container running against dir, so the container is
// removed by the name the call gave it. The removal's own failure is joined to the timeout, so a
// container that may still be writing is never reported as cleaned up (gt-p85pw.117).
func (c container) call(dir string, env, args []string) ([]byte, int, error) {
	name := containerName()
	stdout, exit, err := c.docker(env, c.args(dir, env, name, args))
	if !errors.Is(err, errDockerTimedOut) {
		return stdout, exit, err
	}
	_, rmExit, rmErr := c.docker(nil, []string{"rm", "-f", name})
	if rmErr == nil && rmExit == 0 {
		return nil, 0, err
	}
	if rmErr == nil {
		rmErr = fmt.Errorf("docker rm exited %d", rmExit)
	}
	return nil, 0, errors.Join(err, fmt.Errorf("remove container %s: %w", name, rmErr))
}

// containerName returns a random name for one call's container, so the removal after a timeout names
// that call's container and not another's (gt-p85pw.117).
func containerName() string {
	return "bdshim-record-" + strings.ToLower(rand.Text())
}

// args returns the docker arguments one bd call is: run --rm under name, dir mounted at workDir and
// nothing else, the working directory there, one -e per name in env, then the image, the bd binary and
// the call. Each -e names a variable and carries no value: docker reads the value from the client's
// environment, where the runner puts it, so a secret never appears in the argv (gt-p85pw.117). A
// call with no directory (the help walk names none) mounts nothing and keeps the image's own working
// directory.
func (c container) args(dir string, env []string, name string, args []string) []string {
	argv := []string{"run", "--rm", "--name", name}
	if dir != "" {
		argv = append(argv, "-v", dir+":"+workDir, "-w", workDir)
	}
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		argv = append(argv, "-e", key)
	}
	argv = append(argv, c.image, c.bin)
	return append(argv, args...)
}

// env returns the environment a child inside the container gets: the recording environment's
// endpoint names, before the host is mapped to the name the container reaches this machine on, and
// the recorder's pinned identity (gt-p85pw.103, gt-p85pw.117).
func (c container) env(string) []string {
	env := endpointEnv(c.base)
	for _, pinned := range pinnedIdentity {
		env = append(env, pinned[0]+"="+pinned[1])
	}
	return env
}

// endpointEnv returns the BEADS_DOLT_* names of base that name a server and no store selector: the
// only inherited environment that crosses into the container (gt-p85pw.92, gt-p85pw.117).
func endpointEnv(base []string) []string {
	env := make([]string, 0, len(base))
	for _, entry := range base {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, doltEnvPrefix) && !slices.Contains(storeSelectors, key) {
			env = append(env, entry)
		}
	}
	return env
}

// containerEnv returns the endpoint env a child inside the container gets: a loopback host replaced
// by doltHost, since 127.0.0.1 inside the container is the container itself, doltHost named when the
// recording names no host at all, and bd's opt-in to a database on a server it cannot vouch for,
// which is the throwaway server every recording runs against (gt-p85pw.117, gt-czb3c). The opt-in is
// added here, after refuseTownServer has read the environment (container.env), so the refusal sees
// exactly what it saw before (gt-czb3c).
func containerEnv(env []string, doltHost string) []string {
	mapped := make([]string, 0, len(env)+2)
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
	return append(mapped, testServerOptIn[0]+"="+testServerOptIn[1])
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
		return bdBuild{}, errors.New("bd version printed a schema level the recorder cannot read as a number")
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

// execHost runs one bd command on this machine and returns its stdout.
func execHost(bin string, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), runLimit)
	defer cancel()
	c := exec.CommandContext(ctx, bin, args...) //nolint:gosec // G204: bin is the bd the operator installs, args are fixed
	var stdout bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = os.Stderr
	c.WaitDelay = time.Second
	err := c.Run()
	return hostResult(stdout.Bytes(), err, errors.Is(ctx.Err(), context.DeadlineExceeded))
}

// hostResult turns how one host bd run ended into its stdout. A run that did not finish, or exited
// non-zero, is an error, since the host bd is read only to be compared with the container's and a
// version the recorder could not read is no proof at all.
func hostResult(stdout []byte, runErr error, timedOut bool) ([]byte, error) {
	if timedOut {
		return nil, fmt.Errorf("the host bd ran past %s", runLimit)
	}
	if runErr != nil {
		return nil, fmt.Errorf("the host bd: %w", runErr)
	}
	return stdout, nil
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
