package templates

import (
	"io/fs"
	"os"
)

// osSecretFS is SecretFS over the host's filesystem. It is the only place in
// the secrets code that calls the os package directly: the renderer and the
// command reach the host through SecretFS, so a test replaces the whole of it,
// and the source scan fails on a direct open or read anywhere else in the
// secrets files (gt-gg9hv.66).
type osSecretFS struct{}

func (osSecretFS) Stat(name string) (fs.FileInfo, error)  { return os.Stat(name) }
func (osSecretFS) Lstat(name string) (fs.FileInfo, error) { return os.Lstat(name) }
func (osSecretFS) ReadFile(name string) ([]byte, error)   { return os.ReadFile(name) }
