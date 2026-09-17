package cmd

import (
	"encoding/json"
	"io"
	"os"
	"testing"
)

// TestPolecatContext tests the polecatContext detection logic.
func TestPolecatContext(t *testing.T) {
	tests := []struct {
		name         string
		gtPolecat    string
		gtRig        string
		cwd          string
		wantRig      string
		wantName     string
		wantOwnSet   bool
	}{
		{
			name:       "GT_POLECAT env set",
			gtPolecat:  "jasper",
			gtRig:      "gastown",
			wantRig:    "gastown",
			wantName:   "jasper",
			wantOwnSet: true,
		},
		{
			name:       "neither env set",
			wantRig:    "",
			wantName:   "",
			wantOwnSet: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GT_POLECAT", tt.gtPolecat)
			t.Setenv("GT_RIG", tt.gtRig)
			rig, name, own := polecatContext()
			if rig != tt.wantRig || name != tt.wantName {
				t.Errorf("polecatContext() = (%q, %q, %q), want (%q, %q, %q)",
					rig, name, own, tt.wantRig, tt.wantName, "")
			}
			if tt.wantOwnSet && own == "" {
				t.Errorf("expected own dir to be set, got empty")
			}
		})
	}
}

// TestIsPathAllowed tests the path allowance logic.
func TestIsPathAllowed(t *testing.T) {
	own := "/Users/sloan/gt/gastown/polecats/jasper"

	tests := []struct {
		name  string
		path  string
		own   string
		want  bool
	}{
		{"own dir file", "/Users/sloan/gt/gastown/polecats/jasper/foo.go", own, true},
		{"own dir nested", "/Users/sloan/gt/gastown/polecats/jasper/cmd/gt/main.go", own, true},
		{"sibling polecat", "/Users/sloan/gt/gastown/polecats/coral/gastown/foo.go", own, false},
		{"other rig", "/Users/sloan/gt/beads/polecats/jasper/foo.go", own, false},
		{"tmp", "/tmp/test.txt", own, true},
		{"private tmp", "/private/tmp/test.txt", own, true},
		{"scratchpad", "/private/var/folders/abc123/C/com.apple.Safari/", own, true},
		{"claude projects", "/Users/sloan/.claude/projects/test/file.md", own, true},
		{"root gt", "/Users/sloan/gt/docs/README.md", own, false},
		{"mayor dir", "/Users/sloan/gt/mayor/config.yaml", own, false},
		{"deacon dir", "/Users/sloan/gt/deacon/dogs/fido/.dog.json", own, false},
		{"settings dir", "/Users/sloan/gt/settings/roles.json", own, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPathAllowed(tt.path, tt.own); got != tt.want {
				t.Errorf("isPathAllowed(%q, %q) = %v, want %v", tt.path, tt.own, got, tt.want)
			}
		})
	}
}

// TestIsReadOnlyCommand tests the read-only command detection.
func TestIsReadOnlyCommand(t *testing.T) {
	tests := []struct {
		name   string
		cmd    string
		want   bool
	}{
		{"grep", "grep -r 'foo' .", true},
		{"cat", "cat file.txt", true},
		{"ls", "ls -la", true},
		{"head", "head -n 10 file.txt", true},
		{"tail", "tail -f /var/log/syslog", true},
		{"jq", "jq '.foo' file.json", true},
		{"python", "python3 -c 'print(1)'", true},
		{"gh", "gh pr list", true},
		{"bd", "bd show gt-123", true},
		{"gt", "gt prime", true},
		{"git", "git status", true},
		{"find", "find . -name '*.go'", true},
		{"write", "echo hello > file.txt", false},
		{"cp", "cp a b", false},
		{"mv", "mv a b", false},
		{"rm", "rm file.txt", false},
		{"open", "open -a Finder .", false},
		{"code", "code .", false},
		{"vim", "vim file.txt", false},
		{"nano", "nano file.txt", false},
		{"emacs", "emacs file.txt", false},
		{"sudo grep", "sudo grep root /etc/passwd", true},
		{"sudo code", "sudo code .", false},
		{"empty", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isReadOnlyCommand(tt.cmd); got != tt.want {
				t.Errorf("isReadOnlyCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// TestIsPolecatCommand tests polecat command detection.
func TestIsPolecatCommand(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{"gt command", "gt prime", true},
		{"bd command", "bd show gt-123", true},
		{"kill command", "kill -9 1234", true},
		{"echo", "echo hello", false},
		{"ls", "ls -la", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPolecatCommand(tt.cmd); got != tt.want {
				t.Errorf("isPolecatCommand(%q) = %v, want %v", tt.cmd, got, tt.want)
			}
		})
	}
}

// withPolecatStdin replaces os.Stdin for the duration of fn.
func withPolecatStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	origStdin := os.Stdin
	os.Stdin = r
	defer func() { os.Stdin = origStdin }()

	done := make(chan struct{})
	go func() {
		_, _ = io.WriteString(w, content)
		w.Close()
		close(done)
	}()
	fn()
	<-done
}

func TestRunTapGuardPolecatPaths_EditOwnFileAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "jasper")
	t.Setenv("GT_RIG", "gastown")

	hookInput := map[string]interface{}{
		"tool_name": "Edit",
		"tool_input": map[string]interface{}{
			"file_path": "/Users/sloan/gt/gastown/polecats/jasper/cmd/gt/main.go",
		},
	}
	payload, _ := json.Marshal(hookInput)

	var err error
	withPolecatStdin(t, string(payload), func() {
		err = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
	})
	if err != nil {
		t.Errorf("expected Edit of own file to be allowed, got error: %v", err)
	}
}

