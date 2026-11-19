package townbackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// arcEntry is one entry of an archive a test builds in memory.
type arcEntry struct {
	name string
	typ  byte
	data string
}

// gzipTar builds a gzip-compressed tar archive from entries.
func gzipTar(t *testing.T, entries ...arcEntry) []byte {
	t.Helper()
	return gzipTarTrailing(t, 0, entries...)
}

// gzipTarTrailing builds gzipTar and then appends trailing bytes, inside the
// gzip stream, after the tar stream's end blocks.
func gzipTarTrailing(t *testing.T, trailing int, entries ...arcEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ}
		if hdr.Typeflag == tar.TypeDir {
			hdr.Mode = 0o755
		} else {
			if hdr.Typeflag == 0 {
				hdr.Typeflag = tar.TypeReg
			}
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing header %s: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatalf("writing %s: %v", e.name, err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("closing the tar stream: %v", err)
	}
	if trailing > 0 {
		if _, err := zw.Write(make([]byte, trailing)); err != nil {
			t.Fatalf("writing trailing bytes: %v", err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip stream: %v", err)
	}
	return buf.Bytes()
}

// gzipped compresses data without a tar stream behind it.
func gzipped(t *testing.T, data string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(data)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// writeArchive puts data in a fresh directory under the archive's own name.
func writeArchive(t *testing.T, data []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), ArchiveName)
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// identity is the entry every accepted archive carries.
var identity = arcEntry{name: "mayor/town.json", data: `{"name":"town"}`}

func TestVerifyAcceptsATownArchive(t *testing.T) {
	t.Parallel()
	files := []arcEntry{
		{name: "mayor/", typ: tar.TypeDir},
		identity,
		{name: "settings/config.json", data: `{"theme":"dark"}`},
		{name: "rigs/gastown/config.json", data: `{"name":"gastown"}`},
	}
	var want Result
	for _, e := range files {
		if e.typ != tar.TypeDir {
			want.Files++
			want.Bytes += int64(len(e.data))
		}
	}
	got, err := Verify(writeArchive(t, gzipTar(t, files...)))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if got != want {
		t.Errorf("Result = %+v, want %+v", got, want)
	}
}

func TestVerifyRefuses(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		data    []byte
		wantErr string
		// notIn is text the refusal must not print: a credential's value.
		notIn string
	}{
		{
			name:    "not a gzip stream",
			data:    []byte("not an archive"),
			wantErr: "not a readable gzip stream",
		},
		{
			name:    "gzip of bytes that are not a tar stream",
			data:    gzipped(t, "hello"),
			wantErr: "reading the archive",
		},
		{
			name:    "truncated gzip in the middle",
			data:    truncate(gzipTar(t, identity, arcEntry{name: "settings/config.json", data: strings.Repeat("x", 4096)})),
			wantErr: "",
		},
		{
			name:    "file body cut short",
			data:    shortBody(t),
			wantErr: "reading entry " + identity.name,
		},
		{
			name:    "truncated gzip trailer",
			data:    dropTrailer(gzipTar(t, identity)),
			wantErr: "",
		},
		{
			name:    "data after the tar end blocks beyond the bound",
			data:    gzipTarTrailing(t, maxTrailing, identity),
			wantErr: "after the archive's end",
		},
		{
			name:    "no regular file",
			data:    gzipTar(t, arcEntry{name: "mayor/", typ: tar.TypeDir}),
			wantErr: "holds no regular file",
		},
		{
			name:    "no town identity file",
			data:    gzipTar(t, arcEntry{name: "settings/config.json", data: "{}"}),
			wantErr: "has no " + identityFile,
		},
		{
			name:    "secret-shaped file",
			data:    gzipTar(t, identity, arcEntry{name: "settings/daemon.env", data: "HOST=x\n"}),
			wantErr: "settings/daemon.env has the shape of a secret",
		},
		{
			name:    "secret-shaped directory",
			data:    gzipTar(t, identity, arcEntry{name: "settings/secrets/", typ: tar.TypeDir}),
			wantErr: "settings/secrets has the shape of a secret",
		},
		{
			name:    "credential in a JSON body",
			data:    gzipTar(t, identity, arcEntry{name: "settings/x.json", data: `{"api_key":"abc"}`}),
			wantErr: "settings/x.json carries a credential (json:api_key)",
			notIn:   "abc",
		},
		{
			name:    "credential in an env body",
			data:    gzipTar(t, identity, arcEntry{name: "settings/x.conf", data: "API_TOKEN=abc\n"}),
			wantErr: "settings/x.conf carries a credential (env:API_TOKEN)",
			notIn:   "abc",
		},
		{
			name:    "credential at the scan limit",
			data:    gzipTar(t, identity, arcEntry{name: "settings/x.json", data: paddedCredential(maxScannedSize)}),
			wantErr: "settings/x.json carries a credential",
			notIn:   "abc",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Verify(writeArchive(t, c.data))
			if err == nil {
				t.Fatalf("Verify accepted the archive (Result %+v)", got)
			}
			if got != (Result{}) {
				t.Errorf("Result = %+v on refusal, want the zero Result", got)
			}
			if c.wantErr == "" {
				return
			}
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not name %q", err, c.wantErr)
			}
			if c.notIn != "" && strings.Contains(err.Error(), c.notIn) {
				t.Errorf("error %q prints the credential value %q", err, c.notIn)
			}
		})
	}
}

// TestVerifySkipsLargeFiles pins the scan limit: a file one byte over
// maxScannedSize is counted but not read, so a credential in it is not looked
// for.
func TestVerifySkipsLargeFiles(t *testing.T) {
	t.Parallel()
	body := paddedCredential(maxScannedSize + 1)
	got, err := Verify(writeArchive(t, gzipTar(t, identity, arcEntry{name: "settings/x.json", data: body})))
	if err != nil {
		t.Fatalf("Verify refused a file beyond the scan limit: %v", err)
	}
	want := Result{Files: 2, Bytes: int64(len(identity.data) + len(body))}
	if got != want {
		t.Errorf("Result = %+v, want %+v", got, want)
	}
}

// TestVerifyRefusesAMissingArchive pins that an archive that cannot be read is
// refused, not read as an empty night.
func TestVerifyRefusesAMissingArchive(t *testing.T) {
	t.Parallel()
	missing := filepath.Join(t.TempDir(), "absent", ArchiveName)
	if _, err := Verify(missing); err == nil {
		t.Fatal("Verify accepted an archive that is not there")
	}
}

// shortBody builds a gzip tar stream whose one regular file declares more
// bytes than the stream carries, as a transfer cut inside a file body leaves it.
func shortBody(t *testing.T) []byte {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	hdr := &tar.Header{Name: identity.name, Mode: 0o644, Typeflag: tar.TypeReg, Size: 4096}
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("short")); err != nil {
		t.Fatal(err)
	}
	// The writer is not closed: closing pads the missing bytes and hides the
	// short body this archive means to hold.
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(tarBuf.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// paddedCredential is a valid JSON document holding a credential, padded with
// spaces to size bytes.
func paddedCredential(size int) string {
	head := `{"api_key":"abc"}`
	return head + strings.Repeat(" ", size-len(head))
}

// truncate cuts data in half, as a transfer that stopped midway would.
func truncate(data []byte) []byte {
	return data[:len(data)/2]
}

// dropTrailer cuts the gzip trailer, so every byte of the tar stream survives
// and only the checksum and length record are missing.
func dropTrailer(data []byte) []byte {
	return data[:len(data)-8]
}
