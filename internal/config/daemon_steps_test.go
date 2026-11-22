package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// TestEveryPatrolKeyIsClassified is the audit table's guard (gt-01lcq2): every
// patrol key PatrolsConfig declares is a row of daemonSteps, so a patrol cannot
// register without the table classifying where it runs. A retired key is
// skipped: it is raw JSON kept only so an old file decodes.
func TestEveryPatrolKeyIsClassified(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(PatrolsConfig{})
	var keys []string
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" || f.Type == reflect.TypeOf(json.RawMessage(nil)) {
			continue
		}
		keys = append(keys, name)
	}
	if len(keys) == 0 {
		t.Fatal("PatrolsConfig declares no patrol key, so nothing is checked")
	}
	for _, name := range keys {
		if !DaemonStepNamed(name, DaemonStepPatrol) {
			t.Errorf("%s: a patrol key PatrolsConfig declares that daemonSteps does not classify", name)
		}
	}
}

// TestDaemonStepsHMSync is the audit's hm-sync plugin row (gt-emcaqo): the pod
// runs the plugin, so the row is runs-in-container and names the programs the
// plugin execs, each of which the daemon image carries.
func TestDaemonStepsHMSync(t *testing.T) {
	t.Parallel()
	var row *DaemonStep
	for _, s := range DaemonSteps() {
		if s.Name == "hm-sync" && s.Kind == DaemonStepPlugin {
			s := s
			row = &s
		}
	}
	if row == nil {
		t.Fatal("daemonSteps has no hm-sync plugin row")
	}
	tests := []struct {
		field string
		got   any
		want  any
	}{
		{"class", row.Class, DaemonStepRuns},
		{"needs", row.Needs, []string{"hm", "git", "bd"}},
	}
	for _, tt := range tests {
		if !reflect.DeepEqual(tt.got, tt.want) {
			t.Errorf("hm-sync %s = %v, want %v", tt.field, tt.got, tt.want)
		}
	}
}
