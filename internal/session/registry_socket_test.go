package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSocketName(t *testing.T) {
	t.Parallel()
	townRoot := filepath.Join("/towns", "My Town")
	derived := townSocketName(townRoot)
	for _, env := range []string{"", "default", "auto"} {
		if got := socketName(townRoot, env); got != derived {
			t.Errorf("socketName(%q, %q) = %q, want the derived %q", townRoot, env, got, derived)
		}
	}
	if got := socketName(townRoot, "mysocket"); got != "mysocket" {
		t.Errorf("socketName with explicit GT_TMUX_SOCKET = %q, want %q", got, "mysocket")
	}
}

func TestTownSocketName_Format(t *testing.T) {
	t.Parallel()
	got := townSocketName(filepath.Join(t.TempDir(), "myproject"))
	if !strings.HasPrefix(got, "myproject-") {
		t.Fatalf("socket %q should start with 'myproject-'", got)
	}
	hash := strings.TrimPrefix(got, "myproject-")
	if len(hash) != 6 {
		t.Errorf("socket hash suffix %q should be 6 hex chars, got %d", hash, len(hash))
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Errorf("socket hash suffix %q contains non-hex char %c", hash, c)
			break
		}
	}
}

func TestSanitizeTownName(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  string
	}{
		{"mytown", "mytown"},
		{"MyTown", "mytown"},
		{"my town", "my-town"},
		{"my_town!", "my-town"},
		{"  spaces  ", "spaces"},
		{"My-Town-123", "my-town-123"},
		{"café", "caf"},
		{"", "default"},
		{"!!!!", "default"},
		{"a/b/c", "a-b-c"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := sanitizeTownName(tt.input)
			if got != tt.want {
				t.Errorf("sanitizeTownName(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestTownSocketName(t *testing.T) {
	t.Parallel()
	tmpDir := t.TempDir()

	t.Run("includes basename and hash suffix", func(t *testing.T) {
		townRoot := filepath.Join(tmpDir, "gt")
		os.MkdirAll(townRoot, 0o755)
		got := townSocketName(townRoot)
		if !strings.HasPrefix(got, "gt-") {
			t.Errorf("townSocketName(%q) = %q, want prefix 'gt-'", townRoot, got)
		}
		// Should be "gt-" + 6 hex chars = 9 chars total
		parts := strings.SplitN(got, "-", 2)
		if len(parts) != 2 || len(parts[1]) != 6 {
			t.Errorf("townSocketName(%q) = %q, want 'gt-XXXXXX' format", townRoot, got)
		}
	})

	t.Run("deterministic for same path", func(t *testing.T) {
		townRoot := filepath.Join(tmpDir, "stable")
		os.MkdirAll(townRoot, 0o755)
		a := townSocketName(townRoot)
		b := townSocketName(townRoot)
		if a != b {
			t.Errorf("not deterministic: %q != %q", a, b)
		}
	})

	t.Run("different for same basename at different paths", func(t *testing.T) {
		pathA := filepath.Join(tmpDir, "a", "mytown")
		pathB := filepath.Join(tmpDir, "b", "mytown")
		os.MkdirAll(pathA, 0o755)
		os.MkdirAll(pathB, 0o755)
		socketA := townSocketName(pathA)
		socketB := townSocketName(pathB)
		if socketA == socketB {
			t.Errorf("same-basename dirs got same socket: %q", socketA)
		}
	})
}

func TestLegacySocketName(t *testing.T) {
	t.Parallel()
	got := LegacySocketName("/Users/hal/gt")
	if got != "gt" {
		t.Errorf("LegacySocketName = %q, want %q", got, "gt")
	}
	got = LegacySocketName("/home/user/My Town")
	if got != "my-town" {
		t.Errorf("LegacySocketName = %q, want %q", got, "my-town")
	}
}
