package testutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// SecretFSProbe is a fake of templates.SecretFS: every call is recorded, the
// metadata calls reach the host's filesystem, and a content read is refused
// rather than performed. A test that renders through it can prove the secrets
// code asked only for metadata and read no file, and can inject a metadata
// failure for one name. The one fake serves both internal/templates and
// internal/cmd (gt-gg9hv.66).
type SecretFSProbe struct {
	// FailName is the base name whose metadata call fails; "" fails none, so
	// the config directory's own Stat still succeeds.
	FailName string
	// LstatErr and StatErr are returned instead of reaching the host for
	// FailName, when set.
	LstatErr error
	StatErr  error
	// Calls records every method call as "<Method> <name>".
	Calls []string
}

// ErrSecretFSContentRead is what SecretFSProbe.ReadFile returns: a content read
// is an error, never an empty file.
var ErrSecretFSContentRead = errors.New("the fake refuses every content read")

func (p *SecretFSProbe) Lstat(name string) (fs.FileInfo, error) {
	p.Calls = append(p.Calls, "Lstat "+name)
	if p.LstatErr != nil && filepath.Base(name) == p.FailName {
		return nil, p.LstatErr
	}
	return os.Lstat(name)
}

func (p *SecretFSProbe) Stat(name string) (fs.FileInfo, error) {
	p.Calls = append(p.Calls, "Stat "+name)
	if p.StatErr != nil && filepath.Base(name) == p.FailName {
		return nil, p.StatErr
	}
	return os.Stat(name)
}

func (p *SecretFSProbe) ReadFile(name string) ([]byte, error) {
	p.Calls = append(p.Calls, "ReadFile "+name)
	return nil, ErrSecretFSContentRead
}
