package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// ReadRawJSON reads path as an untyped JSON object, numbers kept as their
// exact text. It decodes nothing strictly, so a file the typed loaders refuse
// (one holding retired keys, for gt pool migrate) can still be read and
// converted. An absent file is an error satisfying os.IsNotExist.
func ReadRawJSON(path string) (map[string]any, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: internal config path
	if err != nil {
		return nil, err
	}
	return decodeRawObject(path, data)
}

func decodeRawObject(path string, data []byte) (map[string]any, error) {
	tree, err := decodeTree(data)
	if err != nil {
		return nil, &ParseError{Path: path, Err: err}
	}
	obj, ok := tree.(map[string]any)
	if !ok {
		return nil, &ParseError{Path: path, Err: errors.New("not a JSON object")}
	}
	return obj, nil
}

// UpdateRawJSON is UpdateConfigJSON for a file that is edited as an untyped
// tree: it holds the file's lock, reads the object, calls mutate, and replaces
// the file atomically when mutate reports a change. The file keeps its mode.
// mutate runs under the lock, so it is the place to back the file up. The
// file must exist; an absent one is an error satisfying os.IsNotExist.
func UpdateRawJSON(path string, mutate func(tree map[string]any) (changed bool, err error)) error {
	unlock, err := lockConfigFile(path)
	if err != nil {
		return err
	}
	defer unlock()
	data, err := os.ReadFile(path) //nolint:gosec // G304: internal config path
	if err != nil {
		return err
	}
	tree, err := decodeRawObject(path, data)
	if err != nil {
		return err
	}
	changed, err := mutate(tree)
	if err != nil || !changed {
		return err
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	out, err := json.MarshalIndent(tree, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}
	return replaceFile(path, append(out, '\n'), mode)
}
