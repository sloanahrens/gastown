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
	link string
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
	writeEntries(t, tw, entries...)
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

// gzipTarFlushed builds a gzip stream whose tar stream stops after the last
// entry, without the two zero end blocks: a write cut at a block boundary
// leaves the archive looking like this.
func gzipTarFlushed(t *testing.T, entries ...arcEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	writeEntries(t, tw, entries...)
	if err := tw.Flush(); err != nil {
		t.Fatalf("flushing the tar stream: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip stream: %v", err)
	}
	return buf.Bytes()
}

// gzipTarDeclared builds a gzip tar archive holding the identity file and then
// hdr, without hdr's body: Verify refuses an entry whose declared size is over
// the scan bound before it reads the body, so the fixture need not carry it.
func gzipTarDeclared(t *testing.T, hdr *tar.Header) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	writeEntries(t, tw, identity)
	if err := tw.WriteHeader(hdr); err != nil {
		t.Fatalf("writing header %s: %v", hdr.Name, err)
	}
	// The tar stream is not closed and no body follows: it stops after the
	// header, which is where Verify refuses.
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip stream: %v", err)
	}
	return buf.Bytes()
}

// gzipZeros returns a gzip member that decompresses to size zero bytes,
// written in chunks so the fixture never holds the whole body in memory.
func gzipZeros(t *testing.T, size int) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	chunk := make([]byte, 1<<20)
	for written := 0; written < size; {
		n := min(size-written, len(chunk))
		if _, err := zw.Write(chunk[:n]); err != nil {
			t.Fatalf("writing zeros: %v", err)
		}
		written += n
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("closing the gzip stream: %v", err)
	}
	return buf.Bytes()
}

// writeEntries writes each entry in the shape its type calls for.
func writeEntries(t *testing.T, tw *tar.Writer, entries ...arcEntry) {
	t.Helper()
	for _, e := range entries {
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ}
		switch hdr.Typeflag {
		case tar.TypeDir:
			hdr.Mode = 0o755
		case tar.TypeSymlink, tar.TypeLink:
			hdr.Linkname = e.link
		case tar.TypeChar, tar.TypeBlock:
			hdr.Mode = 0o600
			hdr.Devmajor, hdr.Devminor = 1, 1
		case tar.TypeFifo:
			hdr.Mode = 0o600
		case tar.TypeXGlobalHeader:
			// A global header carries only PAX records; the writer names it
			// itself when Name is empty.
			hdr.Format = tar.FormatPAX
			hdr.PAXRecords = map[string]string{"comment": "verify"}
		default:
			if hdr.Typeflag == 0 {
				hdr.Typeflag = tar.TypeReg
			}
			hdr.Mode = 0o644
			hdr.Size = int64(len(e.data))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("writing header %s: %v", e.name, err)
		}
		if hdr.Typeflag == tar.TypeReg || hdr.Typeflag == tar.TypeCont {
			if _, err := tw.Write([]byte(e.data)); err != nil {
				t.Fatalf("writing %s: %v", e.name, err)
			}
		}
	}
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
		{name: "settings/link.json", typ: tar.TypeSymlink, link: "config.json"},
	}
	var want Result
	for _, e := range files {
		if e.typ == 0 || e.typ == tar.TypeReg {
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
			data:    cutInHalf(gzipTar(t, identity)),
			wantErr: "reading the archive",
		},
		{
			name:    "file body cut short",
			data:    shortBody(t),
			wantErr: "reading entry " + identity.name,
		},
		{
			name:    "truncated gzip trailer",
			data:    dropTrailer(gzipTar(t, identity)),
			wantErr: "reading the gzip stream to its end",
		},
		{
			name:    "missing end blocks",
			data:    gzipTarFlushed(t, identity),
			wantErr: "ends without its end blocks",
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
			name:    "absolute entry name",
			data:    gzipTar(t, arcEntry{name: "/" + identityFile, data: "{}"}),
			wantErr: "is absolute",
		},
		{
			name:    "entry name with ..",
			data:    gzipTar(t, arcEntry{name: "mayor/../town.json", data: "{}"}),
			wantErr: "contains ..",
		},
		{
			name:    "entry name with an empty segment",
			data:    gzipTar(t, arcEntry{name: "settings//x.json", data: "{}"}),
			wantErr: "empty or",
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
			name:    "secret-shaped symlink name",
			data:    gzipTar(t, identity, arcEntry{name: "settings/daemon.key", typ: tar.TypeSymlink, link: "config.json"}),
			wantErr: "settings/daemon.key has the shape of a secret",
		},
		{
			name:    "symlink to a secret-shaped target",
			data:    gzipTar(t, identity, arcEntry{name: "settings/link.json", typ: tar.TypeSymlink, link: "secrets/daemon.env"}),
			wantErr: "settings/link.json links to secrets/daemon.env, which has the shape of a secret",
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
			if !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("error %q does not name %q", err, c.wantErr)
			}
			if c.notIn != "" && strings.Contains(err.Error(), c.notIn) {
				t.Errorf("error %q prints the credential value %q", err, c.notIn)
			}
		})
	}
}

