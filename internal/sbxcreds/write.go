package sbxcreds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// writeKubeconfig creates path with mode 0600 and refuses a path that exists.
// The bytes go to a temporary beside path and are hard-linked into place, so a
// name that appears in between fails the link instead of being replaced, and path
// never holds a partial kubeconfig. Errors name paths, never the content.
func writeKubeconfig(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("could not create a temporary kubeconfig for %s: %w", path, err)
	}
	temp := f.Name()
	_, werr := f.Write(data)
	cerr := f.Chmod(0o600) // CreateTemp's own 0600 is cut by the umask
	derr := f.Close()
	var perr error
	if werr == nil && cerr == nil && derr == nil {
		perr = os.Link(temp, path)
	}
	rerr := os.Remove(temp)
	if cause := errors.Join(werr, cerr, derr, perr); cause != nil {
		return errors.Join(fmt.Errorf("could not write the kubeconfig %s: %w", path, cause), rerr)
	}
	if rerr != nil {
		return fmt.Errorf("kubeconfig %s is written, but its temporary %s was not removed: %w", path, temp, rerr)
	}
	return nil
}
