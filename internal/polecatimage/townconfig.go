package polecatimage

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// townImageSetting is key's value in the town's settings/config.json daemon.env,
// the map a plugin run's environment comes from. A town with no such file, no
// daemon.env, or no such key configured nothing and answers "", nil.
//
// A config that exists and cannot be read is an error, not "configured
// nothing": the run cannot tell the two apart, and one that published nothing
// because it could not read the town must not pass as one the town asked
// nothing of (gt-du3fi).
func townImageSetting(townRoot, key string) (string, error) {
	path := filepath.Join(townRoot, "settings", "config.json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the run's own town root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("%s could not be read", path)
	}
	var doc struct {
		Daemon json.RawMessage `json:"daemon"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", fmt.Errorf("%s is not readable JSON", path)
	}
	var daemon map[string]json.RawMessage
	if len(doc.Daemon) > 0 {
		if err := json.Unmarshal(doc.Daemon, &daemon); err != nil {
			return "", fmt.Errorf("%s is not readable JSON", path)
		}
	}
	block, ok := daemon["env"]
	if !ok {
		return "", nil
	}
	// Every entry stays raw and only the named key is decoded: daemon.env holds
	// the town's API keys, and one of them must never reach this process as a
	// string a log line could print.
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(block, &entries); err != nil {
		return "", fmt.Errorf("daemon.env in %s is not an object", path)
	}
	entry, ok := entries[key]
	if !ok {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(entry, &value); err != nil {
		return "", fmt.Errorf("%s in daemon.env of %s is not a string", key, path)
	}
	return value, nil
}
