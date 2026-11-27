package storypoints

import "testing"

// sizeBead is a bead description whose Size section body is size.
func sizeBead(size string) string {
	return "## Goal\nA thing.\n\n## Size\n" + size + "\n"
}

func TestDeclaredLines(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        int
		wantOK      bool
	}{
		{
			name:        "good line",
			description: sizeBead("Change: at most 80 added lines in 2 files."),
			want:        80,
			wantOK:      true,
		},
		{
			name:        "good line without the files clause",
			description: sizeBead("Change: at most 80 added lines."),
			want:        80,
			wantOK:      true,
		},
		{
			name:        "bulleted and bold line",
			description: sizeBead("- **Change:** at most 80 added lines in 2 files."),
			want:        80,
			wantOK:      true,
		},
		{
			name:        "same number stated twice",
			description: sizeBead("Change: at most 80 added lines in 2 files.\nChange: at most 80 added lines."),
			want:        80,
			wantOK:      true,
		},
		{
			name:        "no Change line",
			description: sizeBead("One worker, one landing."),
			wantOK:      false,
		},
		{
			name:        "Change line outside the Size section",
			description: "## Goal\nChange: at most 80 added lines.\n\n## Size\nOne worker, one landing.\n",
			wantOK:      false,
		},
		{
			name:        "zero",
			description: sizeBead("Change: at most 0 added lines in 2 files."),
			wantOK:      false,
		},
		{
			name:        "negative",
			description: sizeBead("Change: at most -5 added lines."),
			wantOK:      false,
		},
		{
			name:        "number with a comma",
			description: sizeBead("Change: at most 1,000 added lines in 2 files."),
			wantOK:      false,
		},
		{
			name:        "number too large for an int",
			description: sizeBead("Change: at most 99999999999999999999 added lines."),
			wantOK:      false,
		},
		{
			name:        "malformed Change line beside a good one",
			description: sizeBead("Change: at most 80 added lines.\nChange: at most many added lines."),
			wantOK:      false,
		},
		{
			name:        "two Change lines with different numbers",
			description: sizeBead("Change: at most 80 added lines.\nChange: at most 120 added lines."),
			wantOK:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DeclaredLines(tt.description)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("DeclaredLines = %d, %t; want %d, %t", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestDeclaredLinesInline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		description string
		want        int
		wantOK      bool
	}{
		{
			name:        "budget stated inside a sentence",
			description: sizeBead("One worker, one landing. Change: at most 227 added lines in 3 files."),
			want:        227,
			wantOK:      true,
		},
		{
			name:        "two inline wordings take the smaller",
			description: sizeBead("One worker, one landing. Change: at most 227 added lines in 3 files. Change: at most 70 added lines in 2 files."),
			want:        70,
			wantOK:      true,
		},
		{
			name:        "no declaration",
			description: sizeBead("One worker, one landing."),
			wantOK:      false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, ok := DeclaredLines(tt.description)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("DeclaredLines = %d, %t; want %d, %t", got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
