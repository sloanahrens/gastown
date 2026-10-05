package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// liveTown holds scrubbed copies of the operator town's config files as they
// stood on 2026-09-29 (tokens, emails, usernames and home paths replaced;
// mayor/daemon.json as of 2026-10-01, which holds none of those; town.json's
// dolt endpoint and a rig park record in rigs.json added 2026-10-01 with
// fake values, matching the live files' key shapes), less
// the patrols.quota_resume key gt-638go.2 deleted from the schema and the
// merge_queue.post_merge_command/post_merge_timeout keys gt-6zf1o deleted and
// the refinery-era merge_queue keys gt-5nlvq deleted (batch_*,
// test_verify_command, cycle_session_after_merge) and merge_queue.gate gt-5rt46
// deleted with its last reader and the 41 dead operational
// keys gt-y3pgh.2.13.1 deleted (operational.witness.done_intent_*,
// operational.daemon.boot_spawn_cooldown and the rest) and the mail:mayor
// route action gt-rwp7z.6 retired from the defaults: a
// live file must lose such a key before a binary carrying the deletion is
// installed, or the startup gate refuses the town.
// Strict decoding must accept every key they carry: a kernel that refused the
// live town would stop it the moment it shipped.
const liveTown = "testdata/livetown"

func TestLiveTownFilesDecodeStrictly(t *testing.T) {
	t.Parallel()
	cases := []struct {
		file string
		into func() any
	}{
		{"mayor/town.json", func() any { return &TownConfig{} }},
		{"mayor/rigs.json", func() any { return &RigsConfig{} }},
		{"mayor/daemon.json", func() any { return &DaemonPatrolConfig{} }},
		{"mayor/overseer.json", func() any { return &OverseerConfig{} }},
		{"settings/config.json", func() any { return &TownSettings{} }},
		{"settings/escalation.json", func() any { return &EscalationConfig{} }},
	}
	for _, rig := range []string{"gastown", "beads", "om"} {
		cases = append(cases, struct {
			file string
			into func() any
		}{"rigs/" + rig + "/config.json", func() any { return &RigConfig{} }})
	}
	for _, rig := range []string{"gastown", "beads", "om", "hm", "mango"} {
		cases = append(cases, struct {
			file string
			into func() any
		}{"rigs/" + rig + "/settings/config.json", func() any { return &RigSettings{} }})
	}
	for _, tc := range cases {
		path := filepath.Join(liveTown, tc.file)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := DecodeJSONFile(path, data, tc.into()); err != nil {
			t.Errorf("live %s: %v", tc.file, err)
		}
	}
}

// TestLiveTownFilesRoundTripThroughTheWriter: the writer decodes into the
// schema type and re-marshals it. For every live file that must lose
// nothing: the rewritten file decodes to the same JSON tree as the original.
func TestLiveTownFilesRoundTripThroughTheWriter(t *testing.T) {
	t.Parallel()
	roundTrip := map[string]func(src, dst string) error{
		"mayor/town.json":          func(s, d string) error { return copyThenUpdate[TownConfig](t, s, d) },
		"mayor/rigs.json":          func(s, d string) error { return copyThenUpdate[RigsConfig](t, s, d) },
		"mayor/daemon.json":        func(s, d string) error { return copyThenUpdate[DaemonPatrolConfig](t, s, d) },
		"mayor/overseer.json":      func(s, d string) error { return copyThenUpdate[OverseerConfig](t, s, d) },
		"settings/config.json":     func(s, d string) error { return copyThenUpdate[TownSettings](t, s, d) },
		"settings/escalation.json": func(s, d string) error { return copyThenUpdate[EscalationConfig](t, s, d) },
		"rigs/gastown/config.json": func(s, d string) error { return copyThenUpdate[RigConfig](t, s, d) },
		"rigs/gastown/settings/config.json": func(s, d string) error {
			return copyThenUpdate[RigSettings](t, s, d)
		},
	}
	for file, rt := range roundTrip {
		src := filepath.Join(liveTown, file)
		dst := filepath.Join(t.TempDir(), filepath.Base(file))
		if err := rt(src, dst); err != nil {
			t.Errorf("%s: %v", file, err)
			continue
		}
		before, after := jsonTree(t, src), jsonTree(t, dst)
		if !reflect.DeepEqual(before, after) {
			b, _ := json.Marshal(before)
			a, _ := json.Marshal(after)
			t.Errorf("%s changed on rewrite:\nbefore %s\nafter  %s", file, b, a)
		}
	}
}

func copyThenUpdate[T any](t *testing.T, src, dst string) error {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	if err := os.WriteFile(dst, data, 0o600); err != nil {
		return err
	}
	return UpdateConfigJSON(dst, 0o600, func(*T, bool) error { return nil })
}

func jsonTree(t *testing.T, path string) any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatal(err)
	}
	return v
}
