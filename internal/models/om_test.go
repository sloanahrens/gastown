package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// A town with no reviewer config has no backend to keep; a file that does not
// parse is the operator's to fix, not an empty config.
func TestLoadOMConfigReadsTheReviewersConfig(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	c, err := LoadOMConfig(filepath.Join(dir, "config.json"))
	if err != nil {
		t.Fatalf("LoadOMConfig of a missing file: %v", err)
	}
	if c.Loaded() || len(c.Backend) != 0 {
		t.Errorf("a missing config = %+v, want nothing", c)
	}

	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte(`{"backend":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOMConfig(bad); err == nil {
		t.Error("LoadOMConfig of a file that does not parse = no error, want one")
	}

	good := filepath.Join(dir, "good.json")
	if err := os.WriteFile(good, []byte(`{"backend":["/x/claude","-p","--model","sonnet"],"threshold":0.7,"depth":"standard","timeout":900}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err = LoadOMConfig(good)
	if err != nil {
		t.Fatalf("LoadOMConfig: %v", err)
	}
	if !c.Loaded() || len(c.Backend) != 4 || c.Depth != "standard" || c.Timeout != 900 || c.Threshold == nil || *c.Threshold != 0.7 {
		t.Errorf("config = %+v, want the file's values", c)
	}
}

// A switch owns the backend and nothing else: the depth, the threshold and a key
// this gt does not know survive it (gt-jaagc).
func TestOMSaveKeepsTheKeysItDidNotOwn(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "om", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	body := `{"backend":["/x/claude","-p","--model","sonnet"],"threshold":0.7,"depth":"standard","timeout":900,"future":{"kept":true}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := LoadOMConfig(path)
	if err != nil {
		t.Fatalf("LoadOMConfig: %v", err)
	}
	if err := c.WithBackend([]string{"/x/claude", "-p", "--model", "haiku"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	var doc map[string]any
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("the written config does not parse: %v\n%s", err, b)
	}
	if doc["threshold"] != 0.7 || doc["depth"] != "standard" || doc["timeout"] != float64(900) {
		t.Errorf("the settings beside the backend were changed:\n%s", b)
	}
	if _, ok := doc["future"]; !ok {
		t.Errorf("a key this gt does not know was dropped:\n%s", b)
	}
	backend, _ := doc["backend"].([]any)
	if len(backend) != 4 || backend[3] != "haiku" {
		t.Errorf("backend = %v, want the model swapped to haiku", backend)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want the file's own 0600 kept", fi.Mode().Perm())
	}
}

// A reviewer config written where there was none is private to the user.
func TestOMSaveCreatesAPrivateConfig(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "om", "config.json")
	if err := (OMConfig{}).WithBackend([]string{"claude", "-p", "--model", "haiku"}).Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	c, err := LoadOMConfig(path)
	if err != nil {
		t.Fatalf("LoadOMConfig: %v", err)
	}
	if len(c.Backend) != 4 || c.Backend[3] != "haiku" {
		t.Errorf("backend = %q, want the one written", c.Backend)
	}
}
