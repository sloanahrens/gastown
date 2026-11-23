package session

import (
	"path/filepath"
	"testing"
)

func TestSandboxDirs(t *testing.T) {
	t.Parallel()
	for _, d := range []struct{ name, got, want string }{
		{"SandboxProjectsDir", SandboxProjectsDir, sandboxProjectsDir},
		{"SandboxOutboxDir", SandboxOutboxDir, sandboxOutboxDir},
	} {
		if d.got != d.want || !filepath.IsAbs(d.got) {
			t.Errorf("%s = %q, want %q, an absolute path", d.name, d.got, d.want)
		}
	}
}
