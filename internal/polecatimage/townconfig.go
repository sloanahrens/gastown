package polecatimage

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// townImageSetting is key's value in the town's settings/config.json daemon.env,
// the map the daemon reads the environment of a plugin run from, and why the
// file named none. A town with no such file, no daemon.env, or no such key
// configured nothing and is not at fault, so why is "".
func townImageSetting(townRoot, key string) (value, why string) {
	path := filepath.Join(townRoot, "settings", "config.json")
	data, err := os.ReadFile(path) //nolint:gosec // G304: the path is the run's own town root
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ""
		}
		return "", path + " could not be read"
	}
	var doc struct {
		Daemon json.RawMessage `json:"daemon"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return "", path + " is not readable JSON"
	}
	var daemon map[string]json.RawMessage
	if len(doc.Daemon) > 0 {
		if err := json.Unmarshal(doc.Daemon, &daemon); err != nil {
			return "", path + " is not readable JSON"
		}
	}
	block, ok := daemon["env"]
	if !ok {
		return "", ""
	}
	// Every entry stays raw and only the named key is decoded: daemon.env holds
	// the town's API keys, and one of them must never reach this process as a
	// string a log line could print.
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(block, &entries); err != nil {
		return "", "daemon.env in " + path + " is not an object"
	}
	entry, ok := entries[key]
	if !ok {
		return "", ""
	}
	if err := json.Unmarshal(entry, &value); err != nil {
		return "", key + " in daemon.env of " + path + " is not a string"
	}
	return value, ""
}
