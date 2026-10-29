package rig

import (
	"errors"
	"os"
	"path/filepath"
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

// writeContainerHostTown writes a town whose Dolt block names a container host
// beside the local one, and returns its root.
func writeContainerHostTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	town := `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z",` +
		`"dolt":{"host":"127.0.0.2","container_host":"host.docker.internal","port":5507}}`
	if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "mayor", "town.json"), []byte(town), 0o600); err != nil {
		t.Fatal(err)
	}
	return root
}

// A rig's config.yaml is shared with the Mac through the town bind mount, so the
// endpoint stamped into it is the one a non-container process resolves
// (dolt.host), never dolt.container_host, which only resolves inside the
// container (gt-qpeei.17). The container resolution is the control: it names
// the other host for the same town.
func TestPersistedDoltEndpointIsTheNonContainerEndpoint(t *testing.T) {
	t.Parallel()
	root := writeContainerHostTown(t)

	control, ok := config.ResolveDoltEndpointFor(root, true)
	if !ok || control.Host != "host.docker.internal" {
		t.Fatalf("the container resolution = %+v (ok %v), want the container host: the control for this test", control, ok)
	}
	got, ok := persistedDoltEndpoint(root)
	if !ok || got.Host != "127.0.0.2" || got.Port != 5507 {
		t.Errorf("persistedDoltEndpoint = %+v (ok %v), want dolt.host 127.0.0.2 and port 5507", got, ok)
	}
}
