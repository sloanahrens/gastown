package townstate

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/steveyegge/gastown/internal/guard"
)

// maxFlaggedBytes bounds what FlaggedContent reads: a file larger than this,
// or a gzip member whose content is, is flagged rather than read.
const maxFlaggedBytes = 64 << 20

// MaxFlaggedBytes is the bound FlaggedContent reads under, exported so a caller
// that refuses a file by its declared size uses the rule's own number.
const MaxFlaggedBytes = maxFlaggedBytes

// gzipMagic opens every gzip member.
var gzipMagic = []byte{0x1f, 0x8b}

// FlaggedContent checks a file's content for a credential and returns a guard
// result, not a bool: Pass means the content was read to the end and holds none;
// Fail means it holds one, or cannot be shown to hold none (a stream that does not
// decompress, a truncated member, a member followed by garbage, a gzip member
// inside the member, content over maxFlaggedBytes); Unknown means r itself could
// not be read. Only Pass is clean: a caller that exports a file on anything else
// has reintroduced the bug guard.Result exists to close. No reason holds content.
// Only gzip is read: a credential inside a zip, bzip2, xz or zstd file is not
// found.
func FlaggedContent(r io.Reader) guard.Result {
	_, res := checkedContent(r, 0)
	return res
}

// checkedContent is FlaggedContent that also returns the bytes it judged, as
// read from r and not decompressed: a caller that writes a file out writes
// these, so what is written is what was checked and no second read can differ.
// The bytes are meaningful only when the result is Pass. size is the file's
// stat size, which sizes the read; zero or an unknown size keeps the growing read.
func checkedContent(r io.Reader, size int64) ([]byte, guard.Result) {
	var data []byte
	var err error
	if size > 0 && size <= maxFlaggedBytes {
		data, err = readSized(r, size)
	} else {
		data, err = io.ReadAll(io.LimitReader(r, maxFlaggedBytes+1))
	}
	if err != nil {
		return nil, guard.Unknown(fmt.Errorf("townstate: reading content: %w", err))
	}
	return data, judgeContent(data)
}

// readSized reads r into one buffer of the stat's size plus one byte, so a large
// file is not copied through io.ReadAll's doubling. A file that grew past its
// stat moves to the doubling read rather than failing.
func readSized(r io.Reader, size int64) ([]byte, error) {
	buf := make([]byte, 0, size+1)
	src := io.LimitReader(r, maxFlaggedBytes+1)
	for {
		if len(buf) == cap(buf) {
			buf = slices.Grow(buf, len(buf))
		}
		n, err := src.Read(buf[len(buf):cap(buf)])
		buf = buf[:len(buf)+n]
		if errors.Is(err, io.EOF) {
			return buf, nil
		}
		if err != nil {
			return nil, err
		}
	}
}

// judgeContent is the verdict on data already read.
func judgeContent(data []byte) guard.Result {
	if len(data) > maxFlaggedBytes {
		return guard.Fail("the content is over the bound")
	}
	if !bytes.HasPrefix(data, gzipMagic) {
		return credentialResult(data)
	}
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return guard.Fail("the gzip stream does not decompress")
	}
	// Reading to the end checks the member's checksum, so a corrupt, truncated or
	// garbage-trailed member fails here; the reader is over memory and has nothing
	// to close.
	content, err := io.ReadAll(io.LimitReader(zr, maxFlaggedBytes+1))
	if err != nil {
		return guard.Fail("the gzip stream does not decompress to its end")
	}
	if len(content) > maxFlaggedBytes {
		return guard.Fail("the decompressed content is over the bound")
	}
	// A gzip member inside the member is not read: a second layer is how a
	// credential would be hidden from the scan, and no real export nests one.
	if bytes.HasPrefix(content, gzipMagic) {
		return guard.Fail("the gzip stream holds another gzip stream")
	}
	return credentialResult(content)
}

// credentialResult is Fail when FindCredentials finds anything in data, and
// Pass otherwise.
func credentialResult(data []byte) guard.Result {
	if len(FindCredentials(data)) > 0 {
		return guard.FailErr(errors.New("the content holds a credential"))
	}
	return guard.Pass()
}
