// Package tmuxsweep finds and reaps the tmux servers test runs abandon. A
// suite that dies before its cleanup leaves its private server running, and
// its sessions read to the roster as phantom polecats: no worktree, no agent
// bead, and a candidate for auto-nuke (gt-2bj). Every test server also leaves
// its socket file behind.
//
// The scan is deliberately two-sided. Killing a live server needs its owner
// identified and confirmed gone, because a running test's server must survive
// the sweep. Removing a file nothing is listening on needs neither — an
// unserved socket is residue whoever made it, and requiring an owner pid there
// is what let the litter accumulate, since a pid recycled onto an unrelated
// live process made the file uncollectable forever (gt-20di).
//
// Both halves collect only on evidence that the server is gone, and a failed
// probe is not that evidence: a file left behind is collected by the next
// scan, while one removed out from under a live server leaves that server
// outside the sweep for good (gt-ri37).
//
// The daemon runs this on a cadence and the doctor exposes it as the
// tmux-test-socket check; internal/doctor cannot be imported here, since the
// checks import the daemon package (ADR 0005).
package tmuxsweep

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

// socketName extracts the owning pid from a socket name: the trailing digit
// run, which constants.TestSocketName puts there. The pid is what tells a
// leftover from a run in flight, so a name without one is a socket the scan
// cannot attribute (gt-2bj).
var socketName = regexp.MustCompile(`^(?:gt-test|gt-h9z)-.*?(\d+)$`)

// maxOwnerPid bounds what the trailing field may be read as. A pid fits in
// int32 on every platform gt supports; anything larger is a timestamp, which is
// always "dead" as a pid — the reading that would let this sweep kill a running
// test's server (gt-20di).
const maxOwnerPid = math.MaxInt32

// socketFamilies are the socket-name prefixes gt's suites bind their private
// servers to. A socket outside them is never touched: the town's own socket
// lives in the same directory.
var socketFamilies = []string{"gt-test-", "gt-h9z-"}

// sessionFamilies are the session-name prefixes those suites create. A server
// holding anything else is left running even when its owner is gone — an
// auto-nuke against the wrong session is the failure this guard exists to
// prevent (gt-2bj). The cost: internal/daemon names its sessions after
// production (hq-dog-<name>) on a gt-test-* socket, so that server is not
// collectable while it lives.
var sessionFamilies = []string{"gt-test-", "gt-h9z-"}

// staleSocketMinAge is how long a socket file must have sat with nothing
// listening before it counts as residue. The file exists for a moment before
// its server is listening on it, so sweeping on "nothing answers" alone could
// unlink a socket out from under a server that is mid-bind.
const staleSocketMinAge = 2 * time.Minute

// Server is the subset of tmux operations the sweep needs on a candidate
// socket.
type Server interface {
	ListSessions() ([]string, error)
	GetPanePID(target string) (string, error)
	PaneCurrentPath(session string) (string, error)
	KillServer() error
}

// Options is one sweep's inputs. A nil function takes the production one.
type Options struct {
	// SocketDir is the directory holding tmux sockets; "" is tmux.SocketDir().
	SocketDir string
	// Server returns the tmux client for a socket; nil is
	// tmux.NewTmuxWithSocket.
	Server func(socket string) Server
	// PIDAlive reports whether pid names a live process; nil signals it.
	PIDAlive func(pid int) bool
	// State dials a socket path; nil is DialState.
	State func(path string) State
	// Age is how long a socket file has existed; nil is its mtime, and a file
	// that cannot be stat'd reads as brand new, which keeps an unreadable
	// entry out of the sweep.
	Age func(path string) time.Duration
}

// Report is one scan: what it found, and what it could not establish.
type Report struct {
	// Leftovers are sockets whose owning test process is gone and whose server
	// answers with only test sessions.
	Leftovers []string
	// StaleFiles are socket files nothing is listening on, old enough to
	// collect.
	StaleFiles []string
	// Unprobed are sockets the scan could not establish anything about,
	// neither a server it can report nor a file it may remove. They are
	// carried so a caller reports an unknown rather than a pass it did not
	// earn.
	Unprobed []string
	// Evidence names each leftover's owner and what its sessions are.
	Evidence []string
}

// found reports whether the scan has residue to act on.
func (r Report) Found() bool { return len(r.Leftovers) > 0 || len(r.StaleFiles) > 0 }

func (o Options) socketDir() string {
	if o.SocketDir != "" {
		return o.SocketDir
	}
	return tmux.SocketDir()
}

func (o Options) server(socket string) Server {
	if o.Server != nil {
		return o.Server(socket)
	}
	return tmux.NewTmuxWithSocket(socket)
}

func (o Options) state(path string) State {
	if o.State != nil {
		return o.State(path)
	}
	return DialState(path)
}

func (o Options) age(path string) time.Duration {
	if o.Age != nil {
		return o.Age(path)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return 0
	}
	return time.Since(info.ModTime())
}

func (o Options) ownerAlive(pid int) bool {
	if o.PIDAlive != nil {
		return o.PIDAlive(pid)
	}
	return PIDAlive(pid)
}

// PIDAlive reports whether pid names a process. It is true for a process this
// user cannot signal (EPERM): the process exists, which is all the sweep asks.
func PIDAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	sigErr := p.Signal(syscall.Signal(0))
	return sigErr == nil || errors.Is(sigErr, syscall.EPERM)
}

