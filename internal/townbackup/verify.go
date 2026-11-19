package townbackup

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"

	"github.com/steveyegge/gastown/internal/townstate"
)

// identityFile marks an archive as a town state: every night's export holds it.
const identityFile = "mayor/town.json"

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

// Verify reads the gzip tar archive at archive and refuses one it cannot read
// in full, one with no regular file or no mayor/town.json, one carrying a
// secret-shaped entry or a credential in a small file, and one with data past
// the tar stream's end, naming the entry path and the rule and never a value.
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
	tr := tar.NewReader(gz)
	identity := false
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return Result{}, fmt.Errorf("%s: reading the archive: %w", archive, err)
		}
		name := path.Clean(hdr.Name)
		if townstate.SecretShape(name) {
			return Result{}, fmt.Errorf("%s: entry %s has the shape of a secret", archive, name)
		}
		if hdr.Typeflag != tar.TypeReg && hdr.Typeflag != tar.TypeRegA {
			continue
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
	// The tar reader stops at the archive's end blocks, before the gzip
	// trailer, so draining the stream is what catches a truncated trailer or
	// a bad checksum instead of reading the night as clean.
	n, err := io.Copy(io.Discard, io.LimitReader(gz, maxTrailing))
	if err != nil {
		return Result{}, fmt.Errorf("%s: reading the gzip stream to its end: %w", archive, err)
	}
	if n == maxTrailing {
		return Result{}, fmt.Errorf("%s: has more than %d bytes after the archive's end", archive, maxTrailing)
	}
	if res.Files == 0 {
		return Result{}, fmt.Errorf("%s: holds no regular file", archive)
	}
	if !identity {
		return Result{}, fmt.Errorf("%s: has no %s", archive, identityFile)
	}
	return res, nil
}