// TestVerifyRefusesEntryTypes pins that a type an import would refuse is
// refused here, named with its type, before its content can go unscanned.
func TestVerifyRefusesEntryTypes(t *testing.T) {
	t.Parallel()
	cases := []struct {
		desc  string
		entry arcEntry
		// name is the entry name as the tar reader reports it.
		name string
		typ  string
	}{
		{"hard link", arcEntry{name: "settings/link.json", typ: tar.TypeLink, link: identity.name}, "settings/link.json", "a hard link"},
		{"contiguous file", arcEntry{name: "settings/blob", typ: tar.TypeCont, data: "x"}, "settings/blob", "a contiguous file"},
		{"character device", arcEntry{name: "settings/dev", typ: tar.TypeChar}, "settings/dev", "a character device"},
		{"block device", arcEntry{name: "settings/dev", typ: tar.TypeBlock}, "settings/dev", "a block device"},
		{"fifo", arcEntry{name: "settings/pipe", typ: tar.TypeFifo}, "settings/pipe", "a fifo"},
		{"pax global header", arcEntry{typ: tar.TypeXGlobalHeader}, "GlobalHead.0.0", "a pax global header"},
	}
	for _, c := range cases {
		t.Run(c.desc, func(t *testing.T) {
			t.Parallel()
			_, err := Verify(writeArchive(t, gzipTar(t, identity, c.entry)))
			if err == nil {
				t.Fatal("Verify accepted an entry of a type an import refuses")
			}
			want := "entry " + c.name + " has type " + c.typ
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q", err, want)
			}
		})
	}
}

// largeBody is a regular file size over the 256 KiB the old skip used, so a
// body this size went unscanned before gt-m5rofa.
const largeBody = 300 << 10

// verifyRefused builds an archive from data and requires Verify to refuse it
// with an error naming wantErr and never printing notIn (a credential value).
func verifyRefused(t *testing.T, data []byte, wantErr, notIn string) {
	t.Helper()
	got, err := Verify(writeArchive(t, data))
	if err == nil {
		t.Fatalf("Verify accepted the archive (Result %+v)", got)
	}
	if got != (Result{}) {
		t.Errorf("Result = %+v on refusal, want the zero Result", got)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Errorf("error %q does not name %q", err, wantErr)
	}
	if notIn != "" && strings.Contains(err.Error(), notIn) {
		t.Errorf("error %q prints the credential value %q", err, notIn)
	}
}

// TestVerifyScansLargeFiles pins that a regular file over 256 KiB is scanned
// whole, that a file over the scan bound is refused, and that a gzip member is
// decompressed and scanned through the same bound.
func TestVerifyScansLargeFiles(t *testing.T) {
	t.Parallel()
	t.Run("clean file of the same size passes", func(t *testing.T) {
		t.Parallel()
		body := strings.Repeat("x", largeBody)
		got, err := Verify(writeArchive(t, gzipTar(t, identity, arcEntry{name: "settings/x.json", data: body})))
		if err != nil {
			t.Fatalf("Verify refused a clean %d-byte file: %v", len(body), err)
		}
		want := Result{Files: 2, Bytes: int64(len(identity.data) + len(body))}
		if got != want {
			t.Errorf("Result = %+v, want %+v", got, want)
		}
	})
	t.Run("credential near its end is refused", func(t *testing.T) {
		t.Parallel()
		verifyRefused(t, gzipTar(t, identity, arcEntry{name: "settings/x.json", data: paddedCredential(largeBody)}),
			"settings/x.json carries a credential", "abc")
	})
	t.Run("over the scan bound is refused", func(t *testing.T) {
		t.Parallel()
		hdr := &tar.Header{Name: "settings/big.log", Mode: 0o644, Typeflag: tar.TypeReg, Size: maxScanBytes + 1}
		verifyRefused(t, gzipTarDeclared(t, hdr), "entry settings/big.log is too large to scan", "")
	})
	t.Run("gzip member carrying a credential is refused", func(t *testing.T) {
		t.Parallel()
		// A compressible body, so the member's own bytes do not hold the
		// credential's text: only a scan of the decompressed content finds it.
		member := gzipped(t, strings.Repeat("x", 4<<10)+"\nAPI_TOKEN=abc\n")
		if bytes.Contains(member, []byte("API_TOKEN")) {
			t.Fatal("fixture: the compressed member holds the credential text, so this test would not prove decompression")
		}
		verifyRefused(t, gzipTar(t, identity, arcEntry{name: "logs/export.json.gz", data: string(member)}),
			"logs/export.json.gz carries a credential", "abc")
	})
	t.Run("gzip member that does not decompress is refused", func(t *testing.T) {
		t.Parallel()
		verifyRefused(t, gzipTar(t, identity, arcEntry{name: "logs/export.json.gz", data: "\x1f\x8bnot a gzip stream"}),
			"does not decompress", "")
	})
	t.Run("gzip member cut short is refused", func(t *testing.T) {
		t.Parallel()
		member := gzipped(t, strings.Repeat("x", 4<<10))
		verifyRefused(t, gzipTar(t, identity, arcEntry{name: "logs/export.json.gz", data: string(member[:len(member)-1])}),
			"does not decompress", "")
	})
	t.Run("gzip member whose content is over the bound is refused", func(t *testing.T) {
		t.Parallel()
		verifyRefused(t, gzipTar(t, identity, arcEntry{name: "logs/export.json.gz", data: string(gzipZeros(t, maxScanBytes+1))}),
			"entry logs/export.json.gz is a gzip member whose content is too large to scan", "")
	})
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

// paddedCredential is a valid JSON document padded with spaces to size bytes,
// with the credential at its end so a scan that stopped early would miss it.
func paddedCredential(size int) string {
	tail := `{"api_key":"abc"}`
	return strings.Repeat(" ", size-len(tail)) + tail
}

// cutInHalf cuts data in half, as a transfer that stopped midway would.
func cutInHalf(data []byte) []byte {
	return data[:len(data)/2]
}

// dropTrailer cuts the gzip trailer, so every byte of the tar stream survives
// and only the checksum and length record are missing.
func dropTrailer(data []byte) []byte {
	return data[:len(data)-8]
}
