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

// WriteSettings sets merge_queue.forgejo in the rig's settings/config.json,
// creating the file when absent and backing the existing one up to a dated .bak-
// sibling whenever it changes it (the undo), and refuses a file it cannot read
// or that is not a JSON object.
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

// ensureKeys returns the object doc with each default added at its end when the
// object has no such key. A key that is present is not touched, whatever it holds.
func ensureKeys(doc []byte, defaults []member) ([]byte, error) {
	members, err := parseObject(doc)
	if err != nil {
		return nil, err
	}
	for _, d := range defaults {
		if lastMember(members, d.key) < 0 {
			members = append(members, d)
		}
	}
	return renderMembers(members), nil
}

// renderMembers is members as one compact JSON object, in the order given, each
// value's text untouched.
func renderMembers(members []member) []byte {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for n, m := range members {
		if n > 0 {
			buf.WriteByte(',')
		}
		buf.Write(jsonString(m.key))
		buf.WriteByte(':')
		buf.Write(m.val)
	}
	buf.WriteByte('}')
	return buf.Bytes()
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
