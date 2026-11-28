package presubmit

import (
	"slices"
	"strings"
	"testing"
)

// integrationTag is the build line that makes IntegrationTagged report true for a source file.
const integrationTag = "//go:build integration\n\n"

// TestChangedTaggedTests holds one row per rule for naming the tests a landing
// changed in an integration-tagged test file (gt-10zcdg).
func TestChangedTaggedTests(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		base      string
		newAtHead bool
		head      string
		want      []string
		wantErr   string
	}{
		{"an added and a body-changed test are named, sorted", integrationTag + testSrc("func TestC(t *testing.T) {}", "func TestA(t *testing.T) { t.Log(1) }"), false, integrationTag + testSrc("func TestA(t *testing.T) { t.Log(2) }", "func TestB(t *testing.T) {}", "func TestC(t *testing.T) {}"), []string{"TestA", "TestB"}, ""},
		{"an unchanged test and a comment-only edit are not named", integrationTag + testSrc("func TestA(t *testing.T) {}", "func TestC(t *testing.T) {", "// old", "t.Log(1)", "}"), false, integrationTag + testSrc("func TestA(t *testing.T) {}", "func TestC(t *testing.T) {", "// new", "t.Log(1)", "}"), nil, ""},
		{"a nil base names every test of head, sorted", "", true, integrationTag + testSrc("func TestB(t *testing.T) {}", "func TestA(t *testing.T) {}"), []string{"TestA", "TestB"}, ""},
		{"an untagged head names nothing", "", true, testSrc("func TestA(t *testing.T) {}"), nil, ""},
		{"a head that does not parse is an error naming the file", "", true, integrationTag + "package x\nfunc (\n", nil, "x_test.go"},
		{"a base that does not parse is an error naming the file", "package x\nfunc (\n", false, integrationTag + testSrc("func TestA(t *testing.T) {}"), nil, "x_test.go"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var base []byte
			if !tt.newAtHead {
				base = []byte(tt.base)
			}
			got, err := ChangedTaggedTests("x_test.go", base, []byte(tt.head))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Errorf("error = %v, want one naming %s", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ChangedTaggedTests: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("ChangedTaggedTests = %v, want %v", got, tt.want)
			}
		})
	}
}
