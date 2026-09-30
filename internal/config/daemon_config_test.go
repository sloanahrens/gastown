package config

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestDaemonPatrolConfigIsTheDaemonsSchema(t *testing.T) {
	t.Parallel()
	data := []byte(`{"type":"daemon-patrol-config","version":1,
		"heartbeat":{"enabled":true,"interval":"3m"},
		"env":{"K":"V"},
		"patrols":{"dolt_server":{"enabled":true,"port":3307},
		           "scheduled_maintenance":{"enabled":true,"mode":"gc","gc_min_bytes":1},
		           "dolt_remotes":{"interval":"15m"}}}`)
	var cfg DaemonPatrolConfig
	if err := DecodeJSONFile("daemon.json", data, &cfg); err != nil {
		t.Fatalf("daemon.json with daemon-only patrols = %v", err)
	}
	if cfg.Patrols.DoltServer == nil || cfg.Patrols.DoltServer.Port != 3307 || cfg.Env["K"] != "V" {
		t.Fatalf("decoded %+v", cfg.Patrols)
	}
	if cfg.Patrols.Count() != 2 {
		t.Errorf("Count = %d, want 2 (retired dolt_remotes not counted)", cfg.Patrols.Count())
	}
	out, err := json.Marshal(&cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"dolt_remotes":{"interval":"15m"}`) {
		t.Errorf("rewrite dropped the retired key: %s", out)
	}
}

func TestDaemonPatrolConfigRejectsUnknownPatrolKeys(t *testing.T) {
	t.Parallel()
	for _, body := range []string{
		`{"patrols":{"witness":{"enabled":false,"bogus":1}}}`,
		`{"patrols":{"custom":{"enabled":true}}}`,
		`{"heartbeet":{"enabled":true}}`,
	} {
		err := DecodeJSONFile("daemon.json", []byte(body), &DaemonPatrolConfig{})
		if !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s = %v, want ErrUnparseable", body, err)
		}
	}
}

func TestDaemonPatrolRigEditorsRefuseAnUnparseableFile(t *testing.T) {
	t.Parallel()
	const broken = `{"patrols": {"witness": {"enabled": true, "rigs": ["a"]}, "bogus": {}}}`
	for name, edit := range map[string]func(string) error{
		"add":    func(root string) error { return AddRigToDaemonPatrols(root, "b") },
		"remove": func(root string) error { return RemoveRigFromDaemonPatrols(root, "a") },
		"ensure": EnsureDaemonPatrolConfig,
	} {
		root := t.TempDir()
		path := DaemonPatrolConfigPath(root)
		writeFile(t, path, broken)
		if err := edit(root); !errors.Is(err, ErrUnparseable) {
			t.Errorf("%s over a broken daemon.json = %v, want ErrUnparseable", name, err)
		}
		if got, _ := os.ReadFile(path); string(got) != broken {
			t.Errorf("%s rewrote the broken file: %q", name, got)
		}
	}
}

func TestDaemonPatrolRigEditorsLeaveAMissingFileMissing(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := AddRigToDaemonPatrols(root, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(DaemonPatrolConfigPath(root)); !os.IsNotExist(err) {
		t.Fatalf("AddRigToDaemonPatrols created daemon.json: %v", err)
	}
}
