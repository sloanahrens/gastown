package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// The OpenShell Kubernetes driver runs the workload as uid/gid 10001, not as
// the image USER, so the image must give that uid a passwd and group entry or
// `id -un`, `ls -l`, chown and git or ssh lookups of the current user fail
// ("unknown userid 10001", gt-1csbf). The Docker driver still runs the image
// USER, which stays sandbox (uid 1000).
func TestPolecatDockerfileNamesTheKubernetesDriverUID(t *testing.T) {
	t.Parallel()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(filepath.Dir(here), "..", "..", "deploy", "polecat", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	// Continuation lines joined, so each instruction is one line.
	var runs []string
	user := ""
	for _, line := range strings.Split(strings.ReplaceAll(string(data), "\\\n", " "), "\n") {
		if cmd, isRun := strings.CutPrefix(line, "RUN "); isRun {
			runs = append(runs, cmd)
		}
		if u, isUser := strings.CutPrefix(line, "USER "); isUser {
			user = strings.TrimSpace(u)
		}
	}
	var group, account bool
	for _, cmd := range runs {
		for _, part := range strings.Split(cmd, "&&") {
			f := strings.Fields(part)
			if len(f) == 0 {
				continue
			}
			switch f[0] {
			case "groupadd":
				group = group || hasFlagValue(f, "-g", "10001")
			case "useradd":
				account = account || (hasFlagValue(f, "-u", "10001") && hasFlagValue(f, "-g", "10001") && hasFlagValue(f, "-d", "/sandbox"))
			}
		}
	}
	if !group {
		t.Error("the Dockerfile creates no group with gid 10001 (groupadd -g 10001 ...)")
	}
	if !account {
		t.Error("the Dockerfile creates no user with uid 10001, gid 10001 and home /sandbox (useradd -u 10001 -g 10001 -d /sandbox ...)")
	}
	if user != "sandbox" {
		t.Errorf("the image USER is %q, want sandbox: the Docker driver runs it as uid 1000", user)
	}
}

// hasFlagValue reports whether fields holds flag immediately followed by value.
func hasFlagValue(fields []string, flag, value string) bool {
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] == flag && fields[i+1] == value {
			return true
		}
	}
	return false
}
