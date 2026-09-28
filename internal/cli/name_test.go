package cli

import (
	"os"
	"testing"
)

// envMap returns a getenv func over a mutable map.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestName_DefaultIsGt(t *testing.T) {
	t.Parallel()
	c := &nameCache{getenv: envMap(map[string]string{"GT_COMMAND": ""})}
	if got := c.get(); got != "gt" {
		t.Errorf("get() = %q, want %q", got, "gt")
	}
}

func TestName_RespectsGT_COMMAND(t *testing.T) {
	t.Parallel()
	c := &nameCache{getenv: envMap(map[string]string{"GT_COMMAND": "gastown"})}
	if got := c.get(); got != "gastown" {
		t.Errorf("get() = %q, want %q", got, "gastown")
	}
}

func TestName_OnceSemantics(t *testing.T) {
	t.Parallel()
	env := map[string]string{"GT_COMMAND": "first"}
	c := &nameCache{getenv: envMap(env)}
	if first := c.get(); first != "first" {
		t.Fatalf("get() = %q, want %q", first, "first")
	}
	// Changing env after first call should have no effect (sync.Once)
	env["GT_COMMAND"] = "second"
	if second := c.get(); second != "first" {
		t.Errorf("get() returned %q after env change, want %q (sync.Once should cache)", second, "first")
	}
}

// TestName_ReadsProcessEnv checks Name is wired to the process environment
// without mutating it.
func TestName_ReadsProcessEnv(t *testing.T) {
	t.Parallel()
	want := os.Getenv("GT_COMMAND")
	if want == "" {
		want = "gt"
	}
	if got := Name(); got != want {
		t.Errorf("Name() = %q, want %q", got, want)
	}
}
