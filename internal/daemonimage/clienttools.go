package daemonimage

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/atomicfile"
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

// FetchClientTools downloads each pinned client tool, verifies the download's
// sha256 against its pin, and writes the executable to b.Out under the tool's
// name (taking the one member out of a .tar.gz download). It returns the
// paths in the order of the pins. A download that differs from its pin writes
// nothing (gt-sx34i).
func (b Builder) FetchClientTools(ctx context.Context) ([]string, error) {
	if b.Fetch == nil {
		return nil, errors.New("daemonimage: no fetcher to download the client tools with")
	}
	if b.Out == "" || !filepath.IsAbs(b.Out) {
		return nil, fmt.Errorf("daemonimage: build context %q is not an absolute directory; the client tools are written there for the Dockerfile's COPY", b.Out)
	}
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

// fetchClientTool is one pin's download, verification and placement. The
// download is held in memory until its sha256 verifies, so a refused download
// leaves nothing in b.Out.
func (b Builder) fetchClientTool(ctx context.Context, pin templates.ClientToolPin, max int64) (string, error) {
	if !claudeSHA256.MatchString(pin.SHA256) {
		return "", fmt.Errorf("daemonimage: the %s pin %q is not a sha256 of 64 lower-case hex digits", pin.Name, pin.SHA256)
	}
	body, err := b.Fetch(ctx, pin.URL)
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot download %s from %s: %w", pin.Name, pin.URL, err)
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot download %s from %s: %w", pin.Name, pin.URL, err)
	}
	if int64(len(data)) > max {
		return "", fmt.Errorf("daemonimage: %s from %s is over the %d-byte bound; nothing was written to %s", pin.Name, pin.URL, max, b.Out)
	}
	sum := sha256.Sum256(data)
	if got := hex.EncodeToString(sum[:]); got != pin.SHA256 {
		return "", fmt.Errorf("daemonimage: %s from %s has sha256 %s, not the pinned %s; nothing was written to %s", pin.Name, pin.URL, got, pin.SHA256, b.Out)
	}
	if pin.Member != "" {
		if data, err = extractMember(data, pin.Member); err != nil {
			return "", fmt.Errorf("daemonimage: %s from %s: %w", pin.Name, pin.URL, err)
		}
	}
	final := filepath.Join(b.Out, pin.Name)
	if err := atomicfile.WriteFile(final, data, 0o755); err != nil {
		return "", fmt.Errorf("daemonimage: cannot write %s: %w", final, err)
	}
	return final, nil
}

// extractMember returns the bytes of the one regular file named member in the
// .tar.gz archive. The name is compared whole and nothing is written to a path
// taken from the archive, so a hostile member name cannot place a file.
func extractMember(archive []byte, member string) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("the download is not gzip: %w", err)
	}
	tr := tar.NewReader(zr)
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
				return nil, fmt.Errorf("cannot read %q from the archive: %w", member, err)
			}
			return data, nil
		}
	}
}
