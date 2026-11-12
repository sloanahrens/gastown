package main

import (
	"bytes"
	"fmt"
	"io"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/gtwrap"
)

// fileArgs makes a Mac file the call names reach the pod. Only the kubernetes
// target needs it: the pod cannot read a Mac path, so the wrapper reads the file
// here, before any kubectl call, and returns it as the call's stdin with the
// flag's value rewritten to "-". A call that names no file comes back unchanged
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
	data, err := s.readFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("gt-wrap: cannot read %s for %s: %w; the tool did not run", file.Path, file.Flag, err)
	}
	return rewritten, bytes.NewReader(data), nil
}
