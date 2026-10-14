package cmd

import (
	"reflect"
	"testing"
)

func TestEditorArgv(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		editor string
		want   []string
	}{
		{"unset falls back to vi", "", []string{"vi", "/p/f.md"}},
		{"blank falls back to vi", "  \t ", []string{"vi", "/p/f.md"}},
		{"bare command", "nano", []string{"nano", "/p/f.md"}},
		{"command with wait flag", "code --wait", []string{"code", "--wait", "/p/f.md"}},
		{"short flag", "subl -w", []string{"subl", "-w", "/p/f.md"}},
		{"extra whitespace", "  emacsclient   -nw  ", []string{"emacsclient", "-nw", "/p/f.md"}},
		{"absolute path", "/usr/bin/vim", []string{"/usr/bin/vim", "/p/f.md"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := editorArgv(tt.editor, "/p/f.md"); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("editorArgv(%q) = %q, want %q", tt.editor, got, tt.want)
			}
		})
	}
}