// Scan reads the socket directory and reports what it found. A directory it
// cannot read is an error: the caller then reports an unknown, never a pass.
func Scan(opts Options) (Report, error) {
	dir := opts.socketDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Report{}, err
	}

	var r Report
	for _, e := range entries {
		socket := e.Name()
		if !IsTestSocketName(socket) {
			continue
		}
		path := filepath.Join(dir, socket)
		switch opts.state(path) {
		case Unreachable:
			// The dial could not be made, which says nothing about the server.
			r.Unprobed = append(r.Unprobed, socket)
			continue
		case Gone:
			// Unlinked while the scan ran; there is nothing to collect.
			continue
		case Refused:
			if opts.age(path) >= staleSocketMinAge {
				r.StaleFiles = append(r.StaleFiles, socket)
			}
			continue
		}

		pid, ok := CandidateOwnerPid(socket)
		if !ok || opts.ownerAlive(pid) {
			continue
		}
		server := opts.server(socket)
		sessions, err := server.ListSessions()
		if err != nil {
			// The dial answered but tmux could not be asked. Same asymmetry as
			// the dial above: an unreadable server is not a departed one, and
			// removing the file here would drop it out of every later scan.
			r.Unprobed = append(r.Unprobed, socket)
			continue
		}
		if len(sessions) == 0 {
			// The dial answered, so did the server, and it holds no sessions:
			// it exited between the two, leaving the file behind (gt-2bj).
			r.StaleFiles = append(r.StaleFiles, socket)
			continue
		}
		if !allTestSessions(sessions) {
			continue
		}
		sort.Strings(sessions)
		r.Leftovers = append(r.Leftovers, socket)
		r.Evidence = append(r.Evidence, fmt.Sprintf("%s (owner pid %d is gone): %s",
			socket, pid, strings.Join(evidence(server, sessions), ", ")))
	}

	sort.Strings(r.Leftovers)
	sort.Strings(r.StaleFiles)
	sort.Strings(r.Unprobed)
	return r, nil
}

// Reap kills each abandoned server and unlinks the socket files. A server
// unlinks its own socket on the way out, so the explicit Remove covers the one
// that died between Scan and Reap, and the files whose server had already
// exited.
//
// Nothing unprobed is touched: those are the sockets Scan could not make a
// finding about, and a reap has no more evidence than the scan it follows
// (gt-ri37).
func Reap(opts Options, r Report) error {
	var firstErr error
	for _, socket := range r.Leftovers {
		if err := opts.server(socket).KillServer(); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("killing tmux server on %s: %w", socket, err)
			}
			continue
		}
		_ = os.Remove(filepath.Join(opts.socketDir(), socket))
	}
	for _, socket := range r.StaleFiles {
		_ = os.Remove(filepath.Join(opts.socketDir(), socket))
	}
	return firstErr
}

// IsTestSocketName reports whether the socket belongs to one of the test
// families this sweep owns.
func IsTestSocketName(socket string) bool {
	for _, family := range socketFamilies {
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
		if !IsTestSessionName(s) {
			return false
		}
	}
	return true
}

// IsTestSessionName reports whether a tmux session name belongs to one of the
// families gt's suites create. A session named otherwise is never swept: an
// auto-nuke against the wrong session is the failure this guard exists to
// prevent (gt-2bj).
func IsTestSessionName(session string) bool {
	return hasAnyPrefix(session, sessionFamilies)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// CandidateOwnerPid extracts the owning test process's pid from a socket name.
// It reports false when the trailing field is not a plausible pid, which means
// the socket does not name its owner (tmux test sockets put the pid last for
// exactly this reason).
func CandidateOwnerPid(socket string) (int, bool) {
	m := socketName.FindStringSubmatch(socket)
	if m == nil {
		return 0, false
	}
	pid, err := strconv.Atoi(m[1])
	if err != nil || pid <= 0 || pid > maxOwnerPid {
		return 0, false
	}
	return pid, true
}

// evidence pairs each session with the pane pid and cwd that identify what
// left it behind. Phantom sessions are short-lived, so a report that names
// only the session leaves the reader where the name-only sightings did
// (gt-2bj).
func evidence(server Server, sessions []string) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		parts := []string{}
		if pid, err := server.GetPanePID(s); err == nil && pid != "" {
			parts = append(parts, "pane_pid="+pid)
		}
		if cwd, err := server.PaneCurrentPath(s); err == nil && cwd != "" {
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

// State is what a dial of the socket file can conclude. The outcomes are not
// two: only a refusal is evidence about the server, and reading a dial that
// cannot be made as absence is how the sweep came to unlink the sockets of live
// servers (gt-ri37).
type State int

const (
	// Serving: a server accepted the connection.
	Serving State = iota
	// Refused: the file is there and nothing holds it — the one outcome that
	// makes the file residue.
	Refused
	// Gone: the file was unlinked mid-scan, so there is nothing to collect.
	Gone
	// Unreachable: the dial failed for a reason that is not the server's
	// absence, leaving what holds the socket unknown and the file untouchable.
	Unreachable
)

// DialState reports what holds the socket file. A file whose server exited
// refuses the connection instantly, which is what keeps the directory scan off
// a tmux spawn per stale file.
func DialState(path string) State {
	conn, err := net.DialTimeout("unix", path, 250*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		return Serving
	}
	return ClassifyDialErr(err)
}

// ClassifyDialErr maps a failed dial onto what it says about the server. A
// refusal and an unlinked path are the only answers about the socket itself;
// EMFILE and ENFILE from this process's descriptor table, EACCES, EAGAIN, and a
// dial that timed out are answers about the probe.
//
// The timeout is the one worth naming, because it is not hypothetical: a server
// with a full listen queue accepts nothing until it drains, so a busy run's live
// server presents as a dial that hangs, and calling that absence would unlink
// the socket of a test in flight (gt-ri37).
func ClassifyDialErr(err error) State {
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return Refused
	case errors.Is(err, syscall.ENOENT):
		return Gone
	default:
		return Unreachable
	}
}
