package config

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

type counterFile struct {
	N int `json:"n"`
}

func TestWriteConfigJSONRefusesAnUnparseableFile(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"syntax":      `{"n": 1,}`,
		"unknown key": `{"n": 1, "m": 2}`,
		"type":        `{"n": "one"}`,
	} {
		path := filepath.Join(t.TempDir(), "c.json")
		writeFile(t, path, body)
		if err := WriteConfigJSON(path, &counterFile{N: 9}, 0o600); !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s: WriteConfigJSON = %v, want ErrUnparseable", name, err)
		}
		err := UpdateConfigJSON(path, 0o600, func(c *counterFile, _ bool) error { c.N++; return nil })
		if !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s: UpdateConfigJSON = %v, want ErrUnparseable", name, err)
		}
		if got, _ := os.ReadFile(path); string(got) != body {
			t.Errorf("%s: file rewritten to %q", name, got)
		}
	}
}

func TestWriteConfigJSONCreatesAndKeepsMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sub", "c.json")
	if err := WriteConfigJSON(path, &counterFile{N: 1}, 0o640); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o640 {
		t.Errorf("new file mode = %v, want 0640", fi.Mode().Perm())
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteConfigJSON(path, &counterFile{N: 2}, 0o644); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("rewritten file mode = %v, want the existing 0600", fi.Mode().Perm())
	}
	var got counterFile
	data, _ := os.ReadFile(path)
	if err := DecodeJSONFile(path, data, &got); err != nil || got.N != 2 {
		t.Fatalf("read back %+v, %v", got, err)
	}
	if data[len(data)-1] != '\n' {
		t.Errorf("file does not end with a newline")
	}
	leftovers, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "c.json.tmp.*"))
	if len(leftovers) != 0 {
		t.Errorf("temp files left behind: %v", leftovers)
	}
}

func TestUpdateConfigJSONSerializesWriters(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.json")
	const writers = 20
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- UpdateConfigJSON(path, 0o600, func(c *counterFile, _ bool) error { c.N++; return nil })
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	var got counterFile
	data, _ := os.ReadFile(path)
	if err := DecodeJSONFile(path, data, &got); err != nil || got.N != writers {
		t.Fatalf("counter = %d (%v), want %d: an update was lost", got.N, err, writers)
	}
}

func TestUpdateConfigJSONReportsAbsence(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.json")
	var sawExists []bool
	mutate := func(c *counterFile, exists bool) error { sawExists = append(sawExists, exists); c.N = 5; return nil }
	for i := 0; i < 2; i++ {
		if err := UpdateConfigJSON(path, 0o600, mutate); err != nil {
			t.Fatal(err)
		}
	}
	if len(sawExists) != 2 || sawExists[0] || !sawExists[1] {
		t.Fatalf("exists = %v, want [false true]", sawExists)
	}
}

func TestUpdateConfigJSONMutateErrorWritesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "c.json")
	writeFile(t, path, `{"n": 1}`)
	stop := errors.New("stop")
	if err := UpdateConfigJSON(path, 0o600, func(c *counterFile, _ bool) error { c.N = 7; return stop }); !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
	if got, _ := os.ReadFile(path); string(got) != `{"n": 1}` {
		t.Errorf("file changed: %q", got)
	}
}

// TestSaveTownSettingsCreatesThePrivateMode: a new settings/config.json is
// 0600 because agent presets carry API tokens until gt-y3pgh.5.
func TestSaveTownSettingsCreatesThePrivateMode(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "settings", "config.json")
	if err := SaveTownSettings(path, NewTownSettings()); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("new settings file mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

// TestUpdateConfigJSONWritesOnlyWhatChanged: a rewrite must not write a
// struct's zero value for a key the file leaves out. The refinery reads
// merge_queue.enabled as a pointer, so "enabled": false where the key was
// absent would switch the merge queue off.
func TestUpdateConfigJSONWritesOnlyWhatChanged(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, `{"type":"rig","version":1,"name":"r","git_url":"x","created_at":"2026-01-01T00:00:00Z","merge_queue":{"test_command":"make test","batch_min_age":"5m"}}`)
	err := UpdateConfigJSON(path, 0o600, func(c *RigConfig, _ bool) error {
		c.DefaultBranch = "main"
		c.MergeQueue.TestCommand = "make test-fast"
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := jsonTree(t, path).(map[string]any)
	mq := got["merge_queue"].(map[string]any)
	if _, ok := mq["enabled"]; ok {
		t.Errorf("rewrite added merge_queue.enabled: %v", mq)
	}
	if mq["test_command"] != "make test-fast" || mq["batch_min_age"] != "5m" || got["default_branch"] != "main" {
		t.Errorf("changes not applied or untouched values lost: %v", got)
	}
	if _, ok := got["beads"]; ok {
		t.Errorf("rewrite added an absent object: %v", got)
	}
}

func TestUpdateConfigJSONRemovesWhatTheCallerCleared(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "config.json")
	writeFile(t, path, `{"type":"rig","version":1,"name":"r","git_url":"x","created_at":"2026-01-01T00:00:00Z","default_branch":"dev"}`)
	if err := UpdateConfigJSON(path, 0o600, func(c *RigConfig, _ bool) error { c.DefaultBranch = ""; return nil }); err != nil {
		t.Fatal(err)
	}
	if _, ok := jsonTree(t, path).(map[string]any)["default_branch"]; ok {
		t.Error("a cleared omitempty field stayed in the file")
	}
}
