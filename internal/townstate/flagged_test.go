package townstate

import (
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"strings"
	"testing"
)

// flaggedToken is token-shaped and built from parts, so no credential literal
// sits in this file.
const flaggedToken = "sk-" + "abcdefghijklmnop1234"

// zeroReader yields zeros forever, so a bound is exercised without holding the
// input in the test.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }

// refuseReader fails every read.
type refuseReader struct{}

func (refuseReader) Read([]byte) (int, error) { return 0, errors.New("the host refused the read") }

func gzipBytes(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(data); err != nil {
		t.Fatalf("writing the gzip member: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip member: %v", err)
	}
	return buf.Bytes()
}

func TestFlaggedContent(t *testing.T) {
	t.Parallel()
	truncated := gzipBytes(t, []byte("the build ran for twenty minutes\n"))
	truncated = truncated[:len(truncated)-4]
	var over bytes.Buffer
	zw := gzip.NewWriter(&over)
	if _, err := io.Copy(zw, io.LimitReader(zeroReader{}, maxFlaggedBytes+1)); err != nil {
		t.Fatalf("compressing over the bound: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip member: %v", err)
	}
	cases := []struct {
		label string
		in    io.Reader
		want  string // "pass", "fail" or "unknown"
	}{
		{"plain file naming a credential", bytes.NewReader([]byte("API_KEY=" + flaggedToken + "\n")), "fail"},
		{"plain file holding a token-shaped word", bytes.NewReader([]byte(`{"note":"run with ` + flaggedToken + ` please"}`)), "fail"},
		{"clean plain file", bytes.NewReader([]byte("the build ran for twenty minutes\n")), "pass"},
		{"gzip file naming a credential", bytes.NewReader(gzipBytes(t, []byte("API_KEY="+flaggedToken+"\n"))), "fail"},
		{"clean gzip file", bytes.NewReader(gzipBytes(t, []byte("the build ran for twenty minutes\n"))), "pass"},
		{"gzip magic that does not decompress", bytes.NewReader([]byte{0x1f, 0x8b, 0x00, 0x00}), "fail"},
		{"truncated gzip member", bytes.NewReader(truncated), "fail"},
		{"gzip inside gzip holding a credential", bytes.NewReader(gzipBytes(t, gzipBytes(t, []byte("API_KEY="+flaggedToken+"\n")))), "fail"},
		{"a second gzip member holding a credential", bytes.NewReader(append(gzipBytes(t, []byte("the build ran\n")), gzipBytes(t, []byte("API_KEY="+flaggedToken+"\n"))...)), "fail"},
		{"a gzip member followed by garbage", bytes.NewReader(append(gzipBytes(t, []byte("the build ran\n")), []byte("not a member")...)), "fail"},
		{"empty file", bytes.NewReader(nil), "pass"},
		{"file over the bound", io.LimitReader(zeroReader{}, maxFlaggedBytes+1), "fail"},
		{"gzip content over the bound", bytes.NewReader(over.Bytes()), "fail"},
		{"a failing reader", refuseReader{}, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.label, func(t *testing.T) {
			t.Parallel()
			res := FlaggedContent(c.in)
			got := "unknown"
			switch {
			case res.IsPass():
				got = "pass"
			case res.IsFail():
				got = "fail"
			}
			if got != c.want {
				t.Fatalf("FlaggedContent(%s) = %v, want %s", c.label, res, c.want)
			}
			if !res.IsPass() && strings.Contains(res.Err().Error(), flaggedToken) {
				t.Errorf("FlaggedContent(%s) reason holds the credential: %v", c.label, res)
			}
		})
	}
}
