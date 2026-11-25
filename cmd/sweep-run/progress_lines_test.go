package main

import (
	"slices"
	"testing"
)

// feedLines writes chunks through a lineFeeder and returns the lines it fed.
func feedLines(t *testing.T, chunks ...string) []string {
	t.Helper()
	var got []string
	l := &lineFeeder{feed: func(line string) { got = append(got, line) }}
	for _, c := range chunks {
		if _, err := l.Write([]byte(c)); err != nil {
			t.Fatalf("Write(%q): %v", c, err)
		}
	}
	return got
}

func byteChunks(s string) []string {
	var chunks []string
	for i := range len(s) {
		chunks = append(chunks, s[i:i+1])
	}
	return chunks
}

func TestLineFeederFeedsEachCompleteLineOnce(t *testing.T) {
	t.Parallel()

	const line = "sweep: integration GREEN passed=12 failed=0 skipped=1 in 45s"
	tests := []struct {
		name   string
		chunks []string
		want   []string
	}{
		{"two lines in one chunk", []string{"a\nb\n"}, []string{"a", "b"}},
		{"line split in two chunks", []string{line[:20], line[20:] + "\n"}, []string{line}},
		{"line split at every byte", byteChunks(line + "\n"), []string{line}},
		{"line ends on chunk boundary", []string{"a\n", "b\n"}, []string{"a", "b"}},
		{"CRLF trimmed", []string{"a\r\n"}, []string{"a"}},
		{"CRLF split across chunks", []string{"a\r", "\n"}, []string{"a"}},
		{"empty line fed", []string{"a\n\nb\n"}, []string{"a", "", "b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := feedLines(t, tt.chunks...); !slices.Equal(got, tt.want) {
				t.Errorf("fed %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLineFeederFeedsUnterminatedLineOnFlushOnly(t *testing.T) {
	t.Parallel()

	var got []string
	l := &lineFeeder{feed: func(line string) { got = append(got, line) }}
	n, err := l.Write([]byte("a\nb"))
	if err != nil || n != 3 {
		t.Fatalf("Write = %d, %v; want 3, nil", n, err)
	}
	if want := []string{"a"}; !slices.Equal(got, want) {
		t.Fatalf("before Flush fed %q, want %q", got, want)
	}
	l.Flush()
	l.Flush()
	if want := []string{"a", "b"}; !slices.Equal(got, want) {
		t.Errorf("after Flush twice fed %q, want %q", got, want)
	}
}
