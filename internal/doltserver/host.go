package doltserver

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
	configpkg "github.com/steveyegge/gastown/internal/config"
)

// host is the machine the adapter manages Dolt on (docs/testing.md, "Seams
// for external tools"): the environment it reads, the commands it runs (dolt,
// lsof, ps, bd), the processes it starts and signals, the TCP it probes, the
// SQL connections it opens and the clock it waits on. Every function that
// touches one of those is a method of host; the exported function of the
// same name runs it on std, the real machine. A test builds a host whose
// fields answer in process, so the adapter's own logic (argument building,
// output parsing, process identity checks, error mapping, retry timing)
// runs with no process, socket or sleep. A nil field is the real machine.
type host struct {
	// lookupEnv reads an environment variable (os.LookupEnv), and
	// environList is the whole environment a child inherits (os.Environ).
	lookupEnv   func(key string) (string, bool)
	environList func() []string
	// run runs a command to completion and returns what it wrote. A failure
	// that should read as an exit status implements interface{ ExitCode() int }.
	run func(c hostCall) (stdout, stderr []byte, err error)
	// start starts a long-running process (the sql-server) and returns its PID.
	start func(cmd *exec.Cmd) (int, error)
	// alive reports whether pid is a running process.
	alive func(pid int) bool
	// signal sends sig to pid.
	signal func(pid int, sig syscall.Signal) error
	// dial reports whether a TCP connection to address succeeds within timeout.
	dial func(address string, timeout time.Duration) error
	// listen reports whether address can be bound for TCP right now.
	listen func(address string) error
	// lookupHost resolves a host name (net.LookupHost).
	lookupHost func(host string) ([]string, error)
	// readlink reads a symbolic link (/proc/<pid>/cwd on Linux).
	readlink func(path string) (string, error)
	// openDB opens a MySQL connection pool to dsn.
	openDB func(dsn string) (*sql.DB, error)
	// now and sleep are the clock the adapter's waits and timestamps use.
	now   func() time.Time
	sleep func(d time.Duration)
	// bdArgs prepends --allow-stale to a bd argv when that bd accepts it.
	bdArgs func(env, args []string) []string
	// readIssuePrefix and writeIssuePrefix read issue_prefix through bd and
	// write it through the in-process store (seedRigIssuePrefix).
	readIssuePrefix  func(townRoot, beadsDir string) (string, error)
	writeIssuePrefix func(townRoot, beadsDir, database, prefix string) error
	// doltEndpoint is the town's Dolt endpoint (config.ResolveDoltEndpoint).
	doltEndpoint func(townRoot string) (configpkg.DoltEndpoint, bool)
}

// std is the real machine.
var std = &host{}

// hostCall is one command a host runs: its argv (name first), directory,
// environment (nil inherits the process's) and stdin.
type hostCall struct {
	Args  []string
	Dir   string
	Env   []string
	Stdin []byte
}

func (h *host) resolveDoltEndpoint(townRoot string) (configpkg.DoltEndpoint, bool) {
	if h.doltEndpoint != nil {
		return h.doltEndpoint(townRoot)
	}
	return configpkg.ResolveDoltEndpoint(townRoot)
}

func (h *host) lookupEnvVar(key string) (string, bool) {
	if h.lookupEnv != nil {
		return h.lookupEnv(key)
	}
	return os.LookupEnv(key)
}

// environ is the environment a child process inherits.
func (h *host) environ() []string {
	if h.environList != nil {
		return h.environList()
	}
	return os.Environ()
}

