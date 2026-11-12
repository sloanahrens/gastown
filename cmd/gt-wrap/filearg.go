package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// maxFileArgBytes bounds a file the wrapper reads and sends: a bead's text,
// comment or design is far smaller, and the file travels in the kubectl exec.
const maxFileArgBytes = 1 << 20

// fileArgs makes a Mac file the call names reach the pod. Only the kubernetes
// target needs it: the pod cannot read a Mac path, so the wrapper reads the file
// here, before any kubectl call, and returns it as the call's stdin with the
// flag's value rewritten to "-". The file must be a regular file of at most
// maxFileArgBytes, read once; anything else is refused with the same line as an
// unreadable file. A call that names no file comes back unchanged
// with a nil reader. A relative path is read from dir, the caller's directory.
func fileArgs(cfg targetConfig, tool string, args []string, dir string, s surface) ([]string, io.Reader, error) {
	if cfg.Target != kubernetesTarget {
		return args, nil, nil
	}
	rewritten, file, err := gtwrap.ScanFileArgs(tool, args)
	if err != nil || file == nil {
		return args, nil, err
	}
	path := file.Path
	if !filepath.IsAbs(path) {
		path = filepath.Join(dir, path)
	}
	refuse := func(cause error) error {
		return fmt.Errorf("gt-wrap: cannot read %s for %s: %w; the tool did not run", file.Path, file.Flag, cause)
	}
	info, err := s.stat(path)
	if err != nil {
		return nil, nil, refuse(err)
	}
	if !info.Mode().IsRegular() {
		return nil, nil, refuse(errors.New("not a regular file"))
	}
	if info.Size() > maxFileArgBytes {
		return nil, nil, refuse(fmt.Errorf("larger than the %d-byte limit", maxFileArgBytes))
	}
	data, err := s.readFile(path)
	if err != nil {
		return nil, nil, refuse(err)
	}
	if len(data) > maxFileArgBytes {
		return nil, nil, refuse(fmt.Errorf("larger than the %d-byte limit", maxFileArgBytes))
	}
	return rewritten, bytes.NewReader(data), nil
}
