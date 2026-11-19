package townbackup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/steveyegge/gastown/internal/townstate"
)

// identityFile marks an archive as a town state: every night's export holds it.
const identityFile = "mayor/town.json"

// blockSize is one tar block. An archive ends with two zero blocks, and the
// tar reader reports a stream cut at a block boundary as a clean end, so Verify
// keeps the last two blocks to tell a closed archive from a truncated one.
const blockSize = 512

// maxScannedSize bounds the regular files Verify reads for credentials: a
// larger file is only counted, since scanning it would take longer than the
// night is worth.
const maxScannedSize = 256 << 10

// maxTrailing bounds what Verify reads past the tar stream's end blocks while
// reaching the gzip trailer, so a decompression bomb cannot run unbounded. An
// export writes nothing there.
const maxTrailing = 1 << 20

// Result is what a verified archive holds: its regular files and their bytes.
type Result struct {
	Files int
	Bytes int64
}

// Verify reads the gzip tar archive at archive and reports the regular files
// it holds, refusing one it cannot read in full, one with no regular file or
// no mayor/town.json, and one carrying a secret-shaped name or a credential.
func Verify(archive string) (res Result, err error) {
	f, err := os.Open(archive)
	if err != nil {
		return Result{}, fmt.Errorf("verifying %s: %w", archive, err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return Result{}, fmt.Errorf("%s: not a readable gzip stream: %w", archive, err)
	}
	// The trailer is checked while the stream is read, not by Close, so a
	// close error only matters when nothing else has failed.
	defer func() {
		if cerr := gz.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("%s: closing the gzip stream: %w", archive, cerr)
		}
	}()
	tail := &tailBuffer{b: make([]byte, 2*blockSize)}
	tr := tar.NewReader(io.TeeReader(gz, tail))
	identity := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, fmt.Errorf("%s: reading the archive: %w", archive, err)
		}
		name, err := entryName(hdr.Name)
		if err != nil {
			return Result{}, fmt.Errorf("%s: %w", archive, err)
		}
		if townstate.SecretShape(name) {
			return Result{}, fmt.Errorf("%s: entry %s has the shape of a secret", archive, name)
		}
		// An import accepts regular files, directories and symlinks. A
		// directory holds no content and a symlink's content is its target's
		// name, so both are checked by name; a regular file is scanned; every
		// other type can carry content no scan would see, so it is refused
		// rather than skipped (gt-2rj9jc).
		switch hdr.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeSymlink:
			if townstate.SecretShape(hdr.Linkname) {
				return Result{}, fmt.Errorf("%s: entry %s links to %s, which has the shape of a secret", archive, name, hdr.Linkname)
			}
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			return Result{}, fmt.Errorf("%s: entry %s has type %s; verify accepts regular files, directories and symlinks", archive, name, tarTypeName(hdr.Typeflag))
		}
		res.Files++
		res.Bytes += hdr.Size
		if name == identityFile {
			identity = true
		}
		if hdr.Size > maxScannedSize {
			continue
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return Result{}, fmt.Errorf("%s: reading entry %s: %w", archive, name, err)
		}
		if findings := townstate.FindCredentials(data); len(findings) > 0 {
			return Result{}, fmt.Errorf("%s: entry %s carries a credential (%s)", archive, name, strings.Join(findings, ", "))
		}
	}
	// The tar reader stops at the end blocks, before the gzip trailer, so
	// draining the stream is what catches a truncated trailer or a bad
	// checksum instead of reading the night as clean.
	n, err := io.Copy(io.Discard, io.LimitReader(gz, maxTrailing))
	if err != nil {
		return Result{}, fmt.Errorf("%s: reading the gzip stream to its end: %w", archive, err)
	}
	if n == maxTrailing {
		return Result{}, fmt.Errorf("%s: has %d or more bytes after the archive's end", archive, maxTrailing)
	}
	if !bytes.Equal(tail.b, make([]byte, len(tail.b))) {
		return Result{}, fmt.Errorf("%s: the tar stream ends without its end blocks", archive)
	}
	if res.Files == 0 {
		return Result{}, fmt.Errorf("%s: holds no regular file", archive)
	}
	if !identity {
		return Result{}, fmt.Errorf("%s: has no %s", archive, identityFile)
	}
	return res, nil
}

// tailBuffer keeps the last len(b) bytes written through it, so Verify can
// inspect the blocks the tar reader consumed on its way to the end.
type tailBuffer struct{ b []byte }

func (t *tailBuffer) Write(p []byte) (int, error) {
	if len(p) >= len(t.b) {
		copy(t.b, p[len(p)-len(t.b):])
		return len(p), nil
	}
	copy(t.b, t.b[len(p):])
	copy(t.b[len(t.b)-len(p):], p)
	return len(p), nil
}

// entryName refuses a tar name that cannot be a town-root-relative path, the
// rule an import applies, so a name the import refuses is not reported as a
// missing identity file instead.
func entryName(raw string) (string, error) {
	name := strings.TrimSuffix(raw, "/")
	if path.IsAbs(name) || filepath.IsAbs(name) {
		return "", fmt.Errorf("entry %s is absolute; verify accepts only paths relative to the town root", name)
	}
	for _, seg := range strings.Split(name, "/") {
		switch seg {
		case "..":
			return "", fmt.Errorf("entry %s contains ..; it would leave the town root", name)
		case "", ".":
			return "", fmt.Errorf("entry %q has an empty or %q segment", name, seg)
		}
	}
	return name, nil
}

// tarTypeName names a tar type flag for a refusal. An import accepts regular
// files, directories and symlinks, so every other type is named here instead
// of quoting a byte a terminal cannot show.
func tarTypeName(flag byte) string {
	switch flag {
	case tar.TypeLink:
		return "a hard link"
	case tar.TypeCont:
		return "a contiguous file"
	case tar.TypeGNUSparse:
		return "a sparse file"
	case tar.TypeChar:
		return "a character device"
	case tar.TypeBlock:
		return "a block device"
	case tar.TypeFifo:
		return "a fifo"
	case tar.TypeXGlobalHeader:
		return "a pax global header"
	default:
		return fmt.Sprintf("an unsupported type (0x%02x)", flag)
	}
}
