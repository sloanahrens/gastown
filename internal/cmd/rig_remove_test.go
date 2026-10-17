package cmd

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeTmuxSessions installs a tmux shim on PATH that lists the given sessions
// and logs every invocation, so a test can see which tmux commands a verb ran
// without a tmux server. It returns the log's path.
func fakeTmuxSessions(t *testing.T, sessions ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tmux shim is POSIX-only; tmux itself does not run on Windows")
	}

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "tmux.log")
	script := "#!/bin/sh\n" +
		`printf '%s\n' "$*" >> "` + logPath + "\"\n" +
		`for a in "$@"; do if [ "$a" = list-sessions ]; then printf '%s\n' ` +
		"'" + strings.Join(sessions, "' '") + "'; fi; done\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(binDir, "tmux"), []byte(script), 0o755); err != nil {
		t.Fatalf("write fake tmux: %v", err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// A rig name the registry does not know has no sessions of its own, but its
// prefix falls back to the default "gt", which is the gastown rig's. gt rig
// remove gastwon --force used to stop every gt-* session before it found the
// rig missing (gt-bfo4z).
func TestRigRemoveUnknownRigTouchesNoSessions(t *testing.T) {
	for _, tc := range []struct {
		name  string
		force bool
	}{
		{"force", true},
		{"no force", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			town := t.TempDir()
			for path, body := range map[string]string{
				"mayor/town.json": `{"type":"town","version":2,"name":"test"}`,
				"mayor/rigs.json": `{"version":1,"rigs":{}}`,
			} {
				full := filepath.Join(town, path)
				if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			t.Chdir(town)

			logPath := fakeTmuxSessions(t, "gt-witness", "gt-crew-sloan", "gt-furiosa")
			prevForce := rigRemoveForce
			rigRemoveForce = tc.force
			t.Cleanup(func() { rigRemoveForce = prevForce })

			err := runRigRemove(rigRemoveCmd, []string{"gastwon"})
			if err == nil {
				t.Fatal("runRigRemove for an unregistered rig = nil, want the not-found error")
			}
			if !strings.Contains(err.Error(), "gastwon") {
				t.Errorf("error = %v, want it to name the rig", err)
			}
			if strings.Contains(err.Error(), "running sessions") {
				t.Errorf("error = %v: refused over sessions of a rig that is not registered", err)
			}

			log, _ := os.ReadFile(logPath)
			if strings.Contains(string(log), "kill") {
				t.Errorf("a session was stopped for an unregistered rig; tmux saw:\n%s", log)
			}
		})
	}
}

// The registered rig keeps the old behavior: --force stops its sessions.
func TestRigRemoveRegisteredRigStopsSessionsWithForce(t *testing.T) {
	town := t.TempDir()
	for path, body := range map[string]string{
		"mayor/town.json": `{"type":"town","version":2,"name":"test"}`,
		"mayor/rigs.json": `{"version":1,"rigs":{"gastown":{"git_url":"file:///nonexistent"}}}`,
	} {
		full := filepath.Join(town, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(town)

	logPath := fakeTmuxSessions(t, "gt-witness")
	prevForce := rigRemoveForce
	rigRemoveForce = false
	t.Cleanup(func() { rigRemoveForce = prevForce })

	err := runRigRemove(rigRemoveCmd, []string{"gastown"})
	if err == nil || !strings.Contains(err.Error(), "running sessions") {
		t.Fatalf("runRigRemove without --force on a rig with sessions = %v, want the running-sessions refusal", err)
	}
	log, _ := os.ReadFile(logPath)
	if strings.Contains(string(log), "kill") {
		t.Errorf("refusal stopped a session; tmux saw:\n%s", log)
	}
}
