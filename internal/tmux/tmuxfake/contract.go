// Package tmuxfake is an in-memory tmux server for unit tests. Its behavior
// is pinned to real tmux by RunSessionsContract, which runs against the fake in
// the unit tier and against a real tmux server in the integration tier.
package tmuxfake

import (
	"errors"
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
}
