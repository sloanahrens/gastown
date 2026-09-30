package config

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestDecodeJSONFileRejectsUnknownKeysWithPathAndOffset(t *testing.T) {
	t.Parallel()
	data := []byte("{\n  \"type\": \"town-settings\",\n  \"role_agents\": {\"mayor\": \"x\"},\n  \"polecat_pool\": {\"max_local\": 1, \"bogus\": 2}\n}\n")
	err := DecodeJSONFile("/t/settings/config.json", data, &TownSettings{})
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("DecodeJSONFile = %v, want *ParseError", err)
	}
	if len(pe.Keys) != 1 || pe.Keys[0] != "polecat_pool.bogus" {
		t.Fatalf("Keys = %v, want [polecat_pool.bogus]", pe.Keys)
	}
	if pe.Line != 4 || pe.Column != 36 {
		t.Errorf("location = line %d column %d, want line 4 column 36", pe.Line, pe.Column)
	}
	msg := err.Error()
	for _, want := range []string{"/t/settings/config.json", "polecat_pool.bogus", "line 4"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if strings.Contains(msg, "\n") {
		t.Errorf("message must be one line: %q", msg)
	}
}

func TestDecodeJSONFileReportsEveryUnknownKey(t *testing.T) {
	t.Parallel()
	data := []byte(`{"zzz": 1, "agents": {"a": {"command": "c", "nope": true}}, "operational": {"daemon": {"x": 1}}}`)
	err := DecodeJSONFile("f.json", data, &TownSettings{})
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("got %v", err)
	}
	want := []string{"zzz", "agents.a.nope", "operational.daemon.x"}
	if strings.Join(pe.Keys, ",") != strings.Join(want, ",") {
		t.Fatalf("Keys = %v, want %v", pe.Keys, want)
	}
	if pe.Offset != 2 {
		t.Errorf("Offset = %d, want the first unknown key's offset 2", pe.Offset)
	}
}

func TestDecodeJSONFileMapKeysAndArraysAreWalked(t *testing.T) {
	t.Parallel()
	ok := []byte(`{"role_agents": {"anything": "x"}, "disabled_patrols": ["a"], "agents": {"n": {"args": ["--x"], "env": {"K": "V"}}}}`)
	if err := DecodeJSONFile("f.json", ok, &TownSettings{}); err != nil {
		t.Fatalf("valid file = %v", err)
	}
	var v struct {
		Items []struct {
			A int `json:"a"`
		} `json:"items"`
	}
	err := DecodeJSONFile("f.json", []byte(`{"items": [{"a": 1}, {"a": 2, "b": 3}]}`), &v)
	var pe *ParseError
	if !errors.As(err, &pe) || len(pe.Keys) != 1 || pe.Keys[0] != "items[1].b" {
		t.Fatalf("got %v (%+v), want items[1].b", err, pe)
	}
}

func TestDecodeJSONFileMatchesKeysLikeEncodingJSON(t *testing.T) {
	t.Parallel()
	// encoding/json assigns keys case-insensitively; the walker must agree or
	// it would reject a key the decoder actually used.
	if err := DecodeJSONFile("f.json", []byte(`{"Type": "town-settings", "VERSION": 1}`), &TownSettings{}); err != nil {
		t.Fatalf("case-folded keys = %v", err)
	}
	var v struct {
		Skip string `json:"-"`
		Keep string
	}
	if err := DecodeJSONFile("f.json", []byte(`{"Keep": "k"}`), &v); err != nil {
		t.Fatalf("untagged field = %v", err)
	}
	if err := DecodeJSONFile("f.json", []byte(`{"Skip": "s"}`), &v); !errors.Is(err, ErrUnparseable) {
		t.Fatalf(`json:"-" field accepted: %v`, err)
	}
}

func TestDecodeJSONFileEmbeddedStructFieldsAreKnown(t *testing.T) {
	t.Parallel()
	type inner struct {
		A int `json:"a"`
	}
	var v struct {
		inner
		B int `json:"b"`
	}
	if err := DecodeJSONFile("f.json", []byte(`{"a": 1, "b": 2}`), &v); err != nil {
		t.Fatalf("embedded field = %v", err)
	}
}

func TestDecodeJSONFileRawMessageAndInterfaceAreOpaque(t *testing.T) {
	t.Parallel()
	var v struct {
		Raw json.RawMessage `json:"raw"`
		Any any             `json:"any"`
	}
	if err := DecodeJSONFile("f.json", []byte(`{"raw": {"x": {"y": 1}}, "any": {"z": [1]}}`), &v); err != nil {
		t.Fatalf("opaque values = %v", err)
	}
}

func TestDecodeJSONFileRejectsTrailingData(t *testing.T) {
	t.Parallel()
	err := DecodeJSONFile("f.json", []byte("{}\n{}\n"), &TownSettings{})
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 2 {
		t.Fatalf("trailing value = %v (%+v), want a ParseError on line 2", err, pe)
	}
	if err := DecodeJSONFile("f.json", []byte("{}\n\n  \n"), &TownSettings{}); err != nil {
		t.Fatalf("trailing whitespace = %v", err)
	}
}

func TestDecodeYAMLFileRejectsUnknownKeys(t *testing.T) {
	t.Parallel()
	var v struct {
		Listener struct {
			Port int `yaml:"port"`
		} `yaml:"listener"`
	}
	if err := DecodeYAMLFile("c.yaml", []byte("listener:\n  port: 3307\n"), &v); err != nil || v.Listener.Port != 3307 {
		t.Fatalf("valid yaml = %v, port %d", err, v.Listener.Port)
	}
	err := DecodeYAMLFile("c.yaml", []byte("listener:\n  port: 3307\n  bogus: 1\n"), &v)
	var pe *ParseError
	if !errors.As(err, &pe) || pe.Line != 3 || pe.Path != "c.yaml" {
		t.Fatalf("unknown yaml key = %v (%+v), want ParseError at line 3", err, pe)
	}
	if !strings.Contains(err.Error(), "line 3") || strings.Contains(err.Error(), "\n") {
		t.Errorf("message = %q, want one line naming line 3", err.Error())
	}
	if err := DecodeYAMLFile("c.yaml", []byte("listener: [\n"), &v); !errors.Is(err, ErrUnparseable) {
		t.Fatalf("broken yaml = %v", err)
	}
}
