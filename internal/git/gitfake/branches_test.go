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

// An env-less listing after an env-carrying one reports no env, so a consumer
// never reads the previous call's deploy key as this call's (gt-4tc05).
func TestListRemoteRefsWithHashesEnvLessListingClearsEnv(t *testing.T) {
	t.Parallel()
	f := New()
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	f.InitBare(t, origin)
	dir := filepath.Join(root, "clone")
	f.InitBare(t, dir)
	f.AddRemote(t, dir, "origin", origin)
	h := f.Open(dir)

	if _, err := h.ListRemoteRefsWithHashesEnv("origin", "refs/heads/", []string{"GIT_SSH_COMMAND=ssh -i /keys/promote"}); err != nil {
		t.Fatalf("ListRemoteRefsWithHashesEnv: %v", err)
	}
	if _, err := h.ListRemoteRefsWithHashes("origin", "refs/heads/"); err != nil {
		t.Fatalf("ListRemoteRefsWithHashes: %v", err)
	}
	if got := f.ListRemoteEnv(); got != nil {
		t.Errorf("recorded env after an env-less listing = %v, want nil", got)
	}
}
