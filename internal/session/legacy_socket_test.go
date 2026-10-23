package session

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

type mockLegacyTmux struct {
	sessions []string
	listErr  error
	killed   []string
	killErr  error
}

func (m *mockLegacyTmux) ListSessions() ([]string, error) {
	if len(m.killed) == 0 {
		return m.sessions, m.listErr
	}
	killed := make(map[string]bool, len(m.killed))
	for _, sess := range m.killed {
		killed[sess] = true
	}
	remaining := make([]string, 0, len(m.sessions))
	for _, sess := range m.sessions {
		if !killed[sess] {
			remaining = append(remaining, sess)
		}
	}
	return remaining, m.listErr
}

func (m *mockLegacyTmux) KillSessionWithProcesses(name string) error {
	if m.killErr != nil {
		return m.killErr
	}
	m.killed = append(m.killed, name)
	return nil
}

// fakeLegacySockets reads currentSocket as this process's socket, answers
// every other socket with mock, and knows rig prefix "ga".
func fakeLegacySockets(currentSocket string, mock *mockLegacyTmux) legacySockets {
	r := NewPrefixRegistry()
	r.Register("ga", "gastown")
	return legacySockets{
		current:  currentSocket,
		open:     func(string) legacySocketTmux { return mock },
		prefixes: r,
	}
}

