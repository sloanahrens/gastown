package polecat

import (
	"errors"
	"sync"
	"testing"
)

// probeTmux is the Manager's own tmux client as these tests see it. It counts
// the has-session reads that reach it, so a test can see whether a read went
// to tmux or to the installed reader. It reports the session running, and its
// pane pid read fails, so the liveness check after a running answer does not
// judge the session dead.
type probeTmux struct {
	mu  sync.Mutex
	has int
}

func (p *probeTmux) HasSession(string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.has++
	return true, nil
}

func (p *probeTmux) KillSessionWithProcesses(string) error { return nil }

func (p *probeTmux) IsAgentAliveChecked(string) (bool, error) { return true, nil }

func (p *probeTmux) GetPanePID(string) (string, error) {
	return "", errors.New("pane pid read fails in this double")
}

func (p *probeTmux) hasCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.has
}

// seamReader is the reader a sling installs. It answers running as configured
// and counts the reads it answers.
type seamReader struct {
	running bool

	mu  sync.Mutex
	has int
}

func (r *seamReader) HasSession(string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.has++
	return r.running, nil
}

func (r *seamReader) hasCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.has
}

// TestManagerSessionProbesUseTheSeam: while a reader is installed, the Manager's
// has-session reads of the seat listing and the reuse go through it and never to
// its own tmux client; with none installed, or once it ends, they go to tmux.
func TestManagerSessionProbesUseTheSeam(t *testing.T) {
	t.Parallel()

	t.Run("the listing's read goes through the reader, not tmux", func(t *testing.T) {
		t.Parallel()
		mgr, _, _, _, _ := canonicalWithPolecats(t, true, "p1")
		tm := &probeTmux{}
		mgr.tmux = tm
		reader := &seamReader{running: true}
		end := mgr.ShareSessionProbes(reader)
		defer end()

		running, _ := mgr.polecatSessionState("p1")
		if !running {
			t.Errorf("polecatSessionState reported not running, want the reader's running answer")
		}
		if got := reader.hasCount(); got != 1 {
			t.Errorf("reader answered %d has-session reads, want 1", got)
		}
		if got := tm.hasCount(); got != 0 {
			t.Errorf("tmux answered %d has-session reads while the reader was installed, want 0", got)
		}
	})

	t.Run("the reuse's kill check reads through the reader, not tmux", func(t *testing.T) {
		t.Parallel()
		mgr, _, _, _, _ := canonicalWithPolecats(t, true, "p1")
		tm := &probeTmux{}
		mgr.tmux = tm
		reader := &seamReader{running: false}
		end := mgr.ShareSessionProbes(reader)
		defer end()

		if err := mgr.killExistingPolecatSession("p1", "reuse"); err != nil {
			t.Fatalf("killExistingPolecatSession: %v", err)
		}
		if got := reader.hasCount(); got != 1 {
			t.Errorf("reader answered %d has-session reads, want 1", got)
		}
		if got := tm.hasCount(); got != 0 {
			t.Errorf("tmux answered %d has-session reads while the reader was installed, want 0", got)
		}
	})

	t.Run("with no reader installed the read goes to tmux", func(t *testing.T) {
		t.Parallel()
		mgr, _, _, _, _ := canonicalWithPolecats(t, true, "p1")
		tm := &probeTmux{}
		mgr.tmux = tm

		mgr.polecatSessionState("p1")
		if got := tm.hasCount(); got != 1 {
			t.Errorf("tmux answered %d has-session reads with no reader, want 1", got)
		}
	})

	t.Run("once the end func runs the read goes back to tmux", func(t *testing.T) {
		t.Parallel()
		mgr, _, _, _, _ := canonicalWithPolecats(t, true, "p1")
		tm := &probeTmux{}
		mgr.tmux = tm
		reader := &seamReader{running: true}
		end := mgr.ShareSessionProbes(reader)
		end()

		mgr.polecatSessionState("p1")
		if got := reader.hasCount(); got != 0 {
			t.Errorf("reader answered %d reads after its end func ran, want 0", got)
		}
		if got := tm.hasCount(); got != 1 {
			t.Errorf("tmux answered %d has-session reads after the end func, want 1", got)
		}
	})
}