func TestRunTapGuardPolecatPaths_EditSiblingBlocked(t *testing.T) {
	t.Setenv("GT_POLECAT", "jasper")
	t.Setenv("GT_RIG", "gastown")

	hookInput := map[string]interface{}{
		"tool_name": "Edit",
		"tool_input": map[string]interface{}{
			"file_path": "/Users/sloan/gt/gastown/polecats/coral/gastown/cmd/gt/main.go",
		},
	}
	payload, _ := json.Marshal(hookInput)

	var err error
	withPolecatStdin(t, string(payload), func() {
		err = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
	})
	if err == nil {
		t.Error("expected Edit of sibling polecat file to be blocked, got nil")
	}
}

func TestRunTapGuardPolecatPaths_WriteTmpAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "jasper")
	t.Setenv("GT_RIG", "gastown")

	hookInput := map[string]interface{}{
		"tool_name": "Write",
		"tool_input": map[string]interface{}{
			"file_path": "/tmp/test-file.txt",
		},
	}
	payload, _ := json.Marshal(hookInput)

	var err error
	withPolecatStdin(t, string(payload), func() {
		err = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
	})
	if err != nil {
		t.Errorf("expected Write of /tmp file to be allowed, got error: %v", err)
	}
}

func TestRunTapGuardPolecatPaths_BashReadOnlyAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "jasper")
	t.Setenv("GT_RIG", "gastown")

	hookInput := map[string]interface{}{
		"tool_name": "Bash",
		"tool_input": map[string]interface{}{
			"command": "grep -r 'hello' /Users/sloan/gt/gastown/polecats/coral/gastown/",
		},
	}
	payload, _ := json.Marshal(hookInput)

	var err error
	withPolecatStdin(t, string(payload), func() {
		err = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
	})
	if err != nil {
		t.Errorf("expected grep (read-only) to be allowed, got error: %v", err)
	}
}

func TestRunTapGuardPolecatPaths_BashPolecatCommandAllowed(t *testing.T) {
	t.Setenv("GT_POLECAT", "jasper")
	t.Setenv("GT_RIG", "gastown")

	hookInput := map[string]interface{}{
		"tool_name": "Bash",
		"tool_input": map[string]interface{}{
			"command": "bd show gt-123",
		},
	}
	payload, _ := json.Marshal(hookInput)

	var err error
	withPolecatStdin(t, string(payload), func() {
		err = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
	})
	if err != nil {
		t.Errorf("expected bd command to be allowed, got error: %v", err)
	}
}

func TestRunTapGuardPolecatPaths_BashNotPolecatAllowed(t *testing.T) {
	// Not running as a polecat — should allow everything.
	t.Setenv("GT_POLECAT", "")
	t.Setenv("GT_RIG", "")

	hookInput := map[string]interface{}{
		"tool_name": "Bash",
		"tool_input": map[string]interface{}{
			"command": "echo hello",
		},
	}
	payload, _ := json.Marshal(hookInput)

	var err error
	withPolecatStdin(t, string(payload), func() {
		err = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
	})
	if err != nil {
		t.Errorf("expected non-polecat context to allow everything, got error: %v", err)
	}
}

