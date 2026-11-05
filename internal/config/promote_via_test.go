package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestForgejoPromoteVia: merge_queue.forgejo.promote_via loads from a rig's
// settings, a rig-local value overrides the tier below it, and a value this
// binary does not implement is refused with the key named (gt-182zb).
func TestForgejoPromoteVia(t *testing.T) {
	t.Parallel()

	write := func(t *testing.T, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("writing settings: %v", err)
		}
		return path
	}

	t.Run("loads from a rig's settings", func(t *testing.T) {
		t.Parallel()
		path := write(t, `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{
			"promote_target":"git@github.com:example/app.git",
			"promote_key_file":"/home/gt/promote-app.key",
			"promote_via":"app-promote"}}}`)

		settings, err := LoadRigSettings(path)
		if err != nil {
			t.Fatalf("LoadRigSettings: %v", err)
		}
		if settings.MergeQueue == nil || settings.MergeQueue.Forgejo == nil {
			t.Fatal("the forgejo block did not load")
		}
		if got := settings.MergeQueue.Forgejo.PromoteVia; got != PromoteViaAppPromote {
			t.Errorf("PromoteVia = %q, want %q", got, PromoteViaAppPromote)
		}
	})

	t.Run("a rig-level value overrides the base", func(t *testing.T) {
		t.Parallel()
		base := &MergeQueueConfig{Forgejo: &ForgejoConfig{
			PromoteTarget: "git@github.com:example/app.git",
		}}
		local := &MergeQueueConfig{Forgejo: &ForgejoConfig{
			PromoteVia: PromoteViaAppPromote,
		}}

		merged := MergeSettingsCommand(base, local)
		if merged == nil || merged.Forgejo == nil {
			t.Fatal("MergeSettingsCommand dropped the forgejo block")
		}
		if got := merged.Forgejo.PromoteVia; got != PromoteViaAppPromote {
			t.Errorf("PromoteVia = %q, want the rig-local %q", got, PromoteViaAppPromote)
		}
		if got := merged.Forgejo.PromoteTarget; got != base.Forgejo.PromoteTarget {
			t.Errorf("PromoteTarget = %q, want the base target kept", got)
		}
	})

	t.Run("a base value survives a rig-level block that omits it", func(t *testing.T) {
		t.Parallel()
		base := &MergeQueueConfig{Forgejo: &ForgejoConfig{
			PromoteTarget: "git@github.com:example/app.git",
			PromoteVia:    PromoteViaAppPromote,
		}}
		local := &MergeQueueConfig{Forgejo: &ForgejoConfig{
			PromoteKeyFile: "/home/gt/promote-app.key",
		}}

		merged := MergeSettingsCommand(base, local)
		if merged == nil || merged.Forgejo == nil {
			t.Fatal("MergeSettingsCommand dropped the forgejo block")
		}
		if got := merged.Forgejo.PromoteVia; got != PromoteViaAppPromote {
			t.Errorf("PromoteVia = %q, want the base %q kept", got, PromoteViaAppPromote)
		}
		if got := merged.Forgejo.PromoteKeyFile; got != local.Forgejo.PromoteKeyFile {
			t.Errorf("PromoteKeyFile = %q, want the rig-local key file", got)
		}
	})

	t.Run("an unknown value is refused with the key named", func(t *testing.T) {
		t.Parallel()
		path := write(t, `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{
			"promote_via":"app_promote"}}}`)

		_, err := LoadRigSettings(path)
		if err == nil {
			t.Fatal("LoadRigSettings accepted promote_via app_promote; want a refusal")
		}
		if !strings.Contains(err.Error(), "merge_queue.forgejo.promote_via") {
			t.Errorf("err = %v, want it to name merge_queue.forgejo.promote_via", err)
		}
	})
}
