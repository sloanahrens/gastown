package cmd

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTrimJSONForError(t *testing.T) {
	t.Parallel()
	// "€" is 3 bytes; 499 ASCII bytes put it across byte 500.
	straddling := strings.Repeat("a", 499) + "€" + strings.Repeat("b", 98)
	if len(straddling) != 600 {
		t.Fatalf("fixture is %d bytes, want 600", len(straddling))
	}

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"short input is kept", " {\"a\":1} \n", `{"a":1}`},
		{"exactly the limit is kept", strings.Repeat("a", 500), strings.Repeat("a", 500)},
		{"ASCII is cut at the limit", strings.Repeat("a", 600), strings.Repeat("a", 500) + "..."},
		{"rune straddling the limit is dropped whole", straddling, strings.Repeat("a", 499) + "..."},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := trimJSONForError([]byte(tt.in))
			if !utf8.ValidString(got) {
				t.Errorf("trimJSONForError returned invalid UTF-8: %q", got)
			}
			if got != tt.want {
				t.Errorf("trimJSONForError = %q, want %q", got, tt.want)
			}
		})
	}
}
