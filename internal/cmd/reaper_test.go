package cmd

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestReaperDatabaseNamesTrimsConfiguredList(t *testing.T) {
	oldDB := reaperDB
	t.Cleanup(func() { reaperDB = oldDB })

	reaperDB = " hq, gastown ,, beads "
	got := reaperDatabaseNames()
	want := []string{"hq", "gastown", "beads"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reaperDatabaseNames() = %#v, want %#v", got, want)
	}
}

// TestReaperAutoClosePreviewFlag pins the flag that carries the dry run's
// authorization (gt-39bu). `gt reaper run` composes its own preview in-process,
// so the flag belongs to auto-close alone; a bare `gt reaper auto-close` has to
// be able to refuse for want of it.
func TestReaperAutoClosePreviewFlag(t *testing.T) {
	flag := reaperAutoCloseCmd.Flags().Lookup("preview")
	if flag == nil {
		t.Fatal("gt reaper auto-close has no --preview flag: a live run could not be bound to a dry run")
	}
	if flag.DefValue != "" {
		t.Errorf("--preview default = %q, want empty: a default would authorize every live run", flag.DefValue)
	}
	if runFlag := reaperRunCmd.Flags().Lookup("preview"); runFlag != nil {
		t.Error("gt reaper run takes --preview, but it previews in-process before writing and should not accept a caller's hash")
	}
}

func TestWaitBeforeReaperDatabase(t *testing.T) {
	oldDelay := reaperDBDelay
	t.Cleanup(func() { reaperDBDelay = oldDelay })

	reaperDBDelay = "0s"
	if err := waitBeforeReaperDatabase(0); err != nil {
		t.Fatalf("first database wait returned error: %v", err)
	}
	if err := waitBeforeReaperDatabase(1); err != nil {
		t.Fatalf("zero-delay wait returned error: %v", err)
	}

	reaperDBDelay = "not-a-duration"
	if err := waitBeforeReaperDatabase(1); err == nil {
		t.Fatal("invalid delay should return an error")
	}
}

func TestDefaultReaperEndpointIgnoresStaleBeadsAliases(t *testing.T) {
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("GT_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	host, port := defaultReaperEndpoint()
	if host != "127.0.0.1" || port != 3307 {
		t.Fatalf("defaultReaperEndpoint() = %s:%d, want 127.0.0.1:3307", host, port)
	}
}

func TestDefaultReaperEndpointUsesTownConfig(t *testing.T) {
	townRoot := t.TempDir()
	mayorDir := filepath.Join(townRoot, "mayor")
	if err := os.MkdirAll(mayorDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mayorDir, "town.json"), []byte(`{"name":"test-town"}`), 0644); err != nil {
		t.Fatal(err)
	}
	doltDataDir := filepath.Join(townRoot, ".dolt-data")
	if err := os.MkdirAll(doltDataDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(doltDataDir, "config.yaml"), []byte("listener:\n  host: 127.0.0.2\n  port: 5507\n"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(townRoot)
	t.Setenv("GT_DOLT_IGNORE_CONFIG", "")
	t.Setenv("GT_DOLT_HOST", "")
	t.Setenv("GT_DOLT_PORT", "")
	t.Setenv("BEADS_DOLT_SERVER_HOST", "stale-host")
	t.Setenv("BEADS_DOLT_SERVER_PORT", "9999")
	t.Setenv("BEADS_DOLT_PORT", "9999")

	host, port := defaultReaperEndpoint()
	if host != "127.0.0.2" || port != 5507 {
		t.Fatalf("defaultReaperEndpoint() = %s:%d, want 127.0.0.2:5507", host, port)
	}
}
