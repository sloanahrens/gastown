package rig

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
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

// rig add stamps the town's endpoint into the rig's config.yaml through bd
// config set, and nothing for a town without one (gt-y3pgh.3).
func TestStampRigDoltEndpoint(t *testing.T) {
	t.Parallel()
	bd := &recordingConfigSetter{fail: map[string]error{"dolt.host": errors.New("boom")}}
	warnings := stampRigDoltEndpoint(bd, config.DoltEndpoint{Host: "127.0.0.2", Port: 5507}, true)
	want := [][2]string{{"dolt.port", "5507"}, {"dolt.host", "127.0.0.2"}}
	if !reflect.DeepEqual(bd.set, want) {
		t.Errorf("bd config set calls = %v, want %v", bd.set, want)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "dolt.host") {
		t.Errorf("warnings = %v, want one naming dolt.host", warnings)
	}

	none := &recordingConfigSetter{}
	if w := stampRigDoltEndpoint(none, config.DoltEndpoint{}, false); len(w) != 0 || len(none.set) != 0 {
		t.Errorf("no endpoint: set %v, warnings %v; want nothing", none.set, w)
	}
}
