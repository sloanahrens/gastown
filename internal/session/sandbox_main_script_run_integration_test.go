//go:build integration

package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// These tests run sandboxMainScript under sh with a fake tmux and a fake sleep
// on PATH, both in t.TempDir(). The fake sleep records each call and whether
// the marker existed at that moment, then returns at once, so no real time
// passes. The unit tier checks the script's text (sandbox_main_script_test.go).

// mainScriptFakes is what one run of the script sees.
type mainScriptFakes struct {
	newSessionFails bool   // the holder session cannot start
	hasSession      bool   // the agent's session exists
	paneDead        bool   // the agent's pane reads dead
	tmuxUnreachable bool   // tmux answers no list call
	waitSeconds     string // $2: how long the script waits for the session
	lostPolls       string // $5: the unreachable-poll limit; "" takes the default
	home            string // $4: where the marker goes
}

// runMainScript runs sandboxMainScript under /bin/sh with the fakes on PATH and
// returns its exit code, its sleep log, and whether the marker exists under home.
// The PATH holds only the fake bin and the system directories for grep and touch.
func runMainScript(t *testing.T, f mainScriptFakes, hold string) (exit int, sleeps []string, marked bool) {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(root, "sleeps.log")
	marker := filepath.Join(f.home, sandboxEndedMarker)
	writeFakeTool(t, filepath.Join(bin, "tmux"), fakeTmux(f))
	writeFakeTool(t, filepath.Join(bin, "sleep"), fakeSleep(logPath, marker))

	args := []string{"-c", sandboxMainScript, "sh", fakeMainSession, f.waitSeconds, "2", f.home, f.lostPolls, hold}
	//testpolicy:allow no-subprocess — the script is the unit under test: it runs under sh, and the fake tmux and sleep it calls are the only seam
	cmd := exec.Command("/bin/sh", args...)
	cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		exit = 0
	case errors.As(err, &ee):
		exit = ee.ExitCode()
	default:
		t.Fatalf("running the script: %v: %s", err, out)
	}

	if data, rerr := os.ReadFile(logPath); rerr == nil {
		sleeps = strings.Split(strings.TrimSpace(string(data)), "\n")
	} else if !errors.Is(rerr, os.ErrNotExist) {
		t.Fatal(rerr)
	}
	_, serr := os.Stat(marker)
	marked = serr == nil
	return exit, sleeps, marked
}

func writeFakeTool(t *testing.T, path, body string) {
	t.Helper()
	//testpolicy:allow no-exec-files — the fake tmux and sleep must be executable for the script to run them; both live in the test's t.TempDir()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil { //nolint:gosec // G306: a fake tool the test runs
		t.Fatal(err)
	}
}

// fakeTmux answers the script's calls from f: the holder starts or fails, the
// agent's session exists or not, its pane reads dead or alive, and tmux may be
// unreachable for every list call.
func fakeTmux(f mainScriptFakes) string {
	newSession := "exit 0"
	if f.newSessionFails {
		newSession = "exit 1"
	}
	has := "exit 1"
	if f.hasSession {
		has = "exit 0"
	}
	pane := "echo 0"
	list := "exit 0"
	switch {
	case f.tmuxUnreachable:
		pane, list = "exit 1", "exit 1"
	case f.paneDead:
		pane = "echo 1"
	}
	//testpolicy:allow no-exec-files — the body of the fake tmux that writeFakeTool puts in the test's t.TempDir() for the script to run
	return "#!/bin/sh\n" +
		"case \"$*\" in\n" +
		"  *new-session*) " + newSession + " ;;\n" +
		"  *has-session*) " + has + " ;;\n" +
		"  *list-panes*) " + pane + " ;;\n" +
		"  *list-sessions*) " + list + " ;;\n" +
		"esac\n"
}

