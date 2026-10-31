// Package workspaceid answers which workspace a rig belongs to, from the
// optional town file settings/workspaces.json.
//
// Nothing calls this package yet: its callers are the beads that give the
// events feed, usage records, resource sampler rows and the sandbox inventory
// a workspace id (plan rows M2 to M5, gt-m7o6q).
package workspaceid

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// CustomerZero is the workspace id a town gets until the operator maps a rig
// to another one.
const CustomerZero = "customer-zero"

// File is the optional town file Load reads, relative to the town root.
const File = "settings/workspaces.json"

var (
	// ErrNoTownRoot reports Load called without a town root to read under.
	ErrNoTownRoot = errors.New("workspaceid: empty town root")
	// ErrEmptyRigPrefix reports For called without a rig prefix to look up.
	ErrEmptyRigPrefix = errors.New("workspaceid: empty rig prefix")
)

// Map is one read of a town's workspace ids.
type Map struct {
	defaultID string
	rigs      map[string]string
}

// file is the on-disk shape of File.
type file struct {
	Default string            `json:"default"`
	Rigs    map[string]string `json:"rigs"`
}

// Load reads <townRoot>/settings/workspaces.json: an absent file yields the
// CustomerZero default with no mappings, and a file that is present but does
// not hold a non-empty default and non-empty rig ids is an error.
func Load(townRoot string) (*Map, error) {
	if townRoot == "" {
		return nil, ErrNoTownRoot
	}
	path := filepath.Join(townRoot, File)
	data, err := os.ReadFile(path) //nolint:gosec // G304: path is the town root the caller named
	if errors.Is(err, os.ErrNotExist) {
		return &Map{defaultID: CustomerZero}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("workspaceid: reading %s: %w", path, err)
	}
	var f file
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return nil, fmt.Errorf("workspaceid: %s: %w", path, err)
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("workspaceid: %s: trailing data after the workspace map", path)
	}
	if f.Default == "" {
		return nil, fmt.Errorf("workspaceid: %s: no default workspace id", path)
	}
	for prefix, id := range f.Rigs {
		if prefix == "" {
			return nil, fmt.Errorf("workspaceid: %s: a rig entry has an empty prefix", path)
		}
		if id == "" {
			return nil, fmt.Errorf("workspaceid: %s: rig %q maps to an empty workspace id", path, prefix)
		}
	}
	return &Map{defaultID: f.Default, rigs: f.Rigs}, nil
}

// For is the workspace id of rigPrefix: its entry in the file, or the file's
// default when the prefix has none, and an error when the prefix is empty.
func (m *Map) For(rigPrefix string) (string, error) {
	if rigPrefix == "" {
		return "", ErrEmptyRigPrefix
	}
	if id, ok := m.rigs[rigPrefix]; ok {
		return id, nil
	}
	return m.defaultID, nil
}
