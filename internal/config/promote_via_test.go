package config

import (
	"os"
	"path/filepath"
	"strconv"
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
		// The rig-local tier accepts "default" as well as app-promote, so its
		// refusal must name both values an operator can use.
		if !strings.Contains(err.Error(), `"default"`) {
			t.Errorf("err = %v, want it to name the %q value the rig-local tier accepts", err, PromoteViaDefault)
		}
	})

	t.Run("a rig-level default loads from the rig's settings", func(t *testing.T) {
		t.Parallel()
		path := write(t, `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{
			"promote_via":"default"}}}`)

		if _, err := LoadRigSettings(path); err != nil {
			t.Fatalf("LoadRigSettings refused promote_via default: %v", err)
		}
	})

	// "default" is a clear-marker only the rig-local settings tier resolves.
	// Every other tier that reaches the shared validator - the town-floor
	// config.json through the merged value, and the repo-committed
	// .gastown/settings.json through its loader - must refuse it rather than
	// pass it to the promoter as an opt-out it never meant.
	repoPath := func(t *testing.T, promoteVia string) string {
		t.Helper()
		root := t.TempDir()
		dir := filepath.Join(root, ".gastown")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("creating .gastown: %v", err)
		}
		body := `{"type":"rig-settings","version":1,"merge_queue":{"forgejo":{"promote_via":` +
			strconv.Quote(promoteVia) + `}}}`
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(body), 0o644); err != nil {
			t.Fatalf("writing repo settings: %v", err)
		}
		return root
	}

	t.Run("the shared validator refuses a default outside the rig-local tier", func(t *testing.T) {
		t.Parallel()
		err := ValidateForgejoPromoteVia(PromoteViaDefault)
		if err == nil {
			t.Fatalf("ValidateForgejoPromoteVia accepted %q; a floor or merged value must be refused", PromoteViaDefault)
		}
		if !strings.Contains(err.Error(), PromoteViaDefault) || !strings.Contains(err.Error(), "rig-local settings/config.json") {
			t.Errorf("err = %v, want it to name %q and the rig-local tier that accepts it", err, PromoteViaDefault)
		}
	})

	t.Run("a repo-committed default is refused", func(t *testing.T) {
		t.Parallel()
		_, err := LoadRepoSettings(repoPath(t, PromoteViaDefault))
		if err == nil {
			t.Fatalf("LoadRepoSettings accepted promote_via %q at the repo tier", PromoteViaDefault)
		}
		if !strings.Contains(err.Error(), PromoteViaDefault) {
			t.Errorf("err = %v, want it to name %q", err, PromoteViaDefault)
		}
	})

	t.Run("a repo-committed app-promote is accepted", func(t *testing.T) {
		t.Parallel()
		if _, err := LoadRepoSettings(repoPath(t, PromoteViaAppPromote)); err != nil {
			t.Fatalf("LoadRepoSettings refused promote_via %q at the repo tier: %v", PromoteViaAppPromote, err)
		}
	})

	// A rig-level "default" clears the value it overrides, so the merged block
	// reads as the empty default. Merged through MergeSettingsCommand, the call
	// the rig resolver makes, rather than the helper.
	clearCases := []struct {
		name  string
		base  *ForgejoConfig
		local *ForgejoConfig
		want  string
	}{
		{
			name:  "clears an inherited app-promote",
			base:  &ForgejoConfig{PromoteTarget: "git@github.com:example/app.git", PromoteVia: PromoteViaAppPromote},
			local: &ForgejoConfig{PromoteVia: PromoteViaDefault},
			want:  "",
		},
		{
			name:  "with no base value stays the empty default",
			base:  &ForgejoConfig{PromoteTarget: "git@github.com:example/app.git"},
			local: &ForgejoConfig{PromoteVia: PromoteViaDefault},
			want:  "",
		},
	}
	for _, tc := range clearCases {
		t.Run("a rig-level default "+tc.name, func(t *testing.T) {
			t.Parallel()
			merged := MergeSettingsCommand(&MergeQueueConfig{Forgejo: tc.base}, &MergeQueueConfig{Forgejo: tc.local})
			if merged == nil || merged.Forgejo == nil {
				t.Fatal("MergeSettingsCommand dropped the forgejo block")
			}
			if got := merged.Forgejo.PromoteVia; got != tc.want {
				t.Errorf("PromoteVia = %q, want %q", got, tc.want)
			}
			if got := merged.Forgejo.PromoteTarget; got != tc.base.PromoteTarget {
				t.Errorf("PromoteTarget = %q, want the base target kept", got)
			}
		})
	}

	// The rig-root floor's "default" names nothing to clear. A rig-local block
	// that omits promote_via must leave it in place, not turn it into the
	// empty default, or the marker becomes an opt-out no tier asked for.
	t.Run("a rig-root default survives a rig-local block that omits promote_via", func(t *testing.T) {
		t.Parallel()
		base := &MergeQueueConfig{Forgejo: &ForgejoConfig{
			PromoteTarget: "git@github.com:example/app.git",
			PromoteVia:    PromoteViaDefault,
		}}
		local := &MergeQueueConfig{Forgejo: &ForgejoConfig{
			PromoteKeyFile: "/home/gt/promote-app.key",
		}}

		merged := MergeSettingsCommand(base, local)
		if merged == nil || merged.Forgejo == nil {
			t.Fatal("MergeSettingsCommand dropped the forgejo block")
		}
		if got := merged.Forgejo.PromoteVia; got != PromoteViaDefault {
			t.Errorf("PromoteVia = %q, want the base %q kept", got, PromoteViaDefault)
		}
	})
}
