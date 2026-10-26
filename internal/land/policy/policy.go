// Package policy holds the fixed town-policy block om reads at landing: one
// checked-in markdown file, so the rules om weighs are reviewed like code
// rather than assembled at landing time (gt-lcpej).
package policy

import (
	_ "embed"
	"fmt"
	"io"
	"os"
)

//go:embed om-policy.md
var block string

// MaxBytes bounds PolicyBlock: om keeps a context file up to this size and
// truncates past it, so policy text that outgrew the bound would reach a
// review cut short without the write failing (gt-lcpej).
const MaxBytes = 8 << 10

// PolicyBlock returns the embedded town-policy text.
func PolicyBlock() string {
	return block
}

// tempFile is the part of an open temp file WriteBlock uses; a test passes a
// fake that fails a write or a close, which the real file cannot do on demand.
type tempFile interface {
	io.WriteCloser
	Name() string
}

// WriteBlock writes PolicyBlock to a 0600 temp file in dir, which the caller
// has created, and returns its path; the caller removes it, as it does for the
// criteria file (gt-lcpej). No caller passes the file to om yet: gt-3l5qy does.
func WriteBlock(dir string) (string, error) {
	return writeBlock(dir, func(dir, pattern string) (tempFile, error) {
		return os.CreateTemp(dir, pattern)
	})
}

// writeBlock is WriteBlock with the temp-file constructor injected, so the
// failure branches are reached by a fake rather than by a package variable.
func writeBlock(dir string, create func(dir, pattern string) (tempFile, error)) (string, error) {
	f, err := create(dir, "om-policy-*.md")
	if err != nil {
		return "", fmt.Errorf("policy block: creating temp file: %w", err)
	}
	name := f.Name()
	if _, err := io.WriteString(f, block); err != nil {
		_ = f.Close()
		_ = os.Remove(name)
		return "", fmt.Errorf("policy block: writing %s: %w", name, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(name)
		return "", fmt.Errorf("policy block: closing %s: %w", name, err)
	}
	return name, nil
}
