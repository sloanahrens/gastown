package tmuxsweep

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// fakeServer stands in for the tmux client on one socket.
type fakeServer struct {
	sessions []string
	listErr  error
	killErr  error
	killed   bool
}

func (f *fakeServer) ListSessions() ([]string, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.sessions, nil
}

func (f *fakeServer) GetPanePID(string) (string, error) { return "4242", nil }

func (f *fakeServer) PaneCurrentPath(string) (string, error) {
	return "/private/var/folders/xy/T", nil
}

func (f *fakeServer) KillServer() error {
	if f.killErr != nil {
		return f.killErr
	}
	f.killed = true
	return nil
}

// socketDir builds a socket directory holding one file per name, each aged.
func socketDir(t *testing.T, age time.Duration, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, name := range names {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatalf("writing socket file %s: %v", name, err)
		}
		if age > 0 {
			when := time.Now().Add(-age)
			if err := os.Chtimes(path, when, when); err != nil {
				t.Fatalf("aging socket file %s: %v", name, err)
			}
		}
	}
	return dir
}

// servingOpts is the sweep of a directory whose sockets all answer.
func servingOpts(dir string, ownerAlive bool, server Server) Options {
	return Options{
		SocketDir: dir,
		Server:    func(string) Server { return server },
		PIDAlive:  func(int) bool { return ownerAlive },
		State:     func(string) State { return Serving },
	}
}

func TestScanReportsLeftoverWhenOwnerIsGone(t *testing.T) {
	t.Parallel()
	server := &fakeServer{sessions: []string{"gt-test-modeA-2"}}
	r, err := Scan(servingOpts(socketDir(t, 0, "gt-test-91506"), false, server))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.Leftovers) != 1 || r.Leftovers[0] != "gt-test-91506" {
		t.Fatalf("Leftovers = %v, want [gt-test-91506]", r.Leftovers)
	}
	if len(r.Evidence) != 1 {
		t.Fatalf("Evidence = %v, want one line", r.Evidence)
	}
}

func TestScanLeavesLiveOwnersAlone(t *testing.T) {
	t.Parallel()
	server := &fakeServer{sessions: []string{"gt-test-modeA-2"}}
	r, err := Scan(servingOpts(socketDir(t, 0, "gt-test-91506"), true, server))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.Found() {
		t.Fatalf("a live owner's server was swept: %+v", r)
	}
}

func TestScanLeavesForeignSessionsAlone(t *testing.T) {
	t.Parallel()
	server := &fakeServer{sessions: []string{"hq-dog-doctor"}}
	r, err := Scan(servingOpts(socketDir(t, 0, "gt-test-91506"), false, server))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.Found() {
		t.Fatalf("a server holding a non-test session was swept: %+v", r)
	}
}

func TestScanIgnoresSocketsOutsideTheTestFamilies(t *testing.T) {
	t.Parallel()
	server := &fakeServer{sessions: []string{"gt-test-modeA-2"}}
	r, err := Scan(servingOpts(socketDir(t, time.Hour, "gt-3aa519", "default"), false, server))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.Found() {
		t.Fatalf("a non-test socket was swept: %+v", r)
	}
}

