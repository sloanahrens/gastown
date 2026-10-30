package openshellprov

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeKeyFile stages content as a key file in a fresh temporary directory. The
// token DIR in content is replaced by that directory, so a value can name a
// path in it that the test can check was never created.
func writeKeyFile(t *testing.T, content string) (path, dir string) {
	t.Helper()
	dir = t.TempDir()
	path = filepath.Join(dir, "key.env")
	if err := os.WriteFile(path, []byte(strings.ReplaceAll(content, "DIR", dir)), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, dir
}

func TestKeyValueTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
		// notCreated names a file, relative to the key file's directory, that
		// must not exist after the read: a value that ran a command would make it.
		notCreated string
	}{
		{name: "plain assignment", content: "DEEPSEEK_API_KEY=abc123\n", want: "abc123"},
		{name: "export prefix", content: "export DEEPSEEK_API_KEY=abc123\n", want: "abc123"},
		{name: "double quoted", content: "DEEPSEEK_API_KEY=\"abc 123\"\n", want: "abc 123"},
		{name: "single quoted", content: "DEEPSEEK_API_KEY='abc123'\n", want: "abc123"},
		{name: "export with quotes", content: "export DEEPSEEK_API_KEY=\"abc123\"\n", want: "abc123"},
		{name: "last assignment wins", content: "DEEPSEEK_API_KEY=first\nDEEPSEEK_API_KEY=second\n", want: "second"},
		{name: "commented line is ignored", content: "DEEPSEEK_API_KEY=real\n# DEEPSEEK_API_KEY=old\n", want: "real"},
		{name: "only a commented line", content: "# DEEPSEEK_API_KEY=old\n", want: ""},
		{name: "another key is not this key", content: "OTHER_KEY=abc123\n", want: ""},
		{name: "a longer key name is not this key", content: "DEEPSEEK_API_KEY_LEGACY=abc123\n", want: ""},
		{name: "empty value", content: "DEEPSEEK_API_KEY=\n", want: ""},
		{name: "mismatched quotes are kept", content: "DEEPSEEK_API_KEY=\"abc123'\n", want: "\"abc123'"},
		{name: "lone quote is kept", content: "DEEPSEEK_API_KEY=\"\n", want: "\""},
		{
			name:       "command substitution stays literal",
			content:    "DEEPSEEK_API_KEY=$(touch DIR/pwned)\n",
			want:       "$(touch DIR/pwned)",
			notCreated: "pwned",
		},
		{
			name:       "backticks stay literal",
			content:    "DEEPSEEK_API_KEY=`touch DIR/pwned`\n",
			want:       "`touch DIR/pwned`",
			notCreated: "pwned",
		},
		{
			name:       "a variable stays literal",
			content:    "DEEPSEEK_API_KEY=$HOME/abc\n",
			want:       "$HOME/abc",
			notCreated: "pwned",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			path, dir := writeKeyFile(t, tc.content)
			got, err := KeyValue(path, "DEEPSEEK_API_KEY")
			if err != nil {
				t.Fatalf("KeyValue() error = %v, want none", err)
			}
			if got != strings.ReplaceAll(tc.want, "DIR", dir) {
				t.Errorf("KeyValue() = %q, want %q", got, strings.ReplaceAll(tc.want, "DIR", dir))
			}
			if tc.notCreated != "" {
				if _, err := os.Stat(filepath.Join(dir, tc.notCreated)); err == nil {
					t.Errorf("file %s exists: the value was executed", tc.notCreated)
				}
			}
		})
	}
}

func TestKeyValueReturnsTheReadError(t *testing.T) {
	t.Parallel()
	got, err := KeyValue(filepath.Join(t.TempDir(), "missing.env"), "DEEPSEEK_API_KEY")
	if err == nil {
		t.Fatal("KeyValue() of a missing file error = nil, want the read error")
	}
	if got != "" {
		t.Errorf("KeyValue() of a missing file = %q, want no value", got)
	}
}

// writeChunks writes each chunk to a masked writer in turn, checks every Write
// reports its whole chunk consumed, and returns what reached the underlying writer.
func writeChunks(t *testing.T, secret string, chunks []string) string {
	t.Helper()
	var out bytes.Buffer
	w := Masked(secret, &out)
	for _, c := range chunks {
		n, err := w.Write([]byte(c))
		if err != nil {
			t.Fatalf("Write(%q) error = %v, want none", c, err)
		}
		if n != len(c) {
			t.Fatalf("Write(%q) = %d, want %d", c, n, len(c))
		}
	}
	return out.String()
}

func TestMaskedReplacesEveryOccurrence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		secret string
		chunks []string
		want   string
	}{
		{name: "repeated", secret: "s3cret", chunks: []string{"s3cret and s3cret"}, want: "*** and ***"},
		{name: "adjacent", secret: "s3cret", chunks: []string{"s3crets3cret"}, want: "******"},
		{name: "one character", secret: "k", chunks: []string{"key k"}, want: "***ey ***"},
		{name: "regex metacharacters", secret: "a.*+?(b)[c]", chunks: []string{"a.*+?(b)[c] vs aXb"}, want: "*** vs aXb"},
		{name: "split across writes", secret: "password", chunks: []string{"my pass", "word here"}, want: "my *** here"},
		{name: "split one byte at a time", secret: "pass", chunks: []string{"p", "a", "s", "s"}, want: "***"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := writeChunks(t, tc.secret, tc.chunks); got != tc.want {
				t.Errorf("masked output = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestMaskedNeverLeaksTheSecretInAnyChunk(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	line := "token=s3cr3t-value;again s3cr3t-value\n"
	// Every split point, then every byte on its own: no chunking lets the
	// secret through whole or in part-way.
	for split := 0; split <= len(line); split++ {
		if got := writeChunks(t, secret, []string{line[:split], line[split:]}); strings.Contains(got, secret) {
			t.Errorf("split at %d: output %q contains the secret", split, got)
		}
	}
	bytewise := make([]string, 0, len(line))
	for i := range len(line) {
		bytewise = append(bytewise, line[i:i+1])
	}
	if got := writeChunks(t, secret, bytewise); strings.Contains(got, secret) {
		t.Errorf("byte-by-byte output %q contains the secret", got)
	}
}

func TestMaskedWritesEverythingButTheSecret(t *testing.T) {
	t.Parallel()
	line := "token=s3cr3t-value;again s3cr3t-value\n"
	bytewise := make([]string, 0, len(line))
	for i := range len(line) {
		bytewise = append(bytewise, line[i:i+1])
	}
	const want = "token=***;again ***\n"
	if got := writeChunks(t, "s3cr3t-value", bytewise); got != want {
		t.Errorf("byte-by-byte output = %q, want %q", got, want)
	}
}

func TestMaskedWithAnEmptySecretPassesEverythingThrough(t *testing.T) {
	t.Parallel()
	const input = "nothing to hide\n"
	if got := writeChunks(t, "", []string{input}); got != input {
		t.Errorf("masked output = %q, want the input unchanged", got)
	}
}

func TestMaskedReturnsTheUnderlyingWriteError(t *testing.T) {
	t.Parallel()
	_, err := Masked("s3cret", failingWriter{}).Write([]byte("no secret here"))
	if err == nil {
		t.Fatal("Write to a failing writer error = nil, want the write error")
	}
}
