package doltserver

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	configpkg "github.com/steveyegge/gastown/internal/config"
)

// fakeHost is a machine in memory for host's seams: an environment, a
// process table (argv, working directory, the TCP port a process listens
// on), TCP endpoints that answer, scripted command output, and a clock that
// moves only when the adapter sleeps. It records every command, signal and
// sleep. Nothing it does starts a process, opens a socket or waits.
type fakeHost struct {
	mu        sync.Mutex
	env       map[string]string
	procs     map[int]*fakeProc
	reachable map[string]bool // host:port that accept TCP besides listening processes
	// reachableFrom holds host:port that start accepting TCP once the clock
	// reaches the time given.
	reachableFrom map[string]time.Time
	answers       map[string][]fakeReply
	calls         [][]string
	signals       []fakeSignal
	slept         time.Duration
	now           time.Time
	nextPID       int
	started       []*exec.Cmd
	// onStart, when set, runs for each process start with the new PID.
	onStart func(pid int, cmd *exec.Cmd)
	// startExited, when set, makes every start return a child that has
	// already exited: the handle is already reaped while the process table
	// keeps it alive, the way an unreaped zombie still answers signal(0)
	// (gt-fpunm). startExitErr is that child's exit error.
	startExited  bool
	startExitErr error
	// endpointPort, when set, is every town's Dolt endpoint port (townPort).
	endpointPort int
}

// fakeProc is one process: its argv, working directory and the loopback
// port it listens on (0 for none). A process that ignores SIGTERM dies only
// on SIGKILL.
type fakeProc struct {
	args        []string
	cwd         string
	port        int
	alive       bool
	ignoresTERM bool
}

type fakeSignal struct {
	pid int
	sig syscall.Signal
}

// fakeReply is a canned command result: stdout, stderr and the exit status
// (0 is success).
type fakeReply struct {
	stdout, stderr string
	code           int
}

// fakeExit is a command's exit status, read through ExitCode().
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

// fakeEpoch is where every fake clock starts.
var fakeEpoch = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

func newFakeHost() *fakeHost {
	return &fakeHost{
		env:           map[string]string{},
		procs:         map[int]*fakeProc{},
		reachable:     map[string]bool{},
		reachableFrom: map[string]time.Time{},
		answers:       map[string][]fakeReply{},
		now:           fakeEpoch,
		nextPID:       1 << 22, // above any real PID, so never this test process
	}
}

// host is the host whose seams answer from f.
func (f *fakeHost) host() *host {
	return &host{
		lookupEnv:    f.lookupEnv,
		environList:  f.environ,
		run:          f.run,
		start:        f.start,
		alive:        f.alive,
		signal:       f.signal,
		dial:         f.dial,
		listen:       f.listen,
		lookupHost:   f.lookupHost,
		readlink:     f.readlink,
		openDB:       func(string) (*sql.DB, error) { return nil, errors.New("fake host: no SQL server") },
		now:          f.clockNow,
		sleep:        f.sleep,
		bdArgs:       func(_, args []string) []string { return args },
		doltEndpoint: f.doltEndpoint,
	}
}