// TestLiveBlock is the LIVE block test: actually fire the guard against real
// hook payloads to prove it blocks the right things and allows the right ones.
// This is a harmless test — it only reads stdin and exits; no filesystem
// mutations. The point is end-to-end validation of the guard's decision logic.
func TestLiveBlock(t *testing.T) {
	t.Setenv("GT_POLECAT", "jasper")
	t.Setenv("GT_RIG", "gastown")

	type liveCase struct {
		name    string
		payload map[string]interface{}
		wantErr bool
	}

	cases := []liveCase{
		{
			name: "ALLOW: Edit own worktree file",
			payload: map[string]interface{}{
				"tool_name": "Edit",
				"tool_input": map[string]interface{}{
					"file_path": "/Users/sloan/gt/gastown/polecats/jasper/internal/cmd/tap.go",
				},
			},
			wantErr: false,
		},
		{
			name: "BLOCK: Edit sibling polecat",
			payload: map[string]interface{}{
				"tool_name": "Edit",
				"tool_input": map[string]interface{}{
					"file_path": "/Users/sloan/gt/gastown/polecats/orange/gastown/cmd/gt/main.go",
				},
			},
			wantErr: true,
		},
		{
			name: "ALLOW: Write to /tmp",
			payload: map[string]interface{}{
				"tool_name": "Write",
				"tool_input": map[string]interface{}{
					"file_path": "/tmp/guard-test.txt",
				},
			},
			wantErr: false,
		},
		{
			name: "BLOCK: Write to sibling polecat",
			payload: map[string]interface{}{
				"tool_name": "Write",
				"tool_input": map[string]interface{}{
					"file_path": "/Users/sloan/gt/gastown/polecats/orange/gastown/CLAUDE.md",
				},
			},
			wantErr: true,
		},
		{
			name: "ALLOW: NotebookEdit own worktree",
			payload: map[string]interface{}{
				"tool_name": "NotebookEdit",
				"tool_input": map[string]interface{}{
					"notebook_path": "/Users/sloan/gt/gastown/polecats/jasper/notebook.ipynb",
				},
			},
			wantErr: false,
		},
		{
			name: "ALLOW: Bash read-only grep",
			payload: map[string]interface{}{
				"tool_name": "Bash",
				"tool_input": map[string]interface{}{
					"command": "grep -r 'test' /Users/sloan/gt/gastown/polecats/orange/gastown/",
				},
			},
			wantErr: false,
		},
		{
			name: "ALLOW: Bash polecat command",
			payload: map[string]interface{}{
				"tool_name": "Bash",
				"tool_input": map[string]interface{}{
					"command": "bd show gt-123",
				},
			},
			wantErr: false,
		},
		{
			name: "ALLOW: Bash cd own worktree",
			payload: map[string]interface{}{
				"tool_name": "Bash",
				"tool_input": map[string]interface{}{
					"command": "cd /Users/sloan/gt/gastown/polecats/jasper/gastown && git status",
				},
			},
			wantErr: false,
		},
		{
			name: "BLOCK: Bash write to sibling",
			payload: map[string]interface{}{
				"tool_name": "Bash",
				"tool_input": map[string]interface{}{
					"command": "cp foo.go /Users/sloan/gt/gastown/polecats/orange/gastown/cmd/",
				},
			},
			wantErr: true,
		},
		{
			name: "BLOCK: Bash touch mayor dir",
			payload: map[string]interface{}{
				"tool_name": "Bash",
				"tool_input": map[string]interface{}{
					"command": "cp config.yaml /Users/sloan/gt/mayor/",
				},
			},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload, marshalErr := json.Marshal(tc.payload)
			if marshalErr != nil {
				t.Fatalf("json.Marshal: %v", marshalErr)
			}

			var guardErr error
			withPolecatStdin(t, string(payload), func() {
				guardErr = runTapGuardPolecatPaths(tapGuardPolecatPathsCmd, nil)
			})

			gotErr := guardErr != nil
			if gotErr != tc.wantErr {
				t.Errorf("wantErr=%v, gotErr=%v (payload: %s)", tc.wantErr, gotErr, string(payload))
			}
		})
	}
}
