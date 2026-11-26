package sweeprunner

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

// TestCacheTrimRoundTrip: a missing record is absent; a written one reads back equal, 0644.
func TestCacheTrimRoundTrip(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, ok, err := ReadCacheTrim(dir); err != nil || ok {
		t.Fatalf("empty directory: ok=%v err=%v, want no record and no error", ok, err)
	}
	want := CacheTrimRecord{CapBytes: 5 << 30, OverBytes: 115661635}
	if err := WriteCacheTrim(dir, want); err != nil {
		t.Fatalf("WriteCacheTrim: %v", err)
	}
	got, ok, err := ReadCacheTrim(dir)
	if err != nil || !ok || got != want {
		t.Fatalf("ReadCacheTrim = %+v, %v, %v; want %+v, true, nil", got, ok, err, want)
	}
	if info, err := os.Stat(filepath.Join(dir, CacheTrimFileName)); err != nil || info.Mode().Perm() != 0o644 {
		t.Fatalf("published file (err %v) is not mode 0644", err)
	}
}

// TestCacheTrimReadRefusals: each unusable file is an error naming it, not absence.
func TestCacheTrimReadRefusals(t *testing.T) {
	t.Parallel()
	valid := `{"cap_bytes":1,"over_bytes":0}`
	raw := func(body string) func(*testing.T, string) {
		return func(t *testing.T, dir string) {
			t.Helper()
			if err := os.WriteFile(filepath.Join(dir, CacheTrimFileName), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	rows := []struct {
		name  string
		setup func(*testing.T, string)
	}{
		{"one byte over the bound", raw(valid + strings.Repeat(" ", cacheTrimMaxBytes+1-len(valid)))},
		{"not JSON", raw("cap_bytes=1 over_bytes=0")},
		{"data after the record", raw(valid + " {}")},
		{"negative cap", raw(`{"cap_bytes":-1,"over_bytes":0}`)},
		{"negative over figure", raw(`{"cap_bytes":1,"over_bytes":-5}`)},
		{"symlink at the name", func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Symlink("/nonexistent/elsewhere.json", filepath.Join(dir, CacheTrimFileName)); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, row := range rows {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			row.setup(t, dir)
			_, ok, err := ReadCacheTrim(dir)
			path := filepath.Join(dir, CacheTrimFileName)
			switch {
			case err == nil || ok:
				t.Fatalf("ReadCacheTrim accepted %s, or returned a record with its error", row.name)
			case !strings.Contains(err.Error(), path):
				t.Fatalf("error %q does not name %s", err, path)
			case errors.Is(err, fs.ErrNotExist):
				t.Fatalf("error %q reads as an absent file", err)
			}
		})
	}
}

// TestCacheTrimWriteRefusals: a negative figure is refused on write, leaving no file.
func TestCacheTrimWriteRefusals(t *testing.T) {
	t.Parallel()
	for _, rec := range []CacheTrimRecord{{CapBytes: -1}, {CapBytes: 1, OverBytes: -1}} {
		dir := t.TempDir()
		if err := WriteCacheTrim(dir, rec); err == nil {
			t.Fatalf("WriteCacheTrim accepted %+v", rec)
		}
		if _, err := os.Stat(filepath.Join(dir, CacheTrimFileName)); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("refused write of %+v left %s behind", rec, CacheTrimFileName)
		}
	}
}

// closeFails reads as its reader and fails Close with err.
type closeFails struct {
	io.Reader
	err error
}

func (c closeFails) Close() error { return c.err }

// TestCacheTrimFilesystemFailures: a reported failure is an error naming the file.
func TestCacheTrimFilesystemFailures(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent")
	if err := WriteCacheTrim(missing, CacheTrimRecord{CapBytes: 1}); err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("WriteCacheTrim into a missing directory: %v; want an error naming %s", err, missing)
	}
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, ok, err := ReadCacheTrim(file)
	if err == nil || ok || errors.Is(err, fs.ErrNotExist) || !strings.Contains(err.Error(), file) {
		t.Fatalf("ReadCacheTrim under a regular file = %v, %v; want an error naming %s", ok, err, file)
	}
	boom := errors.New("boom")
	path := filepath.Join(t.TempDir(), CacheTrimFileName)
	opens := []func(string) (io.ReadCloser, error){
		func(string) (io.ReadCloser, error) { return nil, boom },
		func(string) (io.ReadCloser, error) { return io.NopCloser(iotest.ErrReader(boom)), nil },
		func(string) (io.ReadCloser, error) { return closeFails{strings.NewReader("{}"), boom}, nil },
	}
	for i, open := range opens {
		if _, err := readCacheTrimFile(path, open); !errors.Is(err, boom) || !strings.Contains(err.Error(), path) {
			t.Fatalf("failure %d: readCacheTrimFile = %v; want the cause wrapped with %s", i, err, path)
		}
	}
}
