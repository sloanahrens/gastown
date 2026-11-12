//go:build integration

package gtwrap

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestIntegrationKubernetesTargetReachesTheDaemonPod drives the kubernetes target
// through the real kubectl against the control cluster. It reads a live cluster,
// so it runs only when GT_TEST_K8S=1 and skips otherwise.
func TestIntegrationKubernetesTargetReachesTheDaemonPod(t *testing.T) {
	if os.Getenv("GT_TEST_K8S") != "1" {
		t.Skip("GT_TEST_K8S is not 1, so the control cluster is not reached")
	}
	var note bytes.Buffer
	k := Kubectl{
		Town:       kubeTown,
		Context:    kubeContext,
		Namespace:  kubeNamespace,
		Deployment: kubeDeploy,
		Container:  kubeContainer,
		Tool:       "gt",
		Shell:      OSShell{},
		LookPath:   exec.LookPath,
		Note:       &note,
	}
	diag, err := k.Doctor()
	if err != nil {
		t.Fatalf("Doctor = %v", err)
	}
	if diag.Rung != RungExec {
		t.Fatalf("the doctor reports rung %s, want exec: %s", diag.Rung, diag.Reason)
	}
	// A directory the pod lacks runs in the nearest existing ancestor, the town
	// root, and the tool is told the caller's Mac directory there.
	missing := filepath.Join(kubeTown, "gt-wrap-integration-missing")
	exit, err := k.Launch(Call{Argv: []string{"sh", "-c", `test "$GT_CALLER_CWD" = "$1"`, "sh", missing}, Dir: missing})
	if err != nil || exit != 0 {
		t.Fatalf("a call from a missing directory = %d, %v; want 0, nil with GT_CALLER_CWD naming it", exit, err)
	}
	exit, err = k.Launch(Call{Argv: []string{"gt", "status", "--line"}, Dir: kubeTown})
	if err != nil || exit != 0 {
		t.Fatalf("gt status --line through the kubernetes target = %d, %v; want 0, nil", exit, err)
	}
}

// TestPodWalkUpScriptRun runs podWalkUpScript under the real sh, in a town root the
// test owns, so each refusal and the tool's own status come from the shell itself.
// The unit tier's fake pod stands in for the tool with a no-op, so it cannot give
// the tool an exit status.
func TestPodWalkUpScriptRun(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Fatalf("sh is not on PATH: %v", err)
	}
	// A fixed PATH keeps the run hermetic: dirname and the rest come from the system
	// directories, not from the developer's shell.
	const hostPath = "/usr/bin:/bin"
	root := t.TempDir()
	absent := filepath.Join(t.TempDir(), "absent")
	gastown := filepath.Join(root, "gastown")
	locked := filepath.Join(root, "locked")
	for _, dir := range []string{gastown, locked} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// A directory the shell cannot enter. The cleanup restores it before the
	// temporary directory is removed, since removal lists the directory.
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", locked, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(locked, 0o755); err != nil {
			t.Errorf("restore %s: %v", locked, err)
		}
	})

	// report prints the directory the tool started in and its GT_CALLER_CWD, so a
	// row reads where the tool ran and what it was told.
	report := []string{sh, "-c", `pwd; echo "${GT_CALLER_CWD-unset}"`}
	tests := []struct {
		name         string
		root         string
		target       string
		caller       string
		path         string
		tool         []string
		needsNonRoot bool
		wantExit     int
		wantOut      string
		wantErr      string
		oneLine      bool
	}{
		{
			name: "a missing directory walks to its nearest ancestor", root: root,
			target: gastown + "/crew/x", caller: "/Users/sloan/gt/gastown/crew/x", path: hostPath, tool: report,
			wantExit: 0, wantOut: gastown + "\n/Users/sloan/gt/gastown/crew/x\n",
		},
		{
			name: "a directory that maps to itself keeps the environment", root: root,
			target: gastown, caller: gastown, path: hostPath, tool: report,
			wantExit: 0, wantOut: gastown + "\nunset\n",
		},
		{
			name: "a tool exit status passes through", root: root,
			target: gastown, caller: gastown, path: hostPath, tool: []string{sh, "-c", "exit 3"},
			wantExit: 3,
		},
		{
			name: "a target outside the root is refused", root: root,
			target: filepath.Dir(root) + "/elsewhere", caller: "/Users/sloan/Documents/scratch", path: hostPath, tool: report,
			wantExit: 125, wantErr: "is not under the pod town root", oneLine: true,
		},
		{
			name: "a relative target is refused", root: root,
			target: "gastown/crew/x", caller: "/Users/sloan/Documents/scratch", path: hostPath, tool: report,
			wantExit: 125, wantErr: "is not under the pod town root", oneLine: true,
		},
		{
			name: "an empty target is refused", root: root,
			target: "", caller: "/Users/sloan/Documents/scratch", path: hostPath, tool: report,
			wantExit: 125, wantErr: "is not under the pod town root", oneLine: true,
		},
		{
			name: "a target with a .. step is refused", root: root,
			target: gastown + "/../gastown/crew/x", caller: "/Users/sloan/Documents/scratch", path: hostPath, tool: report,
			wantExit: 125, wantErr: "has a .. step", oneLine: true,
		},
		{
			name: "an absent town root is refused and names it", root: absent,
			target: absent + "/gastown/crew/x", caller: "/Users/sloan/Documents/scratch", path: hostPath, tool: report,
			wantExit: 125, wantErr: "the pod town root " + absent + " is not a directory", oneLine: true,
		},
		{
			name: "a missing dirname ends the walk instead of looping", root: root,
			target: gastown + "/crew/x", caller: "/Users/sloan/Documents/scratch", path: t.TempDir(), tool: report,
			wantExit: 125, wantErr: "no directory at or above " + gastown + "/crew/x is in the pod",
		},
		{
			name: "a directory the shell cannot enter is refused and named", root: root,
			target: locked, caller: locked, path: hostPath, tool: report, needsNonRoot: true,
			wantExit: 125, wantErr: "cannot enter " + locked, oneLine: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.needsNonRoot && os.Geteuid() == 0 {
				t.Skip("a root user enters a directory without search permission, so the refusal cannot be reached")
			}
			argv := append([]string{"-c", podWalkUpScript, "sh", tt.target, tt.root, tt.caller}, tt.tool...)
			cmd := exec.Command(sh, argv...)
			cmd.Env = []string{"PATH=" + tt.path}
			var stdout, stderr bytes.Buffer
			cmd.Stdout = &stdout
			cmd.Stderr = &stderr
			exit := 0
			if err := cmd.Run(); err != nil {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("sh did not run: %v", err)
				}
				exit = exitErr.ExitCode()
			}
			if exit != tt.wantExit {
				t.Fatalf("exit = %d, want %d; stderr %q", exit, tt.wantExit, stderr.String())
			}
			if stdout.String() != tt.wantOut {
				t.Fatalf("stdout = %q, want %q", stdout.String(), tt.wantOut)
			}
			if tt.wantErr != "" && !strings.Contains(stderr.String(), tt.wantErr) {
				t.Fatalf("stderr %q does not contain %q", stderr.String(), tt.wantErr)
			}
			if tt.oneLine && strings.Contains(strings.TrimSuffix(stderr.String(), "\n"), "\n") {
				t.Fatalf("the refusal is more than one line: %q", stderr.String())
			}
		})
	}
}
