package cmd

import "testing"

// The shell hook evals this output, so each value must reach the shell as one
// double-quoted word with $, backtick, double quote and backslash escaped. The
// integration-tier round-trip test checks that eval gives the value back.
func TestPrintEnvSetQuotesShellMetacharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain path", value: "/home/me/town", want: `export GT_TOWN_ROOT="/home/me/town"` + "\n"},
		{name: "dollar expansion", value: "/home/me/town$HOME", want: `export GT_TOWN_ROOT="/home/me/town\$HOME"` + "\n"},
		{name: "command substitution", value: "/home/me/town`printf x`", want: "export GT_TOWN_ROOT=\"/home/me/town\\`printf x\\`\"\n"},
		{name: "dollar paren substitution", value: "/home/me/$(printf x)", want: `export GT_TOWN_ROOT="/home/me/\$(printf x)"` + "\n"},
		{name: "double quote and backslash", value: `/home/me/"quoted"\back`, want: `export GT_TOWN_ROOT="/home/me/\"quoted\"\\back"` + "\n"},
		{name: "single quote", value: "/home/me/it's", want: `export GT_TOWN_ROOT="/home/me/it's"` + "\n"},
		{name: "newline", value: "/home/me/town\nnext", want: "export GT_TOWN_ROOT=\"/home/me/town\nnext\"\n"},
		{name: "non-ASCII", value: "/home/me/tōwn/日本語", want: `export GT_TOWN_ROOT="/home/me/tōwn/日本語"` + "\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := posixEnvSetLine("GT_TOWN_ROOT", tt.value); got != tt.want {
				t.Errorf("posixEnvSetLine(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

// The PowerShell hook reads this output, so each value must reach PowerShell as
// one double-quoted string with $, backtick and double quote escaped by a
// backtick. Backslash is literal in PowerShell and must not be escaped.
func TestPowershellDoubleQuoteEscapesPowershellMetacharacters(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  string
	}{
		{name: "plain path", value: `C:\Users\me\town`, want: `"C:\Users\me\town"`},
		{name: "dollar expansion", value: `C:\town$env:USERNAME`, want: "\"C:\\town`$env:USERNAME\""},
		{name: "backtick", value: "C:\\town`n", want: "\"C:\\town``n\""},
		{name: "double quote", value: `C:\"quoted"`, want: "\"C:\\`\"quoted`\"\""},
		{name: "single quote", value: `C:\it's`, want: `"C:\it's"`},
		{name: "non-ASCII", value: `C:\tōwn\日本語`, want: `"C:\tōwn\日本語"`},
		{name: "newline", value: "C:\\town\nnext", want: "\"C:\\town\nnext\""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := powershellDoubleQuote(tt.value); got != tt.want {
				t.Errorf("powershellDoubleQuote(%q) = %q, want %q", tt.value, got, tt.want)
			}
		})
	}
}

func TestRigCacheValueQuotesShellMetacharacters(t *testing.T) {
	t.Parallel()

	const townRoot = "/home/me/town$HOME`printf x`"
	const rigName = "rig$(printf x)"

	tests := []struct {
		name    string
		rigName string
		want    string
	}{
		{
			name:    "in a rig",
			rigName: rigName,
			want: "export GT_TOWN_ROOT=\"/home/me/town\\$HOME\\`printf x\\`\"; " +
				"export GT_ROOT=\"/home/me/town\\$HOME\\`printf x\\`\"; " +
				"export GT_RIG=\"rig\\$(printf x)\"",
		},
		{
			name: "town without a rig",
			want: "export GT_TOWN_ROOT=\"/home/me/town\\$HOME\\`printf x\\`\"; " +
				"export GT_ROOT=\"/home/me/town\\$HOME\\`printf x\\`\"; " +
				"unset GT_RIG",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := posixRigCacheValue(townRoot, tt.rigName); got != tt.want {
				t.Errorf("posixRigCacheValue(%q, %q) = %q, want %q", townRoot, tt.rigName, got, tt.want)
			}
		})
	}
}
