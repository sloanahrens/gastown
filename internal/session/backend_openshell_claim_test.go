package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gofrs/flock"
)

// TestOpenshellHalfMadeClaim: a written claim names its session and gateway,
// keeps a second writer out until it is unlocked, is unseen by the record
// listing, and Remove deletes it (gt-5dw70j).
func TestOpenshellHalfMadeClaim(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	c, err := writeSandboxClaim(town, "gt-box", "gw-one")
	if err != nil {
		t.Fatalf("writeSandboxClaim: %v", err)
	}
	path := sandboxClaimPath(town, "gt-box")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the claim: %v", err)
	}
	if want := `{"session":"gt-box","gateway":"gw-one"}`; strings.TrimSpace(string(data)) != want {
		t.Errorf("claim = %q, want %s", data, want)
	}
	if got := ListSandboxSessions(town); len(got) != 0 {
		t.Errorf("ListSandboxSessions = %v, want the claim ignored", got)
	}
	other := flock.New(path)
	if ok, err := other.TryLock(); ok || err != nil {
		t.Errorf("TryLock while held = %v, %v, want refused", ok, err)
	}
	if err := c.Unlock(); err != nil {
		t.Fatalf("Unlock: %v", err)
	}
	if ok, err := other.TryLock(); !ok || err != nil {
		t.Fatalf("TryLock after Unlock = %v, %v, want taken", ok, err)
	}
	if err := other.Unlock(); err != nil {
		t.Fatalf("unlocking the second holder: %v", err)
	}
	if err := c.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("stat after Remove = %v, want gone", err)
	}
}

// TestOpenshellHalfMadeClaimRefusesWhatItCannotWrite: each failed step returns
// its error and no claim, and a failed write leaves no claim file (gt-5dw70j).
func TestOpenshellHalfMadeClaimRefusesWhatItCannotWrite(t *testing.T) {
	t.Parallel()
	must := func(t *testing.T, err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	const box = "gt-box"
	cases := []struct {
		name, wantErr string
		session       string
		block         func(t *testing.T, town string)
		gone          bool
	}{
		{"bad session name", "bad session name", "../gt-box", nil, true},
		{"registry cannot be made", "creating the sandbox registry", box, func(t *testing.T, town string) {
			must(t, os.WriteFile(filepath.Join(town, ".runtime"), []byte("x"), 0o600))
		}, false},
		{"claim cannot be locked", "locking the sandbox claim", box, func(t *testing.T, town string) {
			must(t, os.MkdirAll(sandboxClaimPath(town, box), 0o700))
		}, false},
		{"claim cannot be written", "writing the sandbox claim", box, func(t *testing.T, town string) {
			must(t, os.MkdirAll(SandboxRegistryDir(town), 0o755))
			must(t, os.WriteFile(sandboxClaimPath(town, box), []byte("{}\n"), 0o444))
		}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town := t.TempDir()
			if tc.block != nil {
				tc.block(t, town)
			}
			c, err := writeSandboxClaim(town, tc.session, "gw")
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("writeSandboxClaim = %v, want an error naming %q", err, tc.wantErr)
			}
			if c != nil {
				t.Fatalf("writeSandboxClaim returned a claim alongside %v", err)
			}
			if _, statErr := os.Stat(sandboxClaimPath(town, tc.session)); tc.gone && !os.IsNotExist(statErr) {
				t.Errorf("a failed write left its claim file: stat = %v", statErr)
			}
		})
	}
}