// withEnv is a copy of h that reads env over its own environment: lookups
// find env first, and a child inherits h's environment with env appended,
// so env's values win.
func (h *host) withEnv(env map[string]string) *host {
	if len(env) == 0 {
		return h
	}
	c := *h
	c.lookupEnv = func(key string) (string, bool) {
		if v, ok := env[key]; ok {
			return v, true
		}
		return h.lookupEnvVar(key)
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c.environList = func() []string {
		out := h.environ()
		for _, k := range keys {
			out = append(out, k+"="+env[k])
		}
		return out
	}
	return &c
}

// exec runs cmd: the process on the real machine, h.run's answer otherwise.
// Its stdout is returned; its stderr goes to cmd.Stderr when that is set.
// Like exec.Cmd.Output, a failure with cmd.Stderr unset carries stderr.
func (h *host) exec(cmd *exec.Cmd) ([]byte, error) {
	if h.run == nil {
		if cmd.Stdout != nil {
			return nil, cmd.Run()
		}
		return cmd.Output()
	}
	var stdin []byte
	if r, ok := cmd.Stdin.(*bytes.Reader); ok {
		stdin = make([]byte, r.Len())
		_, _ = r.Read(stdin)
	} else if r, ok := cmd.Stdin.(*bytes.Buffer); ok {
		stdin = r.Bytes()
	}
	stdout, stderr, err := h.run(hostCall{Args: cmd.Args, Dir: cmd.Dir, Env: cmd.Env, Stdin: stdin})
	if cmd.Stderr != nil {
		_, _ = cmd.Stderr.Write(stderr)
	} else if err != nil {
		err = &callError{err: err, stderr: stderr}
	}
	if cmd.Stdout != nil {
		_, _ = cmd.Stdout.Write(stdout)
	}
	return stdout, err
}

// execCombined is exec.Cmd.CombinedOutput through h.
func (h *host) execCombined(cmd *exec.Cmd) ([]byte, error) {
	if h.run == nil {
		return cmd.CombinedOutput()
	}
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	out, err := h.exec(cmd)
	return append(out, errBuf.Bytes()...), err
}

// callError is a failed run whose stderr was not captured by the caller, as
// *exec.ExitError carries it.
type callError struct {
	err    error
	stderr []byte
}

func (e *callError) Error() string { return e.err.Error() }
func (e *callError) Unwrap() error { return e.err }

// exitCode returns err's process exit status, or -1 when it has none.
func exitCode(err error) int {
	var coded interface{ ExitCode() int }
	if errors.As(err, &coded) {
		return coded.ExitCode()
	}
	return -1
}

func (h *host) startProcess(cmd *exec.Cmd) (int, error) {
	if h.start != nil {
		return h.start(cmd)
	}
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	return cmd.Process.Pid, nil
}

func (h *host) processAlive(pid int) bool {
	if h.alive != nil {
		return h.alive(pid)
	}
	return processIsAlive(pid)
}

func (h *host) signalProcess(pid int, sig syscall.Signal) error {
	if h.signal != nil {
		return h.signal(pid, sig)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Signal(sig)
}

func (h *host) dialTCP(address string, timeout time.Duration) error {
	if h.dial != nil {
		return h.dial(address, timeout)
	}
	conn, err := net.DialTimeout("tcp", address, timeout)
	if err != nil {
		return err
	}
	return conn.Close()
}

// portFree reports whether the loopback port can be bound: nil when it is
// free.
func (h *host) portFree(port int) error {
	address := fmt.Sprintf("127.0.0.1:%d", port)
	if h.listen != nil {
		return h.listen(address)
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	return ln.Close()
}

func (h *host) resolveHost(name string) ([]string, error) {
	if h.lookupHost != nil {
		return h.lookupHost(name)
	}
	return net.LookupHost(name)
}

func (h *host) readLink(path string) (string, error) {
	if h.readlink != nil {
		return h.readlink(path)
	}
	return os.Readlink(path)
}

func (h *host) openMySQL(dsn string) (*sql.DB, error) {
	if h.openDB != nil {
		return h.openDB(dsn)
	}
	return sql.Open("mysql", dsn)
}

func (h *host) clockNow() time.Time {
	if h.now != nil {
		return h.now()
	}
	return time.Now()
}

func (h *host) wait(d time.Duration) {
	if h.sleep != nil {
		h.sleep(d)
		return
	}
	time.Sleep(d)
}

// runBD runs a bd command: beads.Cmd.Run on the real machine (which unwraps
// bd's machine envelope into cmd.Stdout), h.run's answer otherwise.
func (h *host) runBD(cmd *beads.Cmd) error {
	if h.run == nil {
		return cmd.Run()
	}
	_, err := h.exec(cmd.Cmd)
	return err
}

func (h *host) allowStaleArgs(env, args []string) []string {
	if h.bdArgs != nil {
		return h.bdArgs(env, args)
	}
	return beads.MaybePrependAllowStaleWithEnv(env, args)
}
