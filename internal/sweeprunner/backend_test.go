package sweeprunner

import (
	"strings"
	"testing"
)

// TestStepMissingSource holds the one tolerance the plan has: a step that names
// a source tolerates only a failure whose output says that source is not there,
// and a step that names none tolerates nothing (gt-4lj2q).
func TestStepMissingSource(t *testing.T) {
	t.Parallel()
	const logs = "/out/logs"
	for _, tc := range []struct {
		name   string
		step   Step
		output string
		want   bool
	}{
		{
			name:   "the declared source is absent",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /out/logs: Cannot stat: No such file or directory\n",
			want:   true,
		},
		{
			name:   "another file is absent, not the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: open /root/.kube/config: no such file or directory\n",
			want:   false,
		},
		{
			name:   "a path that starts with the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /out/logs-old: Cannot stat: No such file or directory\n",
			want:   false,
		},
		{
			name:   "a path that ends with the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /var/out/logs: Cannot stat: No such file or directory\n",
			want:   false,
		},
		{
			name:   "a path inside the declared source",
			step:   Step{MissingSourcePath: logs},
			output: "error: tar: /out/logs/integration: Cannot stat: No such file or directory\n",
			want:   false,
		},
		{
			name:   "another failure of a step that names a source",
			step:   Step{MissingSourcePath: logs},
			output: "Error from server (Forbidden): pods \"reader\" is forbidden\n",
			want:   false,
		},
		{
			name:   "an absent source a step does not name",
			step:   Step{},
			output: "error: /out/result.json: no such file or directory\n",
			want:   false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tc.step.MissingSource([]byte(tc.output)); got != tc.want {
				t.Errorf("MissingSource(%q) = %v, want %v", tc.output, got, tc.want)
			}
		})
	}
}

// TestGuardFirstLineStripsFormatting holds the first line a failed read quotes
// to one line of printable text: control, format and line or paragraph
// separator runes become spaces, so a log line cannot be split or reordered by
// the stderr of a read (gt-av8o36). It drives NewGuardUnreadable, the
// production constructor, and asserts the quoted FirstLine.
func TestGuardFirstLineStripsFormatting(t *testing.T) {
	t.Parallel()
	argv := []string{"kubectl", "get", "pods"}
	for _, tc := range []struct {
		name   string
		stderr string
		want   string
	}{
		{
			name:   "a right-to-left override is a space",
			stderr: "error\u202Eevil: denied\n",
			want:   "error evil: denied",
		},
		{
			name:   "a zero-width space is a space",
			stderr: "ab\u200Bcd\n",
			want:   "ab cd",
		},
		{
			name:   "a byte-order mark at the start is trimmed away",
			stderr: "\uFEFFerror: denied\n",
			want:   "error: denied",
		},
		{
			name:   "a line separator does not split the line",
			stderr: "a b\n",
			want:   "a b",
		},
		{
			name:   "a paragraph separator does not split the line",
			stderr: "a b\n",
			want:   "a b",
		},
		{
			name:   "an ANSI escape is a space, as before",
			stderr: "\x1b[31mred\x1b[0m\n",
			want:   "[31mred [0m",
		},
		{
			name:   "ordinary text is unchanged",
			stderr: "error: forbidden: pods \"reader\" is forbidden\n",
			want:   "error: forbidden: pods \"reader\" is forbidden",
		},
		{
			name:   "the cap counts runes, not bytes",
			stderr: strings.Repeat("é", 250) + "\n",
			want:   strings.Repeat("é", 200),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := NewGuardUnreadable(argv, 1, []byte(tc.stderr)).FirstLine
			if got != tc.want {
				t.Errorf("FirstLine(%q) = %q, want %q", tc.stderr, got, tc.want)
			}
		})
	}
}
