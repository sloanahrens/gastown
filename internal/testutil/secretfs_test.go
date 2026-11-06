package testutil

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestSecretFSProbe: the fake the secrets render is tested through reaches the
// host for metadata, records every call, returns an injected failure for the one
// named file, and refuses a content read rather than returning bytes
// (gt-gg9hv.66).
func TestSecretFSProbe(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	name := filepath.Join(dir, "a.env")
	if err := os.WriteFile(name, []byte("placeholder"), 0o600); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	t.Run("metadata reaches the host and is recorded", func(t *testing.T) {
		t.Parallel()
		p := &SecretFSProbe{}
		if _, err := p.Lstat(name); err != nil {
			t.Fatalf("Lstat: %v", err)
		}
		if _, err := p.Stat(dir); err != nil {
			t.Fatalf("Stat: %v", err)
		}
		for _, want := range []string{"Lstat " + name, "Stat " + dir} {
			if !slices.Contains(p.Calls, want) {
				t.Errorf("Calls = %v, want %q recorded", p.Calls, want)
			}
		}
	})

	t.Run("an injected Lstat failure is returned for the named file only", func(t *testing.T) {
		t.Parallel()
		injected := errors.New("injected lstat failure")
		p := &SecretFSProbe{FailName: "a.env", LstatErr: injected}
		if _, err := p.Lstat(name); !errors.Is(err, injected) {
			t.Fatalf("Lstat of the named file = %v, want the injected failure", err)
		}
		if _, err := p.Lstat(dir); err != nil {
			t.Errorf("Lstat of another path = %v, want the host's result", err)
		}
	})

	t.Run("an injected Stat failure is returned for the named file only", func(t *testing.T) {
		t.Parallel()
		injected := errors.New("injected stat failure")
		p := &SecretFSProbe{FailName: "a.env", StatErr: injected}
		if _, err := p.Stat(name); !errors.Is(err, injected) {
			t.Fatalf("Stat of the named file = %v, want the injected failure", err)
		}
		if _, err := p.Stat(dir); err != nil {
			t.Errorf("Stat of another path = %v, want the host's result", err)
		}
	})

	t.Run("a content read is refused", func(t *testing.T) {
		t.Parallel()
		p := &SecretFSProbe{}
		data, err := p.ReadFile(name)
		if !errors.Is(err, ErrSecretFSContentRead) {
			t.Fatalf("ReadFile = %q, %v, want the refusal error", data, err)
		}
		if data != nil {
			t.Errorf("ReadFile = %q, want no bytes", data)
		}
		if !slices.Contains(p.Calls, "ReadFile "+name) {
			t.Errorf("Calls = %v, want the read recorded", p.Calls)
		}
	})
}
