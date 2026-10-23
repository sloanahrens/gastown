package daemon

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/steveyegge/gastown/internal/session"
)

// fakeSandboxReads answers a sandboxed seat's panes and processes from canned
// tmux output and records which sessions it was asked about. panesErr and
// tableErr fail the two reads separately.
type fakeSandboxReads struct {
	panes, table       string
	panesErr, tableErr error
	asked              []string
}

func (f *fakeSandboxReads) SessionPanes(s string) (string, error) {
	f.asked = append(f.asked, "panes:"+s)
	return f.panes, f.panesErr
}

func (f *fakeSandboxReads) ProcessTable(s string) (string, error) {
	f.asked = append(f.asked, "ps:"+s)
	return f.table, f.tableErr
}

// A sandboxed seat's gate suite runs where the host's list-panes and ps cannot
// see it, so its panes and processes are read through the sandbox (gt-akky5).
func TestOverrunGateReaderReadsASandboxedSeatInsideItsSandbox(t *testing.T) {
	t.Parallel()
	seat := overrunSeat{Rig: "gastown", Polecat: "capable", Bead: "gt-gate1"}
	sessionName := session.PolecatSessionName("gt", "capable")

	newReader := func(t *testing.T, sandboxed bool, f *fakeSandboxReads) *overrunGateReader {
		t.Helper()
		d, _ := newOverrunDaemon(t, false)
		if sandboxed {
			rec := session.SandboxRecord{Session: sessionName, Rig: "gastown", Polecat: "capable"}
			if err := session.WriteSandboxRecord(d.config.TownRoot, rec); err != nil {
				t.Fatal(err)
			}
		}
		r := newOverrunGateReader(d)
		r.sandboxReads = f
		// The host reads are wrong for a sandboxed seat: they would say it is idle.
		r.panes = func(string) ([]int, error) { return []int{900}, nil }
		r.run = func(*exec.Cmd) ([]byte, []byte, error) { return []byte("  900     1 claude\n"), nil, nil }
		return r
	}

	t.Run("a gate suite in the sandbox's table exempts the seat", func(t *testing.T) {
		t.Parallel()
		f := &fakeSandboxReads{panes: sessionName + "\t143\n", table: "  143     1 claude\n  150   143 make gate\n"}
		got, err := newReader(t, true, f).running(seat)
		if err != nil || !got {
			t.Fatalf("running = %v, %v; want true, nil while make gate runs in the sandbox", got, err)
		}
		if len(f.asked) != 2 || f.asked[0] != "panes:"+sessionName || f.asked[1] != "ps:"+sessionName {
			t.Errorf("sandbox reads asked = %v, want panes then ps for %s", f.asked, sessionName)
		}
	})

	t.Run("an idle sandbox pane is not a gate", func(t *testing.T) {
		t.Parallel()
		f := &fakeSandboxReads{panes: sessionName + "\t143\n", table: "  143     1 claude\n  150   143 git status\n"}
		if got, err := newReader(t, true, f).running(seat); err != nil || got {
			t.Errorf("running = %v, %v; want false, nil", got, err)
		}
	})

	t.Run("a failing sandbox pane read is an error, never the host's answer", func(t *testing.T) {
		t.Parallel()
		f := &fakeSandboxReads{panesErr: errors.New("sandbox exec failed")}
		if _, err := newReader(t, true, f).running(seat); err == nil {
			t.Error("running = nil error; the guard must count all of the seat's time, not read the host")
		}
	})

	t.Run("a failing sandbox process table is an error, never the host's answer", func(t *testing.T) {
		t.Parallel()
		f := &fakeSandboxReads{panes: sessionName + "\t143\n", tableErr: errors.New("sandbox ps failed")}
		if _, err := newReader(t, true, f).running(seat); err == nil {
			t.Error("running = nil error; the guard must count all of the seat's time, not read the host")
		}
		if len(f.asked) != 2 || f.asked[1] != "ps:"+sessionName {
			t.Errorf("sandbox reads asked = %v, want the pane read then the process table read", f.asked)
		}
	})

	t.Run("the registry is read once per session per cycle", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		rec := session.SandboxRecord{Session: sessionName, Rig: "gastown", Polecat: "capable"}
		if err := session.WriteSandboxRecord(d.config.TownRoot, rec); err != nil {
			t.Fatal(err)
		}
		r := newOverrunGateReader(d)
		if on, err := r.sandboxed(sessionName); err != nil || !on {
			t.Fatalf("first sandboxed = %v, %v; want true, nil", on, err)
		}
		// The registry is unreadable from here on; the cycle's first answer still holds.
		dir := session.SandboxRegistryDir(d.config.TownRoot)
		if err := os.RemoveAll(dir); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		if on, err := r.sandboxed(sessionName); err != nil || !on {
			t.Errorf("second sandboxed = %v, %v; want the cycle's cached true, nil", on, err)
		}
	})

	t.Run("a host seat is still read from the host", func(t *testing.T) {
		t.Parallel()
		f := &fakeSandboxReads{}
		if got, err := newReader(t, false, f).running(seat); err != nil || got {
			t.Errorf("running = %v, %v; want false, nil from the host's idle table", got, err)
		}
		if len(f.asked) != 0 {
			t.Errorf("a host seat was read through the sandbox reads: %v", f.asked)
		}
	})

	t.Run("an unreadable registry is an error", func(t *testing.T) {
		t.Parallel()
		d, _ := newOverrunDaemon(t, false)
		dir := session.SandboxRegistryDir(d.config.TownRoot)
		if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dir, []byte("not a directory"), 0o644); err != nil {
			t.Fatal(err)
		}
		r := newOverrunGateReader(d)
		r.sandboxReads = &fakeSandboxReads{}
		if _, err := r.running(seat); err == nil {
			t.Error("running = nil error with an unreadable sandbox registry")
		}
	})
}
