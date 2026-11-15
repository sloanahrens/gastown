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
