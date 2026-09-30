package config

import (
	"os"
	"path/filepath"
	"testing"
)

// liveTown holds scrubbed copies of the operator town's config files as they
// stood on 2026-09-29 (tokens, emails, usernames and home paths replaced).
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
