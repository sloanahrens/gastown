package presubmit

import (
	"slices"
	"strings"
	"testing"
)

// TestRedBaseTests holds one row per rule for naming where each changed test is declared (gt-ko70aq).
func TestRedBaseTests(t *testing.T) {
	t.Parallel()
	const file = "x_test.go"
	tests := []struct {
		name    string
		head    string
		names   []string
		want    []RedBaseTest
		wantErr string
	}{
		{
			name:  "each named test gives its name, file and func line, in file order",
			head:  "package x\n\n// TestA is documented.\nfunc TestA(t *testing.T) {}\n\nfunc helper() {}\n\nfunc TestB(t *testing.T) {\n}\n",
			names: []string{"TestB", "TestA"},
			want: []RedBaseTest{
				{Name: "TestA", File: file, Line: 4},
				{Name: "TestB", File: file, Line: 8},
			},
		},
		{
			name:  "a method sharing a test's name gives no entry",
			head:  "package x\n\nfunc (s S) TestA(t *testing.T) {}\n",
			names: []string{"TestA"},
		},
		{
			name:  "a method and a function with the same name give only the function",
			head:  "package x\n\nfunc (s S) TestA(t *testing.T) {}\n\nfunc TestA(t *testing.T) {}\n",
			names: []string{"TestA"},
			want:  []RedBaseTest{{Name: "TestA", File: file, Line: 5}},
		},
		{
			name:  "an unnamed function gives no entry",
			head:  "package x\n\nfunc helper(t *testing.T) {}\n",
			names: []string{"TestA"},
		},
		{
			name:  "no names gives no entry",
			head:  "package x\n\nfunc TestA(t *testing.T) {}\n",
			names: nil,
		},
		{
			name:    "a file that does not parse is an error naming it",
			head:    "package x\nfunc (\n",
			names:   []string{"TestA"},
			wantErr: file,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got, err := RedBaseTests(file, []byte(tt.head), tt.names)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("RedBaseTests: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("RedBaseTests = %v, want %v", got, tt.want)
			}
		})
	}
}
