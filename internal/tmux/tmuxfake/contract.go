// Package tmuxfake is an in-memory tmux server for unit tests. Its behavior
// is pinned to real tmux by RunSessionsContract, which runs against the fake in
// the unit tier and against a real tmux server in the integration tier.
package tmuxfake

import (
	"errors"
	"path/filepath"
	"sort"
	"testing"

	"github.com/steveyegge/gastown/internal/tmux"
)

// Sessions is the session-lifecycle surface that both the fake and *tmux.Tmux provide.
type Sessions interface {
	NewSession(name, workDir string) error
	NewSessionWithCommandAndEnv(name, workDir, command string, env map[string]string) error
	HasSession(name string) (bool, error)
	ListSessions() ([]string, error)
	KillSession(name string) error
	KillSessionWithProcesses(name string) error
	SetEnvironment(session, key, value string) error
	GetEnvironment(session, key string) (string, error)
	GetPaneCommand(session string) (string, error)
	CapturePane(session string, lines int) (string, error)
	SendKeys(session, keys string) error
}

// RunSessionsContract checks the behavior every Sessions implementation must share.
func RunSessionsContract(t *testing.T, newImpl func(t *testing.T) Sessions) {
	t.Run("new/has/list/kill", func(t *testing.T) {
		s := newImpl(t)
		dir := t.TempDir()
		if err := s.NewSession("gt-c-a", dir); err != nil {
			t.Fatal(err)
		}
		if err := s.NewSession("gt-c-b", dir); err != nil {
			t.Fatal(err)
		}
		if ok, err := s.HasSession("gt-c-a"); err != nil || !ok {
			t.Fatalf("HasSession(a) = %v, %v", ok, err)
		}
		names, err := s.ListSessions()
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		if len(names) != 2 || names[0] != "gt-c-a" || names[1] != "gt-c-b" {
			t.Fatalf("ListSessions = %v", names)
		}
		if err := s.KillSession("gt-c-a"); err != nil {
			t.Fatal(err)
		}
		if ok, _ := s.HasSession("gt-c-a"); ok {
			t.Fatal("session a survived KillSession")
		}
	})
	t.Run("duplicate name", func(t *testing.T) {
		s := newImpl(t)
		dir := t.TempDir()
		if err := s.NewSession("gt-c-dup", dir); err != nil {
			t.Fatal(err)
		}
		if err := s.NewSession("gt-c-dup", dir); !errors.Is(err, tmux.ErrSessionExists) {
			t.Fatalf("second NewSession = %v, want ErrSessionExists", err)
		}
	})
	t.Run("has on missing", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSession("gt-c-other", t.TempDir()); err != nil { // ensure a server exists
			t.Fatal(err)
		}
		if ok, err := s.HasSession("gt-c-missing"); err != nil || ok {
			t.Fatalf("HasSession(missing) = %v, %v; want false, nil", ok, err)
		}
	})
	t.Run("environment round trip", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSessionWithCommandAndEnv("gt-c-env", t.TempDir(), "sleep 300", map[string]string{"GT_ROLE": "polecat"}); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetEnvironment("gt-c-env", "GT_ROLE"); err != nil || v != "polecat" {
			t.Fatalf("GetEnvironment(GT_ROLE) = %q, %v", v, err)
		}
		if err := s.SetEnvironment("gt-c-env", "GT_X", "1"); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetEnvironment("gt-c-env", "GT_X"); err != nil || v != "1" {
			t.Fatalf("GetEnvironment(GT_X) = %q, %v", v, err)
		}
	})
	t.Run("pane command", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSessionWithCommandAndEnv("gt-c-cmd", t.TempDir(), "sleep 300", nil); err != nil {
			t.Fatal(err)
		}
		if c, err := s.GetPaneCommand("gt-c-cmd"); err != nil || c != "sleep" {
			t.Fatalf("GetPaneCommand = %q, %v; want sleep", c, err)
		}
	})
	t.Run("no server", func(t *testing.T) {
		s := newImpl(t)
		if names, err := s.ListSessions(); err != nil || len(names) != 0 {
			t.Fatalf("ListSessions with no server = %v, %v; want empty, nil", names, err)
		}
		if ok, err := s.HasSession("gt-c-none"); err != nil || ok {
			t.Fatalf("HasSession with no server = %v, %v; want false, nil", ok, err)
		}
		if err := s.KillSession("gt-c-none"); err != nil {
			t.Fatalf("KillSession with no server = %v, want nil", err)
		}
	})
	t.Run("missing session", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSession("gt-c-other", t.TempDir()); err != nil { // a server with other sessions
			t.Fatal(err)
		}
		const missing = "gt-c-missing"
		if err := s.KillSession(missing); err != nil {
			t.Errorf("KillSession(missing) = %v, want nil (idempotent)", err)
		}
		if err := s.KillSessionWithProcesses(missing); err != nil {
			t.Errorf("KillSessionWithProcesses(missing) = %v, want nil", err)
		}
		if c, err := s.GetPaneCommand(missing); err == nil {
			t.Errorf("GetPaneCommand(missing) = %q, nil; want an error", c)
		}
		if _, err := s.CapturePane(missing, 10); !errors.Is(err, tmux.ErrPaneNotFound) {
			t.Errorf("CapturePane(missing) = %v, want ErrPaneNotFound", err)
		}
		if err := s.SendKeys(missing, "x"); !errors.Is(err, tmux.ErrPaneNotFound) {
			t.Errorf("SendKeys(missing) = %v, want ErrPaneNotFound", err)
		}
		if _, err := s.GetEnvironment(missing, "GT_ROLE"); err == nil {
			t.Error("GetEnvironment(missing) = nil error")
		}
		if err := s.SetEnvironment(missing, "GT_ROLE", "x"); err == nil {
			t.Error("SetEnvironment(missing) = nil error")
		}
	})
	t.Run("unset variable", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSession("gt-c-unset", t.TempDir()); err != nil {
			t.Fatal(err)
		}
		if v, err := s.GetEnvironment("gt-c-unset", "GT_NEVER_SET"); err == nil {
			t.Errorf("GetEnvironment(unset) = %q, nil; want an error", v)
		}
	})
	t.Run("create validation", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSession("bad.name", t.TempDir()); !errors.Is(err, tmux.ErrInvalidSessionName) {
			t.Errorf("NewSession(bad.name) = %v, want ErrInvalidSessionName", err)
		}
		if err := s.NewSessionWithCommandAndEnv("gt-c-baddir", filepath.Join(t.TempDir(), "absent"), "sleep 300", nil); err == nil {
			t.Error("NewSessionWithCommandAndEnv with a missing work dir = nil error")
		}
		if ok, _ := s.HasSession("gt-c-baddir"); ok {
			t.Error("a rejected create left a session behind")
		}
	})
	t.Run("pane io", func(t *testing.T) {
		s := newImpl(t)
		if err := s.NewSessionWithCommandAndEnv("gt-c-io", t.TempDir(), "sleep 300", nil); err != nil {
			t.Fatal(err)
		}
		if _, err := s.CapturePane("gt-c-io", 10); err != nil {
			t.Errorf("CapturePane(live) = %v", err)
		}
		if err := s.SendKeys("gt-c-io", "x"); err != nil {
			t.Errorf("SendKeys(live) = %v", err)
		}
	})
}
