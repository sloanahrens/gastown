package main

import (
	"bytes"
	"errors"
	"io"
	"io/fs"
	"os"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/testutil/unittier"
)

const cpuMax = "/sys/fs/cgroup/cpu.max"

func TestMain(m *testing.M) {
	os.Exit(unittier.Main(m))
}

// TestCPUQuotaCommand: cpu.max "600000 100000" prints exactly "6\n" and nothing on stderr.
func TestCPUQuotaCommand(t *testing.T) {
	t.Parallel()
	var out, errOut bytes.Buffer
	read := func(string) ([]byte, error) { return []byte("600000 100000"), nil }
	if err := run(&out, &errOut, read, func(string) string { return "" }, 16); err != nil {
		t.Fatalf("run() error = %v", err)
	}
	if out.String() != "6\n" || errOut.Len() != 0 {
		t.Errorf("stdout, stderr = %q, %q; want \"6\\n\" and empty stderr", out.String(), errOut.String())
	}
}

// TestCPUQuotaCommandFallback: an absent file is silent, an unreadable one falls back to
// numCPU with a note naming it, and a failed write on either stream is returned.
func TestCPUQuotaCommandFallback(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		read    func(string) ([]byte, error)
		outW    io.Writer
		errW    io.Writer
		wantOut string
		wantSub string
		wantErr bool
	}{
		{"absent", func(string) ([]byte, error) { return nil, fs.ErrNotExist }, nil, nil, "8\n", "", false},
		{"unreadable", func(string) ([]byte, error) { return nil, errors.New("permission denied") }, nil, nil, "8\n", cpuMax, false},
		{"stdout write fails", func(string) ([]byte, error) { return nil, fs.ErrNotExist }, brokenWriter{}, nil, "", "", true},
		{"stderr write fails", func(string) ([]byte, error) { return nil, errors.New("permission denied") }, nil, brokenWriter{}, "8\n", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out, errOut bytes.Buffer
			if tc.outW == nil {
				tc.outW = &out
			}
			if tc.errW == nil {
				tc.errW = &errOut
			}
			err := run(tc.outW, tc.errW, tc.read, func(string) string { return "" }, 8)
			if (err != nil) != tc.wantErr || out.String() != tc.wantOut || !strings.Contains(errOut.String(), tc.wantSub) || (tc.wantSub == "" && errOut.Len() != 0) {
				t.Errorf("run() = %v, stdout %q, stderr %q; want error %v, stdout %q, note %q", err, out.String(), errOut.String(), tc.wantErr, tc.wantOut, tc.wantSub)
			}
		})
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
