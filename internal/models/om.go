package models

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
)

// OMPath is the reviewer's config under a home directory.
func OMPath(home string) string { return filepath.Join(home, ".config", "om", "config.json") }

// OMConfig is the reviewer's own config, ~/.config/om/config.json: the backend
// argv om runs, and the threshold, depth and timeout beside it.
type OMConfig struct {
	Backend   []string `json:"backend"`
	Threshold *float64 `json:"threshold"`
	Depth     string   `json:"depth"`
	Timeout   int      `json:"timeout"`

	raw    map[string]json.RawMessage
	loaded bool
}

// LoadOMConfig reads the reviewer's config. A missing file is an empty config;
// a file that does not parse is an error, because a backend nobody can read is
// not the same thing as none (gt-jaagc).
func LoadOMConfig(path string) (OMConfig, error) {
	var c OMConfig
	if path == "" {
		return c, nil
	}
	b, err := os.ReadFile(path) //nolint:gosec // G304: path is the caller's config path
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return OMConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := json.Unmarshal(b, &c.raw); err != nil {
		return OMConfig{}, fmt.Errorf("%s: %w", path, err)
	}
	c.loaded = true
	return c, nil
}

// Loaded reports whether the reviewer's config file was there.
func (c OMConfig) Loaded() bool { return c.loaded }

// WithBackend is the config carrying argv as its backend.
func (c OMConfig) WithBackend(argv []string) OMConfig {
	c.Backend = append([]string(nil), argv...)
	return c
}

// Save writes the config to path, keeping every key it did not own where it was
// so a setting this command does not know is not lost to a model switch
// (gt-jaagc). A new file is private to the user who runs the reviewer.
func (c OMConfig) Save(path string) error {
	doc := make(map[string]json.RawMessage, len(c.raw)+1)
	for k, v := range c.raw {
		doc[k] = v
	}
	b, err := json.Marshal(c.Backend)
	if err != nil {
		return err
	}
	doc["backend"] = b
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}
	return config.ReplaceFile(path, out, mode)
}
