package doctor

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

// testSocketName matches the sockets hermetic test runs create: a gt-test-
// prefix followed by the pid of the test process that bound it. The pid is what
// makes a leftover distinguishable from a run in flight — the socket outlives
// its owner when the test process is killed before its cleanup runs, and then
// keeps a tmux server and its sessions alive indefinitely (gt-2bj).
var testSocketName = regexp.MustCompile(`^gt-test-.*?(\d+)$`)

// testSocketProbe is the subset of tmux operations this check needs on a
// candidate socket. Injected in tests.
type testSocketProbe interface {
	ListSessions() ([]string, error)
	GetPanePID(target string) (string, error)
	PaneCurrentPath(session string) (string, error)
	KillServer() error
}

// TmuxTestSocketCheck reports tmux servers left behind by test runs that died
// before their cleanup. Their sessions are gt-test-* named, which is the shape
// the roster reads as a phantom polecat: no worktree, no agent bead, and a
// candidate for auto-nuke (gt-2bj).
type TmuxTestSocketCheck struct {
	FixableCheck

	// leftovers holds socket names whose owning test process is gone, found by
	// Run and reaped by Fix.
	leftovers []string
	// staleFiles holds socket files left by runs whose server already exited.
	staleFiles []string

	socketDirForTest string                              // override for tmux.SocketDir()
	probeForTest     func(socket string) testSocketProbe // override for the real tmux client
	pidAliveForTest  func(pid int) bool                  // override for os.FindProcess
	servingForTest   func(path string) bool              // override for socketServing
}

// NewTmuxTestSocketCheck creates a check for abandoned test tmux servers.
func NewTmuxTestSocketCheck() *TmuxTestSocketCheck {
	return &TmuxTestSocketCheck{
		FixableCheck: FixableCheck{
			BaseCheck: BaseCheck{
				CheckName:        "tmux-test-socket",
				CheckDescription: "Detect tmux servers abandoned by killed test runs",
				CheckCategory:    CategoryInfrastructure,
			},
		},
	}
}

// socketDir is a method so tests can point the scan at a scratch directory.
func (c *TmuxTestSocketCheck) socketDir() string {
	if c.socketDirForTest != "" {
		return c.socketDirForTest
	}
	return tmux.SocketDir()
}

func (c *TmuxTestSocketCheck) probe(socket string) testSocketProbe {
	if c.probeForTest != nil {
		return c.probeForTest(socket)
	}
	return tmux.NewTmuxWithSocket(socket)
}

func (c *TmuxTestSocketCheck) serving(path string) bool {
	if c.servingForTest != nil {
		return c.servingForTest(path)
	}
	return socketServing(path)
}

