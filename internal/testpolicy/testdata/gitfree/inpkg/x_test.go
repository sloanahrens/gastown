package git

import "testing"

func TestInPackage(t *testing.T) {
	t.Parallel()
	_ = NewGit(t.TempDir())
}
