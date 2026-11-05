package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadOperatorConfig: the operator identities come from the town settings
// file's operator.names, and a file or section that is not there is nothing
// configured rather than an error (gt-ywuip).
func TestLoadOperatorConfig(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		write bool
		json  string
		want  []string
	}{
		{name: "no settings file"},
		{
			name:  "a settings file with no operator section",
			write: true,
			json:  `{"type":"town-settings","version":1}`,
		},
		{
			name:  "an operator section with no names",
			write: true,
			json:  `{"operator":{}}`,
		},
		{
			name:  "listed names",
			write: true,
			json:  `{"operator":{"names":["Ada Operator","ada-operator"]}}`,
			want:  []string{"Ada Operator", "ada-operator"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			townRoot := t.TempDir()
			if tt.write {
				if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(TownSettingsPath(townRoot), []byte(tt.json), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			op, err := LoadOperatorConfig(townRoot)
			if err != nil {
				t.Fatalf("LoadOperatorConfig: %v", err)
			}
			if tt.want == nil {
				if op != nil && len(op.Names) != 0 {
					t.Fatalf("LoadOperatorConfig = %+v, want nothing configured", op)
				}
				return
			}
			if op == nil {
				t.Fatalf("LoadOperatorConfig = nil, want the names %q", tt.want)
			}
			if len(op.Names) != len(tt.want) {
				t.Fatalf("Names = %q, want %q", op.Names, tt.want)
			}
			for i, name := range tt.want {
				if op.Names[i] != name {
					t.Errorf("Names[%d] = %q, want %q", i, op.Names[i], name)
				}
			}
		})
	}
}

// TestLoadOperatorConfigReportsAnUnparseableFile: a settings file that is there
// and does not parse is an error, so the caller fails closed instead of reading
// it as "no operator identities" (gt-ywuip).
func TestLoadOperatorConfigReportsAnUnparseableFile(t *testing.T) {
	t.Parallel()
	townRoot := t.TempDir()
	if err := os.MkdirAll(filepath.Join(townRoot, "settings"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(TownSettingsPath(townRoot), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOperatorConfig(townRoot); err == nil {
		t.Fatal("LoadOperatorConfig read an unparseable settings file as no identities")
	}
}

// TestOperatorConfigMatches: a name is matched exactly after the author and the
// name are trimmed, case-sensitively, and neither an empty author nor an empty
// name matches anything (gt-ywuip).
func TestOperatorConfigMatches(t *testing.T) {
	t.Parallel()
	op := &OperatorConfig{Names: []string{"Ada Operator", "  ada-operator  ", ""}}
	tests := []struct {
		name   string
		op     *OperatorConfig
		author string
		want   bool
	}{
		{name: "an exact name", op: op, author: "Ada Operator", want: true},
		{name: "the author padded with spaces", op: op, author: "  Ada Operator\t", want: true},
		{name: "the name padded in the setting", op: op, author: "ada-operator", want: true},
		{name: "another case", op: op, author: "ada operator", want: false},
		{name: "a prefix", op: op, author: "Ada Operator (agent)", want: false},
		{name: "a suffix", op: op, author: "xAda Operator", want: false},
		{name: "an empty author", op: op, author: "", want: false},
		{name: "an empty author against an empty name", op: &OperatorConfig{Names: []string{""}}, author: " ", want: false},
		{name: "no configured operator", op: nil, author: "Ada Operator", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := tt.op.Matches(tt.author); got != tt.want {
				t.Errorf("Matches(%q) = %v, want %v", tt.author, got, tt.want)
			}
		})
	}
}
