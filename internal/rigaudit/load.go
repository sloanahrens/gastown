package rigaudit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/steveyegge/gastown/internal/config"
)

// Load builds the audit's Input from the town at townRoot: the rigs from
// ReadRigs, the town's settings/config.json decoded into Config, and a
// LoadSettings that reads each rig's settings/config.json the same way. An
// unreadable or unparseable registry or town config is an error, and no error
// carries the file's contents.
func Load(townRoot string, state func(rig string) string) (Input, error) {
	unparked, parked, err := ReadRigs(townRoot, state)
	if err != nil {
		return Input{}, err
	}
	cfg, err := readConfig(config.TownSettingsPath(townRoot))
	if err != nil {
		return Input{}, err
	}
	return Input{
		Unparked: unparked,
		Parked:   parked,
		Config:   cfg,
		LoadSettings: func(rig string) (map[string]any, error) {
			return readConfig(config.RigSettingsPath(filepath.Join(townRoot, rig)))
		},
	}, nil
}

// readConfig reads path as one JSON object. Its error names the path but never
// the bytes read, which can hold a secret.
func readConfig(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if doc == nil {
		return nil, fmt.Errorf("%s: not a JSON object", path)
	}
	return doc, nil
}
