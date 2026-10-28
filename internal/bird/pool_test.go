package bird

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// failingReader answers every read with an error, the way a broken file does.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("read failed") }

func TestParsePoolReportsAReadError(t *testing.T) {
	t.Parallel()
	if _, err := ParsePool(failingReader{}); err == nil {
		t.Fatal("ParsePool accepted a reader that fails")
	}
}

func TestParsePoolSkipsBlankAndCommentLines(t *testing.T) {
	t.Parallel()
	got, err := ParsePool(strings.NewReader("wren\n\n  # a comment\n\trobin \n\nlark\n"))
	if err != nil {
		t.Fatalf("ParsePool: %v", err)
	}
	want := []string{"wren", "robin", "lark"}
	if !slices.Equal(got, want) {
		t.Errorf("ParsePool = %q, want %q", got, want)
	}
}

func TestEmbeddedPoolNamesAreUsable(t *testing.T) {
	t.Parallel()
	names := Pool()
	if len(names) < 2 {
		t.Fatalf("embedded pool has %d names, want a pool to choose from", len(names))
	}
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		if !ValidName(name) {
			t.Errorf("embedded name %q is not a valid name", name)
		}
		if seen[name] {
			t.Errorf("embedded name %q appears twice", name)
		}
		seen[name] = true
	}
}

func TestValidName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want bool
	}{
		{"wren", true},
		{"a.b-c_1", true},
		{"", false},
		{"has space", false},
		{"x/y", false},
		{"héron", false},
		{"#wren", false},
	}
	for _, tc := range cases {
		if got := ValidName(tc.name); got != tc.want {
			t.Errorf("ValidName(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}
