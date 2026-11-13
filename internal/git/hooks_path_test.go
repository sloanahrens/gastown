package git

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHooksPathTargetExists covers every way a core.hooksPath value can resolve:
// unset, a directory (relative or absolute), a dangling path, and a path that
// cannot be stat'd because a parent is a file.
func TestHooksPathTargetExists(t *testing.T) {
	t.Parallel()
	repo := t.TempDir()
	if err := os.MkdirAll(filepath.Join(repo, ".githooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	absDir := filepath.Join(t.TempDir(), "hooks")
	if err := os.MkdirAll(absDir, 0o755); err != nil {
		t.Fatal(err)
	}
	regularFile := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(regularFile, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tt := range []struct {
		name    string
		value   string
		want    bool
		wantErr bool
	}{
		{name: "unset", value: "", want: false},
		{name: "relative directory", value: ".githooks", want: true},
		{name: "relative missing", value: ".beads/hooks", want: false},
		{name: "absolute directory", value: absDir, want: true},
		{name: "absolute dangling", value: filepath.Join(t.TempDir(), "gone"), want: false},
		{name: "parent is a file", value: filepath.Join(regularFile, "hooks"), wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := HooksPathTargetExists(repo, tt.value)
			if (err != nil) != tt.wantErr {
				t.Fatalf("HooksPathTargetExists(%q) error = %v, want error %v", tt.value, err, tt.wantErr)
			}
			if got != tt.want {
				t.Fatalf("HooksPathTargetExists(%q) = %v, want %v", tt.value, got, tt.want)
			}
		})
	}
}
