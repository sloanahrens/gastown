package installer

import (
	"strings"
	"testing"
)

// The cases follow the script's own loop: an unknown argument stops parsing
// where it stands, while --source and --sha are checked after every argument is
// read. The two orders differ in the commit the result line names, which is why
// both are pinned.
func TestParse(t *testing.T) {
	t.Parallel()
	defaults := Config{SHA: "origin/main", Source: "manual", SlotTimeout: "600", ImageFirst: true}
	cases := []struct {
		name    string
		args    []string
		want    Config
		wantErr string
	}{
		{
			name: "no arguments installs origin/main by hand",
			want: defaults,
		},
		{
			name: "every flag",
			args: []string{"--sha", "abc123", "--source", "post-land", "--slot-role", "gate",
				"--slot-timeout", "30", "--no-image-first", "--dry-run"},
			want: Config{SHA: "abc123", Source: "post-land", SlotRole: "gate", SlotTimeout: "30",
				DryRun: true, ImageFirst: false},
		},
		{
			name:    "an unknown argument names the token",
			args:    []string{"--sha", "abc123", "--bogus"},
			want:    Config{SHA: "abc123", Source: "manual", SlotTimeout: "600", ImageFirst: true},
			wantErr: "install-gt: unknown argument: --bogus",
		},
		{
			name:    "an unknown argument stops parsing where it stands",
			args:    []string{"--bogus", "--sha", "abc123"},
			want:    defaults,
			wantErr: "install-gt: unknown argument: --bogus",
		},
		{
			name:    "the equals form is not a flag",
			args:    []string{"--sha=abc123"},
			want:    defaults,
			wantErr: "install-gt: unknown argument: --sha=abc123",
		},
		{
			name:    "a bad source is refused after every argument is read",
			args:    []string{"--source", "bogus", "--sha", "abc123"},
			want:    Config{SHA: "abc123", Source: "bogus", SlotTimeout: "600", ImageFirst: true},
			wantErr: "install-gt: --source must be manual, rebuild-gt or post-land",
		},
		{
			name:    "a retired source is a bad source",
			args:    []string{"--source", "post-merge"},
			want:    Config{SHA: "origin/main", Source: "post-merge", SlotTimeout: "600", ImageFirst: true},
			wantErr: "install-gt: --source must be manual, rebuild-gt or post-land",
		},
		{
			name:    "an empty --sha names no commit",
			args:    []string{"--sha", ""},
			want:    Config{SHA: "", Source: "manual", SlotTimeout: "600", ImageFirst: true},
			wantErr: "install-gt: --sha needs a commit",
		},
		{
			name:    "a trailing --sha is emptied, as the script's assign leaves it",
			args:    []string{"--sha"},
			want:    Config{SHA: "", Source: "manual", SlotTimeout: "600", ImageFirst: true},
			wantErr: "install-gt: --sha needs a value",
		},
		{
			name:    "a trailing --slot-timeout is refused rather than left empty",
			args:    []string{"--sha", "abc123", "--slot-timeout"},
			want:    Config{SHA: "abc123", Source: "manual", SlotTimeout: "", ImageFirst: true},
			wantErr: "install-gt: --slot-timeout needs a value",
		},
		{
			name: "a value that looks like a flag is the value",
			args: []string{"--slot-role", "--dry-run"},
			want: Config{SHA: "origin/main", Source: "manual", SlotRole: "--dry-run",
				SlotTimeout: "600", ImageFirst: true},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := Parse(tc.args)
			if got != tc.want {
				t.Errorf("Parse(%q) = %+v, want %+v", tc.args, got, tc.want)
			}
			var gotErr string
			if err != nil {
				gotErr = err.Error()
			}
			if gotErr != tc.wantErr {
				t.Errorf("Parse(%q) error = %q, want %q", tc.args, gotErr, tc.wantErr)
			}
		})
	}
}

// Every message the parser returns is the script's own line, so a caller that
// greps the installer's error output keeps working across the port.
func TestParseMessagesCarryTheScriptsPrefix(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"--bogus"}, {"--sha", ""}, {"--source", "x"}} {
		_, err := Parse(args)
		if err == nil || !strings.HasPrefix(err.Error(), "install-gt: ") {
			t.Errorf("Parse(%q) error = %v, want an install-gt: line", args, err)
		}
	}
}
