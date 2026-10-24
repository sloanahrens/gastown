package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// GNU Make 4.3 cannot spawn recipes in an OpenShell sandbox (its recipe child's
// setresuid/setresgid is denied by the seccomp filter). The image builds 4.4.1
// into /usr/local/bin and must also make /usr/bin/make that binary, or a process
// with a restricted PATH (devops' host-rehearsal fixture builds with root's
// directories only) still finds 4.3 and fails with "cp: Operation not permitted"
// (gt-r1rd5). The build fails unless both paths report 4.4.1.
func TestPolecatImageReplacesTheDistroMake(t *testing.T) {
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
	text := strings.ReplaceAll(string(data), "\\\n", " ")
	for _, want := range []string{
		"ln -sf /usr/local/bin/make /usr/bin/make",
		"/usr/bin/make --version | head -1 | grep -q 'GNU Make 4.4.1'",
		"/bin/make --version | head -1 | grep -q 'GNU Make 4.4.1'",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("deploy/polecat/Dockerfile does not contain %q: /usr/bin/make would stay Ubuntu's 4.3", want)
		}
	}
}
