package forgejocut

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"reflect"
)

// settingsType and settingsVersion are what a rig settings file written from
// nothing carries, as config.RigSettings reads them.
const (
	settingsType    = "rig-settings"
	settingsVersion = 1
)

// forgejoBlock is the merge_queue.forgejo block the landing worker reads, as the
// script wrote it: the Forgejo URL, the gate workflow, one bot login per role, and
// the promotion target with its key file. The keys are the json tags of
// config.ForgejoConfig, which a test decodes the block through.
func (c *Cutover) forgejoBlock() json.RawMessage {
	o := c.Opts
	var buf bytes.Buffer
	buf.WriteString(`{"remote_url":`)
	buf.Write(jsonString(o.ForgejoURL))
	buf.WriteString(`,"gate_workflow":`)
	buf.Write(jsonString(o.GateWorkflow))
	buf.WriteString(`,"bots":{`)
	for i, role := range botRoles {
		if i > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonString(role))
		buf.WriteByte(':')
		buf.Write(jsonString(o.BotPrefix + role))
	}
	buf.WriteString(`},"promote_target":`)
	buf.Write(jsonString(o.PromoteTarget))
	buf.WriteString(`,"promote_key_file":`)
	buf.Write(jsonString(o.PromoteKey))
	buf.WriteByte('}')
	return buf.Bytes()
}

// WriteSettings is step 6: the rig's settings/config.json gets merge_queue.forgejo
// set to the block above, and every other key stays where it was. A file that does
// not exist yet is created with its type and version. A block that already means
// the same thing (the comparison is on the parsed block, so key order and layout
// never force a rewrite) leaves the file untouched and makes no backup; otherwise
// the file is copied to a dated .bak- sibling first, and the undo is copying that
// backup back over config.json. A dry run logs the backup and the write it would
// make and changes nothing.
//
// This changes the file the daemon reads for the landing path, and the daemon
// reads a rig's block at start: the live run is a deliberate step after the
// promote key exists and the first L3 checks have passed, never from a landing.
func (c *Cutover) WriteSettings() error {
	o := c.Opts
	path := filepath.Join(o.RigRoot, "settings", "config.json")
	doc, err := c.files().ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("could not read %s: %w", path, err)
	}
	if !exists {
		doc = []byte("{}")
	}
	block := c.forgejoBlock()
	edited, have, found, err := withForgejoBlock(doc, block)
	if err != nil {
		return fmt.Errorf("%s is not usable: %w", path, err)
	}
	if found && sameJSON(have, block) {
		c.logf("%s: merge_queue.forgejo already matches", path)
		return nil
	}
	if err := c.BackupFile(path); err != nil {
		return err
	}
	if o.DryRun {
		c.logf("would write merge_queue.forgejo to %s", path)
		return nil
	}
	if err := c.replaceFile(path, edited); err != nil {
		return err
	}
	c.logf("wrote merge_queue.forgejo to %s", path)
	return nil
}

// withForgejoBlock returns doc with merge_queue.forgejo set to block, and the
// block the document held before, if it held one. The type and version keys are
// added when absent and an absent merge_queue is made, each at the end of its
// object; nothing present is moved.
func withForgejoBlock(doc []byte, block json.RawMessage) (edited []byte, had json.RawMessage, found bool, err error) {
	path := []string{"merge_queue", "forgejo"}
	doc, err = ensureKeys(doc, []member{
		{"type", jsonString(settingsType)},
		{"version", json.RawMessage(fmt.Sprint(settingsVersion))},
		{"merge_queue", json.RawMessage("{}")},
	})
	if err != nil {
		return nil, nil, false, err
	}
	had, found, err = jsonGet(doc, path)
	if err != nil {
		return nil, nil, false, err
	}
	edited, err = jsonSet(doc, path, block)
	if err != nil {
		return nil, nil, false, err
	}
	return edited, had, found, nil
}

// sameJSON reports whether two JSON values parse to the same data, whatever their
// key order or layout. A value that is not JSON equals nothing.
func sameJSON(a, b json.RawMessage) bool {
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}
