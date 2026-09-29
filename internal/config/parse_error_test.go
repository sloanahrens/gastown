package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeJSONFileReportsPathAndOffset(t *testing.T) {
	t.Parallel()
	path := "/town/settings/config.json"
	data := []byte("{\n  \"type\": \"town-settings\",\n  \"version\": 1,\n}\n")
	var ts TownSettings
	err := DecodeJSONFile(path, data, &ts)
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("DecodeJSONFile = %v, want *ParseError", err)
	}
	if !errors.Is(err, ErrUnparseable) {
		t.Errorf("errors.Is(err, ErrUnparseable) = false")
	}
	if pe.Path != path || pe.Line != 4 || pe.Column != 1 || pe.Offset <= 0 {
		t.Errorf("ParseError = %+v, want path %s at line 4 column 1 with an offset", pe, path)
	}
	msg := err.Error()
	for _, want := range []string{path, "offset", "line 4", "never rewrite"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q does not contain %q", msg, want)
		}
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("message must be one line: %q", msg)
	}
}

func TestDecodeJSONFileTypeErrorCarriesOffset(t *testing.T) {
	t.Parallel()
	var ts TownSettings
	err := DecodeJSONFile("x.json", []byte(`{"version": "one"}`), &ts)
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Offset <= 0 || pe.Line != 1 {
		t.Fatalf("type error = %v (%+v), want a ParseError with an offset on line 1", err, pe)
	}
}

func TestDecodeJSONFileEmptyFileIsUnparseable(t *testing.T) {
	t.Parallel()
	var ts TownSettings
	if err := DecodeJSONFile("x.json", nil, &ts); !errors.Is(err, ErrUnparseable) {
		t.Fatalf("empty file = %v, want ErrUnparseable", err)
	}
}

func TestCheckJSONFileParses(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	var ts TownSettings
	if err := CheckJSONFileParses(filepath.Join(dir, "absent.json"), &ts); err != nil {
		t.Errorf("absent file = %v, want nil", err)
	}
	good := filepath.Join(dir, "good.json")
	writeFile(t, good, `{"type":"town-settings","version":1}`)
	if err := CheckJSONFileParses(good, &TownSettings{}); err != nil {
		t.Errorf("valid file = %v", err)
	}
	bad := filepath.Join(dir, "bad.json")
	writeFile(t, bad, `{"type":`)
	if err := CheckJSONFileParses(bad, &TownSettings{}); !errors.Is(err, ErrUnparseable) {
		t.Errorf("broken file = %v, want ErrUnparseable", err)
	}
}

// TestLoadOrCreateTownSettingsBrokenFileIsAParseError: absent and broken are
// different answers (G3-04). Absent gives defaults; broken names the offset.
func TestLoadOrCreateTownSettingsBrokenFileIsAParseError(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if ts, err := LoadOrCreateTownSettings(path); err != nil || ts == nil {
		t.Fatalf("absent = %v, %v; want defaults", ts, err)
	}
	writeFile(t, path, "{\"default_agent\": \"deepseek\",}")
	_, err := LoadOrCreateTownSettings(path)
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Path != path {
		t.Fatalf("broken = %v, want *ParseError naming %s", err, path)
	}
}

// TestSaversNeverReplaceAnUnparseableFile: a writer that could not read the
// file must not replace it with whatever it built from defaults (G3-03).
func TestSaversNeverReplaceAnUnparseableFile(t *testing.T) {
	t.Parallel()
	const broken = "{\"patrols\": {\"witness\": {\"enabled\": false},}}"
	for name, save := range map[string]func(path string) error{
		"SaveTownSettings":       func(p string) error { return SaveTownSettings(p, NewTownSettings()) },
		"SaveDaemonPatrolConfig": func(p string) error { return SaveDaemonPatrolConfig(p, NewDaemonPatrolConfig()) },
	} {
		path := filepath.Join(t.TempDir(), "file.json")
		writeFile(t, path, broken)
		if err := save(path); !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s over a broken file = %v, want ErrUnparseable", name, err)
		}
		if got, _ := os.ReadFile(path); string(got) != broken {
			t.Errorf("%s rewrote the broken file: %q", name, got)
		}
	}
}
