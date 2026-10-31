package workspaceid

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeTownFile(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "settings"), 0o755); err != nil {
		t.Fatalf("mkdir settings: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, File), []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", File, err)
	}
	return root
}

func TestLoadMissingFileIsDefault(t *testing.T) {
	t.Parallel()
	m, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	id, err := m.For("gastown")
	if err != nil {
		t.Fatalf("For(gastown): %v", err)
	}
	if id != CustomerZero {
		t.Errorf("For(gastown) = %q, want %q", id, CustomerZero)
	}
}

func TestDefault(t *testing.T) {
	t.Parallel()
	m, err := Load(writeTownFile(t, `{"default":"town-default","rigs":{"gastown":"acme"}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := m.Default(); got != "town-default" {
		t.Errorf("Default() = %q, want town-default", got)
	}
	missing, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load(missing): %v", err)
	}
	if got := missing.Default(); got != CustomerZero {
		t.Errorf("Default() without a file = %q, want %q", got, CustomerZero)
	}
}

func TestForMappedAndUnmapped(t *testing.T) {
	t.Parallel()
	m, err := Load(writeTownFile(t, `{"default":"customer-zero","rigs":{"gastown":"acme"}}`))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if id, err := m.For("gastown"); err != nil || id != "acme" {
		t.Errorf(`For("gastown") = %q, %v; want "acme", nil`, id, err)
	}
	if id, err := m.For("other"); err != nil || id != CustomerZero {
		t.Errorf(`For("other") = %q, %v; want %q, nil`, id, err, CustomerZero)
	}
}

func TestLoadMalformedIsError(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"invalid JSON":     `{"default":`,
		"empty default":    `{"default":"","rigs":{}}`,
		"missing default":  `{"rigs":{"gastown":"acme"}}`,
		"unknown field":    `{"default":"customer-zero","rig":"gastown"}`,
		"empty rig prefix": `{"default":"customer-zero","rigs":{"":"acme"}}`,
		"empty rig id":     `{"default":"customer-zero","rigs":{"gastown":""}}`,
		"trailing data":    `{"default":"customer-zero"} {}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if m, err := Load(writeTownFile(t, body)); err == nil {
				t.Fatalf("Load(%s) = %+v, want an error", name, m)
			}
		})
	}
}

func TestLoadUnreadableFileIsError(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	// A directory where the file belongs is present but cannot be read, so it
	// must not read as absent.
	if err := os.MkdirAll(filepath.Join(root, File), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", File, err)
	}
	if m, err := Load(root); err == nil {
		t.Fatalf("Load = %+v, want an error", m)
	}
}

func TestLoadEmptyTownRootIsError(t *testing.T) {
	t.Parallel()
	if _, err := Load(""); !errors.Is(err, ErrNoTownRoot) {
		t.Fatalf("Load(\"\") error = %v, want ErrNoTownRoot", err)
	}
}

func TestForEmptyPrefixIsError(t *testing.T) {
	t.Parallel()
	m, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if id, err := m.For(""); !errors.Is(err, ErrEmptyRigPrefix) {
		t.Fatalf("For(\"\") = %q, %v; want ErrEmptyRigPrefix", id, err)
	}
}
