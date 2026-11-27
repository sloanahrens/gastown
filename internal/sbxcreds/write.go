package sbxcreds

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// kubeconfigOps are the filesystem steps writeKubeconfig takes, so a test fails
// each in turn; osKubeconfigOps does the real calls.
type kubeconfigOps struct {
	createTemp func(dir, pattern string) (*os.File, error)
	write      func(f *os.File, data []byte) error
	chmod      func(f *os.File, mode os.FileMode) error
	close      func(f *os.File) error
	link       func(oldname, newname string) error
	remove     func(name string) error
}

// osKubeconfigOps is the real file system. os.CreateTemp opens with O_EXCL, so
// the temporary never takes a name another run holds.
var osKubeconfigOps = kubeconfigOps{
	createTemp: os.CreateTemp,
	write: func(f *os.File, data []byte) error {
		_, err := f.Write(data)
		return err
	},
	chmod:  func(f *os.File, mode os.FileMode) error { return f.Chmod(mode) },
	close:  func(f *os.File) error { return f.Close() },
	link:   os.Link,
	remove: os.Remove,
}

// writeKubeconfig creates path with mode 0600 and refuses a path that exists.
// The bytes go to a temporary beside path and are hard-linked into place, so a
// name that appears in between fails the link instead of being replaced, and path
// never holds a partial kubeconfig. Errors name paths, never the content.
func writeKubeconfig(path string, data []byte) error {
	return osKubeconfigOps.publish(path, data)
}

// publish writes data at path through o, naming the step that failed and
// removing the temporary on every path.
func (o kubeconfigOps) publish(path string, data []byte) error {
	f, err := o.createTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("could not create a temporary kubeconfig for %s: %w", path, err)
	}
	temp := f.Name()
	step := func(name string, err error) error {
		if err != nil {
			return fmt.Errorf("could not %s the temporary kubeconfig for %s: %w", name, path, err)
		}
		return nil
	}
	werr := step("write", o.write(f, data))
	cerr := step("chmod", o.chmod(f, 0o600)) // CreateTemp's own 0600 is cut by the umask
	derr := step("close", o.close(f))
	var perr error
	if werr == nil && cerr == nil && derr == nil {
		if perr = o.link(temp, path); perr != nil {
			// Issue's refusals are read for this wording.
			perr = fmt.Errorf("could not write the kubeconfig %s: linking the temporary into place: %w", path, perr)
		}
	}
	return errors.Join(werr, cerr, derr, perr, step("remove", o.remove(temp)))
}