func TestScanAgesOutASocketFileNothingHolds(t *testing.T) {
	t.Parallel()
	opts := Options{
		SocketDir: socketDir(t, 2*time.Hour, "gt-test-91506"),
		PIDAlive:  func(int) bool { return false },
		State:     func(string) State { return Refused },
	}
	r, err := Scan(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if len(r.StaleFiles) != 1 {
		t.Fatalf("StaleFiles = %v, want the old unheld file", r.StaleFiles)
	}
}

func TestScanKeepsAFreshSocketFile(t *testing.T) {
	t.Parallel()
	opts := Options{
		SocketDir: socketDir(t, 0, "gt-test-91506"),
		State:     func(string) State { return Refused },
	}
	r, err := Scan(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.Found() {
		t.Fatalf("a file young enough to be mid-bind was swept: %+v", r)
	}
}

func TestScanCarriesUnprobedRatherThanPassing(t *testing.T) {
	t.Parallel()
	opts := Options{
		SocketDir: socketDir(t, time.Hour, "gt-test-91506"),
		State:     func(string) State { return Unreachable },
	}
	r, err := Scan(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if r.Found() || len(r.Unprobed) != 1 {
		t.Fatalf("Report = %+v, want one unprobed socket and nothing found", r)
	}
}

func TestScanReportsAnUnreadableDirectory(t *testing.T) {
	t.Parallel()
	if _, err := Scan(Options{SocketDir: filepath.Join(t.TempDir(), "gone")}); err == nil {
		t.Fatal("Scan of a missing directory = nil error, want the read failure")
	}
}

func TestReapKillsLeftoversAndRemovesFiles(t *testing.T) {
	t.Parallel()
	server := &fakeServer{sessions: []string{"gt-test-modeA-2"}}
	dir := socketDir(t, 2*time.Hour, "gt-test-91506", "gt-test-91507")
	opts := servingOpts(dir, false, server)
	opts.State = func(path string) State {
		if filepath.Base(path) == "gt-test-91507" {
			return Refused
		}
		return Serving
	}
	r, err := Scan(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := Reap(opts, r); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if !server.killed {
		t.Error("Reap did not kill the abandoned server")
	}
	for _, name := range []string{"gt-test-91506", "gt-test-91507"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("socket file %s still present after Reap: %v", name, err)
		}
	}
}

func TestReapSurfacesAKillFailure(t *testing.T) {
	t.Parallel()
	server := &fakeServer{sessions: []string{"gt-test-modeA-2"}, killErr: errors.New("no server")}
	dir := socketDir(t, 0, "gt-test-91506")
	opts := servingOpts(dir, false, server)
	r, err := Scan(opts)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := Reap(opts, r); err == nil {
		t.Fatal("Reap = nil error, want the kill failure")
	}
}

// TestClassifyDialErr covers the reason a failed dial must not be read as
// absence: the process's own limits, permissions, and a busy server's timeout
// all end the dial without saying anything about the server.
func TestClassifyDialErr(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		err  error
		want State
	}{
		{"refused", &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED}, Refused},
		{"gone", &os.SyscallError{Syscall: "connect", Err: syscall.ENOENT}, Gone},
		{"descriptor table full", &os.SyscallError{Syscall: "socket", Err: syscall.EMFILE}, Unreachable},
		{"no permission", &os.SyscallError{Syscall: "connect", Err: syscall.EACCES}, Unreachable},
		{"timed out on a full backlog", &os.SyscallError{Syscall: "connect", Err: syscall.ETIMEDOUT}, Unreachable},
		{"deadline exceeded", os.ErrDeadlineExceeded, Unreachable},
		{"anything else", errors.New("dial unix: unexpected"), Unreachable},
	}
	for _, tc := range cases {
		if got := ClassifyDialErr(tc.err); got != tc.want {
			t.Errorf("ClassifyDialErr(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCandidateOwnerPid(t *testing.T) {
	t.Parallel()
	cases := []struct {
		socket string
		want   int
		ok     bool
	}{
		{"gt-test-91506", 91506, true},
		{"gt-test-daemon-20703", 20703, true},
		{"gt-test-config-4242", 4242, true},
		{"gt-h9z-live-1758012345678901234-4242", 4242, true},
		{"gt-3aa519", 0, false},
		{"gt-test-sentinel", 0, false},
		{"default", 0, false},
		// A timestamp in the trailing position is not a pid, and reading one as
		// a pid is what made the sweep see every such socket's owner as dead.
		{"gt-test-dog-stale-1758012345678901234", 0, false},
	}
	for _, tc := range cases {
		got, ok := CandidateOwnerPid(tc.socket)
		if ok != tc.ok || got != tc.want {
			t.Errorf("CandidateOwnerPid(%q) = (%d, %v), want (%d, %v)", tc.socket, got, ok, tc.want, tc.ok)
		}
	}
}
