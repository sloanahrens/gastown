package repoguard

import (
	"reflect"
	"testing"
)

// trackerLine is written out rather than read from trackerReplace: a test that
// reuses the production constant passes when the constant loses the
// whitespace or the path the guard must admit.
const trackerLine = "replace github.com/steveyegge/gastown/tracker => ./tracker"

// TestCheckGoModReplacesTable pins each shape the guard must report, and the
// tracker line it must admit. The block cases expect the opening line alone:
// the entries inside name a module, not the replace keyword. The indented
// cases are the tightening beyond the script's column-0 regexp: Go's parser
// honors a top-level replace set off by whitespace, so the guard reports it.
func TestCheckGoModReplacesTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		goMod string
		want  []string
	}{
		{
			name:  "no replace directive",
			goMod: "module example.com/x\n\ngo 1.26\n",
		},
		{
			name:  "another module",
			goMod: "module example.com/x\n\nreplace example.com/y => ../y\n",
			want:  []string{"replace example.com/y => ../y"},
		},
		{
			name:  "a tab after the keyword",
			goMod: "replace\texample.com/y => ../y\n",
			want:  []string{"replace\texample.com/y => ../y"},
		},
		{
			name:  "a replace block",
			goMod: "replace (\n\texample.com/y => ../y\n)\n",
			want:  []string{"replace ("},
		},
		{
			name:  "a replace indented by a tab",
			goMod: "module example.com/x\n\n\treplace example.com/y => ../y\n",
			want:  []string{"\treplace example.com/y => ../y"},
		},
		{
			name:  "a replace block indented by spaces",
			goMod: "module example.com/x\n\n  replace (\n\texample.com/y => ../y\n  )\n",
			want:  []string{"  replace ("},
		},
		{
			name:  "a replace indented inside a require block",
			goMod: "require (\n\texample.com/y v1.0.0\n\treplace example.com/z => ../z\n)\n",
			want:  []string{"\treplace example.com/z => ../z"},
		},
		{
			name:  "the tracker line alone",
			goMod: "module example.com/x\n\n" + trackerLine + "\n",
		},
		{
			name:  "the tracker line indented",
			goMod: "module example.com/x\n\n\t" + trackerLine + "\n",
		},
		{
			name:  "the tracker module at another path",
			goMod: "replace github.com/steveyegge/gastown/tracker => ../tracker\n",
			want:  []string{"replace github.com/steveyegge/gastown/tracker => ../tracker"},
		},
		{
			name:  "the tracker line beside another replace",
			goMod: trackerLine + "\nreplace example.com/y => ../y\n",
			want:  []string{"replace example.com/y => ../y"},
		},
		{
			name:  "the tracker line inside a block",
			goMod: "replace (\n\tgithub.com/steveyegge/gastown/tracker => ./tracker\n)\n",
			want:  []string{"replace ("},
		},
		{
			name:  "another module with a CRLF line ending",
			goMod: "module example.com/x\r\n\r\nreplace example.com/y => ../y\r\n",
			want:  []string{"replace example.com/y => ../y"},
		},
		{
			name:  "the tracker line with a CRLF line ending",
			goMod: "module example.com/x\r\n\r\n" + trackerLine + "\r\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CheckGoModReplaces(tc.goMod); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("offending = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCheckGoModReplacesIgnoresComments covers the lines the regexp does not
// match, written from the regexp rather than from the script's cases: a line
// is a directive only when replace, after any indent, is followed by
// whitespace or an opening parenthesis.
func TestCheckGoModReplacesIgnoresComments(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		goMod string
	}{
		{
			name:  "a comment naming a replace",
			goMod: "// replace example.com/y => ../y\n",
		},
		{
			name:  "an indented comment naming a replace",
			goMod: "\t// replace example.com/y => ../y\n",
		},
		{
			name:  "a comment above the tracker line",
			goMod: "// replace the tracker module, see ADR 0006\n" + trackerLine + "\n",
		},
		{
			name:  "a bare replace",
			goMod: "replace\n",
		},
		{
			name:  "a word that only starts with replace",
			goMod: "replacements = example.com/y\n",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := CheckGoModReplaces(tc.goMod); len(got) != 0 {
				t.Errorf("offending = %q, want none", got)
			}
		})
	}
}