// writeTownDoltSettings writes operational.dolt into townRoot's
// settings/config.json (gt-y3pgh.2.3). doltJSON is the dolt object's body.
func writeTownDoltSettings(t *testing.T, townRoot, doltJSON string) {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"type":"town-settings","version":1,"operational":{"dolt":` + doltJSON + `}}`
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// writeTownDaemonEnv writes townRoot's settings/daemon.env (gt-y3pgh.2.4):
// one KEY=VALUE line per entry passed in body.
func writeTownDaemonEnv(t *testing.T, townRoot, body string) {
	t.Helper()
	dir := filepath.Join(townRoot, "settings")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "daemon.env"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setenv sets an environment variable of the fake machine.
func (f *fakeHost) setenv(key, value string) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.env[key] = value
	return f
}

// spawn adds a live process and returns its PID.
func (f *fakeHost) spawn(p fakeProc) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextPID++
	p.alive = true
	f.procs[f.nextPID] = &p
	return f.nextPID
}

// listenOn makes pid listen on port.
func (f *fakeHost) listenOn(pid, port int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.procs[pid].port = port
}

// on scripts the replies to a command, keyed by its argv joined with spaces.
// A key ending in " *" matches any argv starting with the rest. Successive
// calls take successive replies; the last one repeats.
func (f *fakeHost) on(key string, replies ...fakeReply) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[key] = replies
	return f
}

func (f *fakeHost) environ() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	env := make([]string, 0, len(f.env))
	for k, v := range f.env {
		env = append(env, k+"="+v)
	}
	return env
}

func (f *fakeHost) lookupEnv(key string) (string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	v, ok := f.env[key]
	return v, ok
}

// listener returns the live process listening on port, or 0.
func (f *fakeHost) listener(port int) int {
	for pid, p := range f.procs {
		if p.alive && p.port == port && port != 0 {
			return pid
		}
	}
	return 0
}

// run answers the process queries the adapter makes (ps and lsof, from the
// process table) and otherwise the scripted replies. An unscripted command
// exits 127, as a missing binary would.
func (f *fakeHost) run(c hostCall) ([]byte, []byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string(nil), c.Args...))
	args := c.Args
	joined := strings.Join(args, " ")
	switch {
	case len(args) == 5 && args[0] == "ps" && args[1] == "-p" && args[3] == "-o" && args[4] == "args=":
		pid, _ := strconv.Atoi(args[2])
		if p := f.procs[pid]; p != nil && p.alive {
			return []byte(strings.Join(p.args, " ") + "\n"), nil, nil
		}
		return nil, nil, fakeExit(1)
	case joined == "ps -eo pid,args":
		var out strings.Builder
		out.WriteString("  PID ARGS\n")
		for pid, p := range f.procs {
			if p.alive {
				fmt.Fprintf(&out, "%5d %s\n", pid, strings.Join(p.args, " "))
			}
		}
		return []byte(out.String()), nil, nil
	case len(args) == 7 && args[0] == "lsof" && args[1] == "-a" && args[2] == "-p" && args[4] == "-d" && args[5] == "cwd":
		pid, _ := strconv.Atoi(args[3])
		if p := f.procs[pid]; p != nil && p.alive && p.cwd != "" {
			return []byte(fmt.Sprintf("p%d\nfcwd\nn%s\n", pid, p.cwd)), nil, nil
		}
		return nil, nil, fakeExit(1)
	case len(args) == 5 && args[0] == "lsof" && args[1] == "-i" && args[3] == "-sTCP:LISTEN" && args[4] == "-t":
		port, _ := strconv.Atoi(strings.TrimPrefix(args[2], ":"))
		if pid := f.listener(port); pid != 0 {
			return []byte(strconv.Itoa(pid) + "\n"), nil, nil
		}
		return nil, nil, fakeExit(1)
	case len(args) > 0 && args[0] == "ss":
		return nil, nil, fakeExit(1)
	}
	key, found := joined, false
	if _, found = f.answers[joined]; !found {
		best := -1
		for k := range f.answers {
			prefix, isPrefix := strings.CutSuffix(k, " *")
			if isPrefix && (joined == prefix || strings.HasPrefix(joined, prefix+" ")) && len(prefix) > best {
				key, best, found = k, len(prefix), true
			}
		}
	}
	if !found {
		return nil, []byte("fake host: unscripted command: " + joined + "\n"), fakeExit(127)
	}
	queue := f.answers[key]
	r := queue[0]
	if len(queue) > 1 {
		f.answers[key] = queue[1:]
	}
	if r.code != 0 {
		return []byte(r.stdout), []byte(r.stderr), fakeExit(r.code)
	}
	return []byte(r.stdout), []byte(r.stderr), nil
}

func (f *fakeHost) start(cmd *exec.Cmd) (*startedProcess, error) {
	f.mu.Lock()
	f.nextPID++
	pid := f.nextPID
	f.procs[pid] = &fakeProc{args: append([]string(nil), cmd.Args...), cwd: cmd.Dir, alive: true}
	f.started = append(f.started, cmd)
	exited, exitErr := f.startExited, f.startExitErr
	onStart := f.onStart
	f.mu.Unlock()
	if onStart != nil {
		onStart(pid, cmd)
	}
	p := &startedProcess{pid: pid, done: make(chan struct{})}
	if exited {
		p.err = exitErr
		close(p.done)
	}
	return p, nil
}

func (f *fakeHost) alive(pid int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := f.procs[pid]
	return p != nil && p.alive
}

func (f *fakeHost) signal(pid int, sig syscall.Signal) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, fakeSignal{pid, sig})
	p := f.procs[pid]
	if p == nil || !p.alive {
		return errors.New("os: process already finished")
	}
	if sig == syscall.SIGKILL || (sig == syscall.SIGTERM && !p.ignoresTERM) {
		p.alive = false
	}
	return nil
}

// signalsTo returns the signals sent to pid.
func (f *fakeHost) signalsTo(pid int) []syscall.Signal {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []syscall.Signal
	for _, s := range f.signals {
		if s.pid == pid {
			out = append(out, s.sig)
		}
	}
	return out
}

func (f *fakeHost) dial(address string, _ time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reachable[address] {
		return nil
	}
	if from, ok := f.reachableFrom[address]; ok && !f.now.Before(from) {
		return nil
	}
	if host, portStr, err := splitHostPort(address); err == nil && (host == "127.0.0.1" || host == "localhost") {
		port, _ := strconv.Atoi(portStr)
		if f.listener(port) != 0 {
			return nil
		}
	}
	return fmt.Errorf("dial tcp %s: connect: connection refused", address)
}

func splitHostPort(address string) (string, string, error) {
	i := strings.LastIndex(address, ":")
	if i < 0 {
		return "", "", errors.New("no port")
	}
	return address[:i], address[i+1:], nil
}

func (f *fakeHost) listen(address string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, portStr, _ := splitHostPort(address)
	port, _ := strconv.Atoi(portStr)
	if f.listener(port) != 0 || f.reachable[address] {
		return fmt.Errorf("listen tcp %s: bind: address already in use", address)
	}
	return nil
}

func (f *fakeHost) lookupHost(name string) ([]string, error) {
	switch name {
	case "localhost":
		return []string{"127.0.0.1"}, nil
	}
	return nil, fmt.Errorf("lookup %s: no such host", name)
}

func (f *fakeHost) readlink(path string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rest, ok := strings.CutPrefix(path, "/proc/")
	if pidStr, ok2 := strings.CutSuffix(rest, "/cwd"); ok && ok2 {
		pid, _ := strconv.Atoi(pidStr)
		if p := f.procs[pid]; p != nil && p.alive && p.cwd != "" {
			return p.cwd, nil
		}
	}
	return "", errors.New("no such file or directory")
}

func (f *fakeHost) clockNow() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeHost) sleep(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
	f.slept += d
}

// commands returns every command run, argv joined with spaces.
func (f *fakeHost) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

// ranMatching returns the commands containing substr.
func (f *fakeHost) ranMatching(substr string) []string {
	var out []string
	for _, c := range f.commands() {
		if strings.Contains(c, substr) {
			out = append(out, c)
		}
	}
	return out
}

// townPort gives every town on the fake machine the Dolt endpoint port, the
// way mayor/town.json does on a real one.
func (f *fakeHost) townPort(port int) *fakeHost {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.endpointPort = port
	return f
}

// doltEndpoint is the endpoint townPort set, else the town's config files.
func (f *fakeHost) doltEndpoint(townRoot string) (configpkg.DoltEndpoint, bool) {
	f.mu.Lock()
	port := f.endpointPort
	f.mu.Unlock()
	if port > 0 {
		return configpkg.DoltEndpoint{Port: port}, true
	}
	return configpkg.ResolveDoltEndpoint(townRoot)
}

// doltServer adds a live dolt sql-server of the town at townRoot listening on
// port, and returns its PID.
func (f *fakeHost) doltServer(townRoot string, port int) int {
	dataDir := filepath.Join(townRoot, ".dolt-data")
	return f.spawn(fakeProc{args: []string{"dolt", "sql-server", "--config", filepath.Join(dataDir, "config.yaml")}, cwd: dataDir, port: port})
}

// testTown returns a temporary town root with symlinks resolved, so it
// compares equal to the paths the adapter derives.
func testTown(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}
