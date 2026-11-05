package gitfake

import (
	"path/filepath"
	"reflect"
	"testing"
)

// The fake records the environment ListRemoteRefsWithHashesEnv was given, so
// a consumer's test can assert a deploy key's GIT_SSH_COMMAND reached the
// read (gt-4tc05).
func TestListRemoteRefsWithHashesEnvRecordsEnv(t *testing.T) {
	t.Parallel()
	f := New()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	f.InitBare(t, origin)
	dir := filepath.Join(root, "clone")
	f.InitBare(t, dir)
	f.AddRemote(t, dir, "origin", origin)

	env := []string{"GIT_SSH_COMMAND=ssh -i /keys/promote -o IdentitiesOnly=yes"}
	if _, err := f.Open(dir).ListRemoteRefsWithHashesEnv("origin", "refs/heads/", env); err != nil {
		t.Fatalf("ListRemoteRefsWithHashesEnv: %v", err)
	}
	if got := f.ListRemoteEnv(); !reflect.DeepEqual(got, env) {
		t.Errorf("recorded env = %v, want %v", got, env)
	}
}
