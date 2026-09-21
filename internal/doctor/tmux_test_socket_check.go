package doctor

import (
	"errors"
	"fmt"
	"math"
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

// testSocketName extracts the owning pid from a socket name: the trailing digit
// run, which constants.TestSocketName puts there. The pid is what tells a
// leftover from a run in flight, so a name without one is a socket this check
// cannot attribute (gt-2bj).
var testSocketName = regexp.MustCompile(`^(?:gt-test|gt-h9z)-.*?(\d+)$`)

// maxOwnerPid bounds what the trailing field may be read as. A pid fits in
// int32 on every platform gt supports; anything larger is a timestamp, which is
// always "dead" as a pid — the reading that would let this check kill a running
// test's server (gt-20di).
const maxOwnerPid = math.MaxInt32

// testSocketFamilies are the socket-name prefixes gt's suites bind their private
// servers to. A socket outside them is never touched: the town's own socket
// lives in the same directory.
var testSocketFamilies = []string{"gt-test-", "gt-h9z-"}

// testSessionFamilies are the session-name prefixes those suites create. A
// server holding anything else is left running even when its owner is gone — an
// auto-nuke against the wrong session is the failure this guard exists to
// prevent (gt-2bj). The cost: internal/daemon names its sessions after
// production (hq-dog-<name>) on a gt-test-* socket, so that server is not
// collectable while it lives.
var testSessionFamilies = []string{"gt-test-", "gt-h9z-"}

// staleSocketMinAge is how long a socket file must have sat with nothing
// listening before it counts as residue. The file exists for a moment before
// its server is listening on it, so sweeping on "nothing answers" alone could
// unlink a socket out from under a server that is mid-bind.
const staleSocketMinAge = 2 * time.Minute

// testSocketProbe is the subset of tmux operations this check needs on a
// candidate socket. Injected in tests.
type testSocketProbe interface {
	ListSessions() ([]string, error)
	GetPanePID(target string) (string, error)
	PaneCurrentPath(session string) (string, error)
	KillServer() error
}

// TmuxTestSocketCheck reports tmux servers left behind by test runs that died
// before their cleanup, and the socket files every test server leaves behind.
// A leftover server's sessions are test-named, which is the shape the roster
// reads as a phantom polecat: no worktree, no agent bead, and a candidate for
// auto-nuke (gt-2bj).
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
	socketAgeForTest func(path string) time.Duration     // override for the file's mtime
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

// socketAge is how long the socket file has existed. A file that cannot be
// stat'd reads as brand new, which keeps an unreadable entry out of the sweep.
func (c *TmuxTestSocketCheck) socketAge(path string) time.Duration {
	if c.socketAgeForTest != nil {
		return c.socketAgeForTest(path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return time.Since(info.ModTime())
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

// isTestSocketName reports whether the socket belongs to one of the test
// families this check owns.
func isTestSocketName(socket string) bool {
	for _, family := range testSocketFamilies {
		if strings.HasPrefix(socket, family) {
			return true
		}
	}
	return false
}

// allTestSessions reports whether every session on the socket is test-named.
// A socket serving anything else is not ours to kill.
func allTestSessions(sessions []string) bool {
	for _, s := range sessions {
		if !hasAnyPrefix(s, testSessionFamilies) {
			return false
		}
	}
	return true
}

// hasAnyPrefix reports whether s starts with one of the prefixes.
func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// candidateOwnerPid extracts the owning test process's pid from a socket name.
// It reports false when the trailing field is not a plausible pid, which means
// the socket does not name its owner (constants.TestSocketName puts the pid
// last for exactly this reason).
func candidateOwnerPid(socket string) (int, bool) {
	m := testSocketName.FindStringSubmatch(socket)
	if m == nil {
		return 0, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 || pid > maxOwnerPid {
		return 0, false
	}
	return pid, true
}

// Run reports every test socket whose owner is gone and whose server still
// answers with only test sessions. A live owner means a run is in flight, and a
// non-test session means the socket serves something the check must not kill.
// Sockets whose server already exited are counted, not reported: they are
// litter whose removal keeps the scan off a tmux spawn per file.
//
// The two halves need different evidence. Killing a live server needs its owner
// identified and confirmed gone, because a running test's server must survive
// the sweep. Removing a file nothing is listening on needs neither — an unserved
// socket is residue whoever made it, and requiring an owner pid there is what let
// the litter accumulate, since a pid recycled onto an unrelated live process
// made the file uncollectable forever (gt-20di).
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
		if !isTestSocketName(socket) {
			continue
		}
		path := filepath.Join(c.socketDir(), socket)
		if !c.serving(path) {
			if c.socketAge(path) >= staleSocketMinAge {
				c.staleFiles = append(c.staleFiles, socket)
			}
			continue
		}

		pid, ok := candidateOwnerPid(socket)
		if !ok || c.ownerAlive(pid) {
			continue
		}
		probe := c.probe(socket)
		sessions, err := probe.ListSessions()
		if err != nil || len(sessions) == 0 {
			// The dial answered but tmux does not: the server exited between
			// the two, leaving the file behind (gt-2bj).
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
