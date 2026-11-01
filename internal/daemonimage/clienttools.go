package daemonimage

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/templates"
)

// clientToolMaxBytes bounds one client tool download: the largest pinned one is
// about 70 MB.
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

// fetchClientTool is one pin's download, verification and placement.
func (b Builder) fetchClientTool(ctx context.Context, pin templates.ClientToolPin, max int64) (string, error) {
	if !claudeSHA256.MatchString(pin.SHA256) {
		return "", fmt.Errorf("daemonimage: the %s pin %q is not a sha256 of 64 lower-case hex digits", pin.Name, pin.SHA256)
	}
	body, err := b.Fetch(ctx, pin.URL)
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot download %s from %s: %w", pin.Name, pin.URL, err)
	}
	defer func() { _ = body.Close() }()
	dl, err := os.CreateTemp(b.Out, "."+pin.Name+"-dl-*")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot stage %s in %s: %w", pin.Name, b.Out, err)
	}
	defer func() { _ = os.Remove(dl.Name()) }()
	sum := sha256.New()
	n, err := io.Copy(io.MultiWriter(dl, sum), io.LimitReader(body, max+1))
	if cerr := dl.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot download %s from %s: %w", pin.Name, pin.URL, err)
	}
	if n > max {
		return "", fmt.Errorf("daemonimage: %s from %s is over the %d-byte bound; nothing was written to %s", pin.Name, pin.URL, max, b.Out)
	}
	if got := hex.EncodeToString(sum.Sum(nil)); got != pin.SHA256 {
		return "", fmt.Errorf("daemonimage: %s from %s has sha256 %s, not the pinned %s; nothing was written to %s", pin.Name, pin.URL, got, pin.SHA256, b.Out)
	}
	final := filepath.Join(b.Out, pin.Name)
	out, err := os.CreateTemp(b.Out, "."+pin.Name+"-out-*")
	if err != nil {
		return "", fmt.Errorf("daemonimage: cannot stage %s in %s: %w", pin.Name, b.Out, err)
	}
	placed := false
	defer func() {
		if !placed {
			_ = os.Remove(out.Name())
		}
	}()
	if pin.Member == "" {
		src, err := os.Open(dl.Name())
		if err != nil {
			_ = out.Close()
			return "", fmt.Errorf("daemonimage: cannot reread the %s download: %w", pin.Name, err)
		}
		_, err = io.Copy(out, src)
		_ = src.Close()
		if err != nil {
			_ = out.Close()
			return "", fmt.Errorf("daemonimage: cannot write %s: %w", pin.Name, err)
		}
	} else if err := extractMember(dl.Name(), pin.Member, out); err != nil {
		_ = out.Close()
		return "", fmt.Errorf("daemonimage: %s from %s: %w", pin.Name, pin.URL, err)
	}
	if err := out.Close(); err != nil {
		return "", fmt.Errorf("daemonimage: cannot write %s in %s: %w", pin.Name, b.Out, err)
	}
	if err := os.Rename(out.Name(), final); err != nil {
		return "", fmt.Errorf("daemonimage: cannot move %s into %s: %w", pin.Name, b.Out, err)
	}
	placed = true
	return final, nil
}

// extractMember copies the bytes of the one regular file named member out of the
// .tar.gz at archive into w. The name is compared whole and nothing is written
// to a path taken from the archive, so a hostile member name cannot place a file.
func extractMember(archive, member string, w io.Writer) error {
	f, err := os.Open(archive)
	if err != nil {
		return fmt.Errorf("cannot reread the download: %w", err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("the download is not gzip: %w", err)
	}
	defer func() { _ = zr.Close() }()
	tr := tar.NewReader(zr)
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return fmt.Errorf("the archive has no file %q", member)
		}
		if err != nil {
			return fmt.Errorf("the archive does not read: %w", err)
		}
		if h.Typeflag != tar.TypeReg || h.Name != member {
			continue
		}
		if _, err := io.Copy(w, io.LimitReader(tr, clientToolMaxBytes+1)); err != nil {
			return fmt.Errorf("cannot read %q from the archive: %w", member, err)
		}
		return nil
	}
}
