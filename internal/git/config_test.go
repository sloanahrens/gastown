package git

import "testing"

func TestConfigSetWritesLocalConfig(t *testing.T) {
	t.Parallel()
	s := newScripted(map[string]reply{"config beads.role contributor": ok("")})
	if err := newTestGit(t, s).ConfigSet("beads.role", "contributor"); err != nil {
		t.Fatal(err)
	}
	s.noUnscripted(t)
	s = newScripted(map[string]reply{"config beads.role contributor": fail(3, "error: could not lock config file .git/config: File exists\n")})
	if err := newTestGit(t, s).ConfigSet("beads.role", "contributor"); err == nil {
		t.Fatal("a failed config write returned nil")
	}
}
