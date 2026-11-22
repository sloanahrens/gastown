package daemon

import "testing"

// TestLimitedBufferKeepsTheFirstBytes: a buffer of 8 bytes written 5 then 10
// bytes holds the first 8 and says it truncated; every write reports its full
// length with no error, so a child never sees a short write.
func TestLimitedBufferKeepsTheFirstBytes(t *testing.T) {
	t.Parallel()
	b := &limitedBuffer{limit: 8}
	for _, w := range []string{"abcde", "fghijklmno"} {
		n, err := b.Write([]byte(w))
		if n != len(w) || err != nil {
			t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
		}
	}
	if got := string(b.Bytes()); got != "abcdefgh" {
		t.Errorf("Bytes() = %q, want %q", got, "abcdefgh")
	}
	if !b.Truncated() {
		t.Error("Truncated() = false after writing past the limit")
	}
}

// TestLimitedBufferKeepsWritesUnderTheLimitWhole: writes that fit are kept
// whole and do not report truncation, including an empty write and a write that
// ends exactly at the limit.
func TestLimitedBufferKeepsWritesUnderTheLimitWhole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		limit  int
		writes []string
		want   string
	}{
		{"under the limit", 8, []string{"abc", "de"}, "abcde"},
		{"empty write", 8, []string{""}, ""},
		{"exactly the limit", 4, []string{"ab", "cd"}, "abcd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			b := &limitedBuffer{limit: tt.limit}
			for _, w := range tt.writes {
				if n, err := b.Write([]byte(w)); n != len(w) || err != nil {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
				}
			}
			if got := string(b.Bytes()); got != tt.want {
				t.Errorf("Bytes() = %q, want %q", got, tt.want)
			}
			if b.Truncated() {
				t.Error("Truncated() = true for writes that fit")
			}
		})
	}
}