func TestCleanupLegacyDefaultSocketSkipsWhenOnDefaultSocket(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{}
	l := fakeLegacySockets("", mock)

	got := l.cleanupDefault()
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCleanupLegacyDefaultSocketSkipsWhenSocketIsDefault(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{}
	l := fakeLegacySockets("default", mock)

	got := l.cleanupDefault()
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCleanupLegacyDefaultSocketCleansGastownSessions(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{
		sessions: []string{"ga-witness", "hq-deacon"},
	}
	l := fakeLegacySockets("gt-abc123", mock)

	got := l.cleanupDefault()
	if got != 2 {
		t.Errorf("expected 2 cleaned, got %d", got)
	}
	if len(mock.killed) != 2 {
		t.Fatalf("expected 2 killed, got %d: %v", len(mock.killed), mock.killed)
	}
	want := map[string]bool{"ga-witness": true, "hq-deacon": true}
	for _, k := range mock.killed {
		if !want[k] {
			t.Errorf("unexpected kill: %s", k)
		}
	}
}

func TestCleanupLegacyDefaultSocketIgnoresNonGastownSessions(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{
		sessions: []string{"personal-stuff", "hq-notes", "ga-witness"},
	}
	l := fakeLegacySockets("gt-abc123", mock)

	got := l.cleanupDefault()
	if got != 1 {
		t.Errorf("expected 1 cleaned, got %d", got)
	}
	if len(mock.killed) != 1 || mock.killed[0] != "ga-witness" {
		t.Errorf("expected only ga-witness killed, got %v", mock.killed)
	}
}

func TestCleanupLegacyDefaultSocketCleansSpecificTownSessions(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{
		sessions: []string{"hq-deacon", "hq-boot", "hq-dog-alpha", "hq-overseer"},
	}
	l := fakeLegacySockets("gt-abc123", mock)

	got := l.cleanupDefault()
	if got != 3 {
		t.Errorf("expected 3 cleaned, got %d", got)
	}
	if len(mock.killed) != 3 {
		t.Fatalf("expected 3 killed, got %d: %v", len(mock.killed), mock.killed)
	}
	for _, killed := range mock.killed {
		if killed == "hq-overseer" {
			t.Fatal("did not expect hq-overseer to be killed")
		}
	}
}

func TestCleanupLegacyDefaultSocketNoDefaultServer(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{
		listErr: fmt.Errorf("no server running"),
	}
	l := fakeLegacySockets("gt-abc123", mock)

	got := l.cleanupDefault()
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCountLegacyDefaultSocketSkipsWhenOnDefault(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{}
	l := fakeLegacySockets("", mock)

	got := l.countDefault()
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCountLegacyDefaultSocketCountsGastownOnly(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{
		sessions: []string{"ga-witness", "personal", "hq-notes"},
	}
	l := fakeLegacySockets("gt-abc123", mock)

	got := l.countDefault()
	if got != 1 {
		t.Errorf("expected 1, got %d", got)
	}
}

func TestCleanupLegacyBaseSocketSkipsWhenSameSocket(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{}
	l := fakeLegacySockets("gt", mock)

	got := l.cleanupBase("/some/path/gt")
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCleanupLegacyBaseSocketCleansOldSessions(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{
		sessions: []string{"ga-witness"},
	}
	l := fakeLegacySockets("gt-abc123", mock)

	got := l.cleanupBase("/some/path/gt")
	if got != 1 {
		t.Errorf("expected 1 cleaned, got %d", got)
	}
	if len(mock.killed) != 1 || mock.killed[0] != "ga-witness" {
		t.Errorf("expected ga-witness killed, got %v", mock.killed)
	}
}

func TestCountLegacyBaseSocketSkipsWhenSame(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{}
	l := fakeLegacySockets("gt", mock)

	got := l.countBase("/some/path/gt")
	if got != 0 {
		t.Errorf("expected 0, got %d", got)
	}
}

func TestCountLegacyBaseSocketCountsCorrectly(t *testing.T) {
	t.Parallel()
	mock := &mockLegacyTmux{
		sessions: []string{"ga-witness", "hq-deacon", "random-thing"},
	}
	l := fakeLegacySockets("gt-abc123", mock)

	got := l.countBase("/some/path/gt")
	if got != 2 {
		t.Errorf("expected 2, got %d", got)
	}
}

// A sandboxed session must never be listed, counted or killed by the legacy
// socket cleanup. The router and its extra-sessions hook name a sandbox session;
// the clients are built the way production builds them, over canned tmux output.
//
//testpolicy:allow parallel — installs the process-wide session router, extra-sessions hook and default socket, which another test must not see mid-run
func TestLegacySocketCleanupNeverTouchesASandboxSession(t *testing.T) {
	var hostCalls, sandboxCalls []string
	hostGone := false
	hostExec := func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		hostCalls = append(hostCalls, name+" "+strings.Join(args, " "))
		switch {
		case slices.Contains(args, "list-sessions"):
			if hostGone {
				return nil, []byte("no server running"), errors.New("exit status 1")
			}
			return []byte("ga-host\n"), nil, nil
		case slices.Contains(args, "kill-session"):
			hostGone = true
		}
		return nil, nil, nil
	}
	sandboxExec := func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		sandboxCalls = append(sandboxCalls, name+" "+strings.Join(args, " "))
		return nil, nil, nil
	}
	sandboxClient := tmux.NewTmuxWithExec("gt", sandboxExec).Routed()
	tmux.SetSessionRouter(func(sess string) *tmux.Tmux {
		if sess == "ga-bullet" {
			return sandboxClient
		}
		return nil
	})
	tmux.SetExtraSessions(func() []string { return []string{"ga-bullet"} })
	tmux.SetDefaultSocket("gt-legacy-test")
	t.Cleanup(func() {
		tmux.SetDefaultSocket("")
		tmux.SetExtraSessions(nil)
		tmux.SetSessionRouter(nil)
	})
	registry := NewPrefixRegistry()
	registry.Register("ga", "gastown")
	l := newLegacySocketsWith(registry, func(string) *tmux.Tmux { return tmux.NewTmuxWithExec("legacy", hostExec) })

	if got := l.countDefault(); got != 1 {
		t.Errorf("countDefault = %d, want 1 (the host session; the sandbox session is not on the socket)", got)
	}
	if got := l.cleanupDefault(); got != 1 {
		t.Errorf("cleanupDefault = %d, want 1", got)
	}

	if len(sandboxCalls) != 0 {
		t.Errorf("the cleanup reached into the sandbox: %v", sandboxCalls)
	}
	for _, c := range hostCalls {
		if strings.Contains(c, "ga-bullet") {
			t.Errorf("the legacy socket was asked about the sandbox session: %s", c)
		}
	}
	if !slices.ContainsFunc(hostCalls, func(c string) bool { return strings.Contains(c, "kill-session") && strings.Contains(c, "ga-host") }) {
		t.Errorf("the host session was not cleaned: %v", hostCalls)
	}
}
