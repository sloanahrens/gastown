package daemonimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/steveyegge/gastown/internal/templates"
)

// clientToolMaxBytes bounds one client tool download: the largest pinned one is
// about 70 MB, held in memory while it is verified.
const clientToolMaxBytes = 256 << 20

// ClientToolNames are the files FetchClientTools places in the build context,
// in the order of templates.DaemonClientTools.
func ClientToolNames() []string {
	names := make([]string, 0, len(templates.DaemonClientTools))
	for _, t := range templates.DaemonClientTools {
		names = append(names, t.Name)
	}
	return names
}

// FetchClientTools downloads each pinned client tool through fetchPinned (the
// flow the claude CLI uses), verifies the download's sha256 against its pin, and
// writes the executable to b.Out under the tool's name (taking the one member
// out of a .tar.gz download). It returns the paths in the order of the pins. A
// download that differs from its pin writes nothing (gt-sx34i).
func (b Builder) FetchClientTools(ctx context.Context) ([]string, error) {
	var paths []string
	for _, pin := range templates.DaemonClientTools {
		p, err := b.fetchClientTool(ctx, pin, clientToolMaxBytes)
		if err != nil {
			return nil, err
		}
		paths = append(paths, p)
	}
	return paths, nil
}

// fetchClientTool is one pin's fetch with the size bound injected.
func (b Builder) fetchClientTool(ctx context.Context, pin templates.ClientToolPin, max int64) (string, error) {
	if !sha256Pin.MatchString(pin.SHA256) {
		return "", fmt.Errorf("daemonimage: the %s pin %q is not a sha256 of 64 lower-case hex digits", pin.Name, pin.SHA256)
	}
	return b.fetchPinned(ctx, pinnedFile{what: pin.Name, name: pin.Name, url: pin.URL, sha256: pin.SHA256, member: pin.Member, max: max}, createClaudeTemp)
}

// extractMember returns the bytes of the one regular file named member in the
// .tar.gz archive. The name is compared whole and nothing is written to a path
// taken from the archive, so a hostile member name cannot place a file. The
// archive is read to at most max bytes decompressed, so a pinned archive that
// expands past the download bound is refused.
func extractMember(archive []byte, member string, max int64) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the download is not gzip: %w", err)
	}
	// The decompressed stream is bounded as a whole, so members that are skipped
	// count against max too.
	tr := tar.NewReader(io.LimitReader(zr, max))
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("the archive has no file %q", member)
		}
		if err != nil {
			return nil, fmt.Errorf("the archive does not read: %w", err)
		}
		if h.Typeflag == tar.TypeReg && h.Name == member {
			data, err := io.ReadAll(tr)
			if err != nil {
				return nil, fmt.Errorf("cannot read %q from the archive (it may expand past the %d-byte bound): %w", member, max, err)
			}
			return data, nil
		}
	}
}
