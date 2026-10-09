package daemon

import (
	"os/exec"
	"testing"
)

// runCmd and combinedOutput are where the daemon's own commands are bounded:
// the process group is what a context deadline reaches the whole tree
// through, and the WaitDelay is what bounds the wait on the output pipes of
// a child that outlives the command that started it (gt-7uyfc).
func TestRunCmdBoundsTheCommandsItRuns(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		name string
		run  func(*Daemon, *exec.Cmd) error
	}{
		{"runCmd", func(d *Daemon, cmd *exec.Cmd) error {
			_, _, err := d.runCmd(cmd)
			return err
		}},
		{"combinedOutput", func(d *Daemon, cmd *exec.Cmd) error {
			_, err := d.combinedOutput(cmd)
			return err
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var seen *exec.Cmd
			d := &Daemon{execCmd: func(cmd *exec.Cmd) ([]byte, []byte, error) {
				seen = cmd
				return nil, nil, nil
			}}
			// The seam answers this call, so nothing starts it: what the seam
			// is handed — and so what a real command would be configured with
			// — is the whole subject here. Driver-level coverage of a real
			// command lives in the integration tier.
			cmd := &exec.Cmd{Path: "/bin/true", Args: []string{"true"}}
			if err := tt.run(d, cmd); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			if seen != cmd {
				t.Fatalf("%s ran a different command than the caller built", tt.name)
			}
			if seen.SysProcAttr == nil || !seen.SysProcAttr.Setpgid {
				t.Errorf("%s ran the command in the daemon's own process group; a deadline would leave its children behind", tt.name)
			}
			if seen.Cancel == nil {
				t.Errorf("%s ran the command with exec's default cancel, which reaches only the process it started", tt.name)
			}
			if seen.WaitDelay != daemonCmdWaitDelay || daemonCmdWaitDelay <= 0 {
				t.Errorf("%s WaitDelay = %s, want %s", tt.name, seen.WaitDelay, daemonCmdWaitDelay)
			}
		})
	}
}
