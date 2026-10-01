package rig

import (
	"fmt"
	"strconv"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/constants"
)

// configSetter is the bd verb rig add seeds database config through.
type configSetter interface {
	ConfigSet(key, value string) error
}

// seedRigDatabaseConfig writes the new rig database's config through bd,
// never with SQL against bd's config table (gt-fcxe9.12, ADR 0001).
// issue_prefix goes through setPrefix because bd refuses
// "bd config set issue_prefix"; the custom and infra types go through
// "bd config set". Each failure comes back as a warning naming its key.
// The events journal is a config.yaml-only key, so AddRig writes it with the
// other config.yaml values, never through bd here: in a rig whose repo tracks
// .beads, "bd config set" would dirty the tracked config.yaml (gt-7iwy0.7).
func seedRigDatabaseConfig(bd configSetter, setPrefix func(prefix string) error, prefix string) []string {
	var warnings []string
	if err := setPrefix(prefix); err != nil {
		warnings = append(warnings, fmt.Sprintf("could not set issue_prefix in rig database: %v", err))
	}
	for _, kv := range [][2]string{
		{"types.custom", constants.BeadsCustomTypes},
		{"types.infra", constants.BeadsInfraTypes},
	} {
		if err := bd.ConfigSet(kv[0], kv[1]); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not set %s in rig database: %v", kv[0], err))
		}
	}
	return warnings
}

// stampRigDoltEndpoint writes the town's Dolt endpoint into a new rig's
// .beads/config.yaml through bd config set (gt-y3pgh.3, D1), so bd finds
// the server from the rig's own config without gt's environment. A town
// without an endpoint stamps nothing. Each failure comes back as a warning
// naming its key.
func stampRigDoltEndpoint(bd configSetter, ep config.DoltEndpoint, ok bool) []string {
	if !ok {
		return nil
	}
	kvs := [][2]string{{"dolt.port", strconv.Itoa(ep.Port)}}
	if ep.Host != "" {
		kvs = append(kvs, [2]string{"dolt.host", ep.Host})
	}
	var warnings []string
	for _, kv := range kvs {
		if err := bd.ConfigSet(kv[0], kv[1]); err != nil {
			warnings = append(warnings, fmt.Sprintf("could not set %s in config.yaml: %v", kv[0], err))
		}
	}
	return warnings
}
