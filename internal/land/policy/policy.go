// Package policy holds the fixed town-policy block om reads at landing: one
// checked-in markdown file, so the rules om weighs are reviewed like code
// rather than assembled at landing time (gt-lcpej).
package policy

import (
	_ "embed"
	"fmt"
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

// WriteBlock writes PolicyBlock to a 0600 temp file in dir, which the caller
// has created, and returns its path; the caller removes it, as it does for the
// criteria file (gt-lcpej).
func WriteBlock(dir string) (string, error) {
	f, err := os.CreateTemp(dir, "om-policy-*.md")
	if err != nil {
		return "", fmt.Errorf("policy block: creating temp file: %w", err)
	}
	name := f.Name()
	if _, err := f.WriteString(block); err != nil {
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
