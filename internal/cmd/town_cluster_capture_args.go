package cmd

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode"
)

// parseClusterCaptureRef splits a capture ref of the form <kind>/<name> into its
// kind and name. The ref must hold exactly one slash, both parts must be
// non-empty and the ref may hold no whitespace; any other ref is an error that
// names it. It reads no cluster and does no I/O.
func parseClusterCaptureRef(ref string) (kind, name string, err error) {
	if strings.Count(ref, "/") != 1 {
		return "", "", fmt.Errorf("the ref %q is not <kind>/<name>: it must hold exactly one slash", ref)
	}
	kind, name, _ = strings.Cut(ref, "/")
	if kind == "" || name == "" {
		return "", "", fmt.Errorf("the ref %q is not <kind>/<name>: its kind and its name must both be non-empty", ref)
	}
	if strings.IndexFunc(ref, unicode.IsSpace) >= 0 {
		return "", "", fmt.Errorf("the ref %q is not <kind>/<name>: it may not hold whitespace", ref)
	}
	return kind, name, nil
}

// clusterCaptureOutPath returns the absolute, cleaned path a capture is written
// to: out itself when it is absolute, else out taken from workDir. workDir must
// be absolute, so the path never depends on the process's working directory, and
// the path must lie strictly inside workDir: a ".." escape, an absolute path
// elsewhere or workDir itself is an error. It does no I/O.
func clusterCaptureOutPath(workDir, out string) (string, error) {
	if !filepath.IsAbs(workDir) {
		return "", fmt.Errorf("the work dir %q is not absolute, so the capture path %q cannot be resolved", workDir, out)
	}
	workDir = filepath.Clean(workDir)
	path := out
	if !filepath.IsAbs(out) {
		path = filepath.Join(workDir, out)
	}
	path = filepath.Clean(path)
	// Both paths are clean and absolute here, so Rel fails only on a path it
	// cannot relate to workDir; that failure refuses the path like any escape.
	rel, err := filepath.Rel(workDir, path)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("the capture path %q is not inside the work dir %q", out, workDir)
	}
	return path, nil
}