// ownerAlive reports whether the test process named in the socket still exists.
func (c *TmuxTestSocketCheck) ownerAlive(pid int) bool {
	if c.pidAliveForTest != nil {
		return c.pidAliveForTest(pid)
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// FindProcess succeeds for any pid on Unix; signal 0 is what tests it. A
	// refusal rather than an ESRCH means the process exists under another user.
	sigErr := p.Signal(syscall.Signal(0))
	return sigErr == nil || errors.Is(sigErr, syscall.EPERM)
}

// allTestSessions reports whether every session on the socket is test-named.
// A socket serving anything else is not ours to kill.
func allTestSessions(sessions []string) bool {
	for _, s := range sessions {
		if !strings.HasPrefix(s, "gt-test-") {
			return false
		}
	}
	return true
}

// candidateOwnerPid extracts the owning test process's pid from a socket name.
func candidateOwnerPid(socket string) (int, bool) {
	m := testSocketName.FindStringSubmatch(socket)
	if m == nil {
		return 0, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// Run reports every test socket whose owner is gone and whose server still
// answers with only gt-test-* sessions. A live owner means a run is in flight,
// and a non-test session means the socket serves something the check must not
// kill. Sockets whose server already exited are counted, not reported: they are
// litter (1,200+ files on a host that runs the suite often) whose removal is
// what keeps the scan off a tmux spawn per file.
func (c *TmuxTestSocketCheck) Run(ctx *CheckContext) *CheckResult {
	c.leftovers = nil
	c.staleFiles = nil

	entries, err := os.ReadDir(c.socketDir())
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusSkipped,
			Message: "unknown: could not read the tmux socket directory",
			Details: []string{err.Error()},
		}
	}

	var evidence []string
	for _, e := range entries {
		socket := e.Name()
		pid, ok := candidateOwnerPid(socket)
		if !ok || c.ownerAlive(pid) {
			continue
		}
		if !c.serving(filepath.Join(c.socketDir(), socket)) {
			c.staleFiles = append(c.staleFiles, socket)
			continue
		}
		probe := c.probe(socket)
		sessions, err := probe.ListSessions()
		if err != nil || len(sessions) == 0 {
			c.staleFiles = append(c.staleFiles, socket)
			continue
		}
		if !allTestSessions(sessions) {
			continue
		}
		sort.Strings(sessions)
		c.leftovers = append(c.leftovers, socket)
		evidence = append(evidence, fmt.Sprintf("%s (owner pid %d is gone): %s",
			socket, pid, strings.Join(c.sessionEvidence(probe, sessions), ", ")))
	}

	sort.Strings(c.staleFiles)
	if len(c.leftovers) == 0 && len(c.staleFiles) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusOK,
			Message: "No abandoned test tmux servers",
		}
	}

	if len(c.leftovers) == 0 {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("%d socket file(s) left by test runs whose server has exited", len(c.staleFiles)),
			Details: []string{fmt.Sprintf("e.g. %s", strings.Join(firstFew(c.staleFiles, 5), ", "))},
			FixHint: "Run 'gt doctor --fix' to remove them",
		}
	}

	details := evidence
	if len(c.staleFiles) > 0 {
		details = append(details, fmt.Sprintf("plus %d socket file(s) whose server has exited", len(c.staleFiles)))
	}
	sort.Strings(c.leftovers)
	return &CheckResult{
		Name:   c.Name(),
		Status: StatusWarning,
		Message: fmt.Sprintf("%d abandoned test tmux server(s) — their sessions read as phantom polecats (gt-2bj)",
			len(c.leftovers)),
		Details: details,
		FixHint: "Run 'gt doctor --fix' to kill them",
	}
}

// firstFew returns at most n names, for a detail line that counts in full but
// names a sample.
func firstFew(names []string, n int) []string {
	if len(names) <= n {
		return names
	}
	return names[:n]
}

// socketServing reports whether anything is listening on the socket file. A
// socket file whose server exited refuses the connection instantly, which is
// what keeps the directory scan off a `tmux` spawn per stale file.
func socketServing(path string) bool {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// sessionEvidence pairs each session with the pane pid and cwd that identify
// what left it behind. Phantom sessions are short-lived, so a report that names
// only the session leaves the reader where the name-only sightings did (gt-2bj).
func (c *TmuxTestSocketCheck) sessionEvidence(probe testSocketProbe, sessions []string) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		parts := []string{}
		if pid, err := probe.GetPanePID(s); err == nil && pid != "" {
			parts = append(parts, "pane_pid="+pid)
		}
		if cwd, err := probe.PaneCurrentPath(s); err == nil && cwd != "" {
			parts = append(parts, "cwd="+cwd)
		}
		if len(parts) == 0 {
			out = append(out, s)
			continue
		}
		out = append(out, fmt.Sprintf("%s [%s]", s, strings.Join(parts, " ")))
	}
	return out
}

// Fix kills each abandoned server and unlinks the socket files. A server
// unlinks its own socket on the way out, so the explicit Remove covers the one
// that died between Run and Fix, and the files whose server had already exited.
func (c *TmuxTestSocketCheck) Fix(ctx *CheckContext) error {
	var firstErr error
	for _, socket := range c.leftovers {
		if err := c.probe(socket).KillServer(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("killing tmux server on %s: %w", socket, err)
			}
			continue
		}
		_ = os.Remove(filepath.Join(c.socketDir(), socket))
	}
	for _, socket := range c.staleFiles {
		_ = os.Remove(filepath.Join(c.socketDir(), socket))
	}
	c.leftovers = nil
	c.staleFiles = nil
	return firstErr
}