// fakeSleep logs its argument and whether the marker exists when it is called.
func fakeSleep(logPath, marker string) string {
	//testpolicy:allow no-exec-files — the body of the fake sleep that writeFakeTool puts in the test's t.TempDir() for the script to run
	return "#!/bin/sh\n" +
		"m=no; [ -e '" + marker + "' ] && m=yes\n" +
		"echo \"$1 marker=$m\" >> '" + logPath + "'\n"
}

// The agent's session ends with a dead pane: the script writes the marker,
// holds for the bound, and exits 0.
func TestIntegrationSandboxMainScriptRunHoldsAfterTheAgentEnds(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	exit, sleeps, marked := runMainScript(t, mainScriptFakes{hasSession: true, paneDead: true, waitSeconds: "5", home: home}, "1800")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 after the agent's session ended", exit)
	}
	if !marked {
		t.Fatalf("no %s under %s after the agent's session ended", sandboxEndedMarker, home)
	}
	// The poll loop sleeps for the poll interval between polls; the last sleep is
	// the hold, taken with the marker already written.
	if last := sleeps[len(sleeps)-1]; last != "1800 marker=yes" {
		t.Fatalf("last sleep = %q, want the hold %q with the marker already written; sleeps %q", last, "1800 marker=yes", sleeps)
	}
	holds := 0
	for _, s := range sleeps {
		if strings.HasPrefix(s, "1800 ") {
			holds++
		}
	}
	if holds != 1 {
		t.Fatalf("the script waited the hold %d times, want once; sleeps %q", holds, sleeps)
	}
}

// tmux stays unreachable until the unreachable-poll limit: the poll loop ends
// without the pane reading dead, so the script still writes the marker and
// holds. The fake sleep logs the marker state at each poll before the hold.
func TestIntegrationSandboxMainScriptRunHoldsWhenTmuxStaysUnreachable(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	exit, sleeps, marked := runMainScript(t, mainScriptFakes{hasSession: true, tmuxUnreachable: true, waitSeconds: "5", lostPolls: "3", home: home}, "1800")
	if exit != 0 {
		t.Fatalf("exit = %d, want 0 after tmux stayed unreachable", exit)
	}
	if !marked {
		t.Fatalf("no %s under %s after the poll loop ended unreachable", sandboxEndedMarker, home)
	}
	if last := sleeps[len(sleeps)-1]; last != "1800 marker=yes" {
		t.Fatalf("last sleep = %q, want the hold %q; sleeps %q", last, "1800 marker=yes", sleeps)
	}
}

// Each abnormal exit ends without the marker and without the hold: a holder that
// never started, a session that never appeared, and a marker the script cannot
// write. Each returns exit 1 and holds nothing.
func TestIntegrationSandboxMainScriptRunAbnormalExitsDoNotHold(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		f    mainScriptFakes
		// missingHome points the marker at a directory that does not exist.
		missingHome bool
	}{
		{
			name: "the holder session fails to start",
			f:    mainScriptFakes{newSessionFails: true, hasSession: true, paneDead: true, waitSeconds: "5"},
		},
		{
			name: "the agent's session never appears",
			f:    mainScriptFakes{hasSession: false, waitSeconds: "2"},
		},
		{
			name:        "the marker directory does not exist",
			f:           mainScriptFakes{hasSession: true, paneDead: true, waitSeconds: "5"},
			missingHome: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			home := t.TempDir()
			if tt.missingHome {
				home = filepath.Join(home, "missing")
			}
			tt.f.home = home
			exit, sleeps, marked := runMainScript(t, tt.f, "1800")
			if exit != 1 {
				t.Fatalf("exit = %d, want 1", exit)
			}
			if marked {
				t.Fatalf("the abnormal exit wrote %s", sandboxEndedMarker)
			}
			for _, s := range sleeps {
				if strings.HasPrefix(s, "1800 ") {
					t.Fatalf("the abnormal exit waited the hold: sleeps %q", sleeps)
				}
			}
		})
	}
}
