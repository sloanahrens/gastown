package townstate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
)

// RehearsalProfile rewrites the town settings for a rehearsal pod, where a
// seeded copy must start nothing the live daemon starts. Every patrol object
// directly under daemon.patrols is set to enabled false (a missing enabled
// becomes an explicit false), and daemon.supervisor is deleted. Every other key
// and value is left as it was: numbers are kept as their text, and keys come
// out sorted.
//
// changes names each key it changed, in patrol-name order with the supervisor
// last; a patrol that was already disabled is not a change. A settings file
// that is not valid JSON, has no daemon.patrols object, or has a patrol that is
// not an object is refused, with an error that names what is wrong.
func RehearsalProfile(settings []byte) (out []byte, changes []string, err error) {
	root, err := decodeSettings(settings)
	if err != nil {
		return nil, nil, err
	}
	daemon, _ := root["daemon"].(map[string]any)
	patrols, ok := daemon["patrols"].(map[string]any)
	if !ok {
		return nil, nil, errors.New("settings have no daemon.patrols object")
	}
	for _, name := range slices.Sorted(maps.Keys(patrols)) {
		patrol, ok := patrols[name].(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("daemon.patrols.%s is not an object", name)
		}
		if enabled, has := patrol["enabled"]; !has || enabled != false {
			patrol["enabled"] = false
			changes = append(changes, "daemon.patrols."+name+".enabled")
		}
	}
	if _, has := daemon["supervisor"]; has {
		delete(daemon, "supervisor")
		changes = append(changes, "daemon.supervisor")
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(root); err != nil {
		// Decoded JSON always encodes: every number is text the decoder
		// validated and no value can refer back to itself, so this is a bug.
		panic("encoding decoded settings: " + err.Error())
	}
	return buf.Bytes(), changes, nil
}

// decodeSettings reads one JSON object. Numbers stay as their text so the
// rewrite does not change them, and bytes after the object are refused.
func decodeSettings(settings []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(settings))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, fmt.Errorf("settings is not valid JSON: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("settings is not valid JSON: trailing data after the object")
	}
	return root, nil
}
