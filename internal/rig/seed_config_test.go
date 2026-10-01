package rig

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/constants"
)

type recordingConfigSetter struct {
	set  [][2]string
	fail map[string]error
}

func (r *recordingConfigSetter) ConfigSet(key, value string) error {
	r.set = append(r.set, [2]string{key, value})
	return r.fail[key]
}

// TestSeedRigDatabaseConfigGoesThroughBd (gt-fcxe9.12): rig add writes the
// rig database's config through bd (bd config set, and the issue_prefix
// seeder), never with REPLACE INTO config.
func TestSeedRigDatabaseConfigGoesThroughBd(t *testing.T) {
	t.Parallel()
	bd := &recordingConfigSetter{}
	var prefixSeeded string
	warnings := seedRigDatabaseConfig(bd, func(prefix string) error {
		prefixSeeded = prefix
		return nil
	}, "myr")
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v", warnings)
	}
	if prefixSeeded != "myr" {
		t.Errorf("issue_prefix seeded %q, want myr", prefixSeeded)
	}
	want := [][2]string{
		{"types.custom", constants.BeadsCustomTypes},
		{"types.infra", constants.BeadsInfraTypes},
		{"events-journal", "true"},
	}
	if !reflect.DeepEqual(bd.set, want) {
		t.Errorf("bd config set calls = %v, want %v", bd.set, want)
	}
}

// TestSeedRigDatabaseConfigReportsFailures: every failed write becomes a
// warning naming its key; none is dropped.
func TestSeedRigDatabaseConfigReportsFailures(t *testing.T) {
	t.Parallel()
	bd := &recordingConfigSetter{fail: map[string]error{"types.infra": errors.New("refused")}}
	warnings := seedRigDatabaseConfig(bd, func(string) error { return errors.New("no store") }, "myr")
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"issue_prefix", "no store", "types.infra", "refused"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings %q missing %q", joined, want)
		}
	}
}
