package daemon

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/attention"
)

// fakeProcessEnv is the process environment the daemon.env keys land in.
type fakeProcessEnv struct {
	vars map[string]string
}

func (f *fakeProcessEnv) seams() daemonEnvSeams {
	return daemonEnvSeams{
		lookup: func(key string) (string, bool) {
			v, ok := f.vars[key]
			return v, ok
		},
		set: func(key, value string) { f.vars[key] = value },
	}
}

// logCapture collects what applyDaemonEnv logs, formatted.
type logCapture struct {
	lines []string
}

func (c *logCapture) logf(format string, args ...any) {
	c.lines = append(c.lines, fmt.Sprintf(format, args...))
}

func (c *logCapture) joined() string {
	return strings.Join(c.lines, "\n")
}

const (
	testSettingsSource = "settings/config.json"
	testPodValue       = "/var/run/secrets/gt-hm/hm-config.json"
	testStaleValue     = "/run/gt/xdg/gt/hm-config.json"
)

// A container-mode key the process already sets to another value is kept as
// the pod has it, and the report names the key and never the values.
func TestDaemonEnvShadowedKeyIsKeptAndNamed(t *testing.T) {
	t.Parallel()
	proc := &fakeProcessEnv{vars: map[string]string{"HM_CONFIG": testPodValue}}
	var logs logCapture

	shadowed := applyDaemonEnv(map[string]string{"HM_CONFIG": testStaleValue}, testSettingsSource, true, proc.seams(), logs.logf)

	if got := proc.vars["HM_CONFIG"]; got != testPodValue {
		t.Errorf("HM_CONFIG = %q, want the pod's value %q kept", got, testPodValue)
	}
	if !slices.Equal(shadowed, []string{"HM_CONFIG"}) {
		t.Errorf("shadowed keys = %v, want [HM_CONFIG]", shadowed)
	}
	report := logs.joined()
	if !strings.Contains(report, "HM_CONFIG") || !strings.Contains(report, testSettingsSource) {
		t.Errorf("the report must name the key and its source, got:\n%s", report)
	}
	for _, value := range []string{testPodValue, testStaleValue} {
		if strings.Contains(report, value) {
			t.Errorf("the report leaks a value %q, got:\n%s", value, report)
		}
	}
}

// A key the process does not set is applied, and a key it already sets to the
// same value is applied too; neither is reported as shadowed.
func TestDaemonEnvUnsetAndEqualKeysAreApplied(t *testing.T) {
	t.Parallel()
	proc := &fakeProcessEnv{vars: map[string]string{"SAME_KEY": "same-value"}}
	var logs logCapture

	shadowed := applyDaemonEnv(map[string]string{
		"UNSET_KEY": "unset-value",
		"SAME_KEY":  "same-value",
	}, testSettingsSource, true, proc.seams(), logs.logf)

	if got := proc.vars["UNSET_KEY"]; got != "unset-value" {
		t.Errorf("UNSET_KEY = %q, want it applied", got)
	}
	if got := proc.vars["SAME_KEY"]; got != "same-value" {
		t.Errorf("SAME_KEY = %q, want it applied", got)
	}
	if len(shadowed) != 0 {
		t.Errorf("shadowed keys = %v, want none", shadowed)
	}
	if strings.Contains(logs.joined(), "already sets") {
		t.Errorf("an unset or equal key must not be reported as shadowed, got:\n%s", logs.joined())
	}
}

// Outside container mode nothing is shadowed: a differing value is applied as
// before, and the process environment is not consulted.
func TestDaemonEnvOutsideContainerAppliesEveryKey(t *testing.T) {
	t.Parallel()
	proc := &fakeProcessEnv{vars: map[string]string{"HM_CONFIG": testPodValue}}
	var logs logCapture

	shadowed := applyDaemonEnv(map[string]string{"HM_CONFIG": testStaleValue}, testSettingsSource, false, proc.seams(), logs.logf)

	if got := proc.vars["HM_CONFIG"]; got != testStaleValue {
		t.Errorf("HM_CONFIG = %q, want the settings value applied outside container mode", got)
	}
	if len(shadowed) != 0 {
		t.Errorf("shadowed keys = %v, want none outside container mode", shadowed)
	}
}

// The shadowed keys become one attention item that names each key and no value,
// reached through the collector list the daemon queue runs.
func TestEnvShadowRaisesOneItemNamingKeys(t *testing.T) {
	t.Parallel()
	s := &attentionSources{envShadowed: []string{"HM_CONFIG", "OTHER_KEY"}}
	c := collectorFor(t, s, attention.KindConfigEnvShadowed)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("items = %d, want exactly one", len(items))
	}
	if got := attention.RuleFor(items[0]); got != attention.RuleConfigEnvShadowed {
		t.Errorf("rule = %q, want %q", got, attention.RuleConfigEnvShadowed)
	}
	for _, key := range []string{"HM_CONFIG", "OTHER_KEY"} {
		if !strings.Contains(items[0].Summary, key) {
			t.Errorf("summary %q does not name %s", items[0].Summary, key)
		}
	}
}

// With no shadowed key the collector raises nothing, so an item clears once the
// settings file no longer holds the stale entry.
func TestEnvShadowRaisesNothingWhenNoKeyIsShadowed(t *testing.T) {
	t.Parallel()
	s := &attentionSources{}
	c := collectorFor(t, s, attention.KindConfigEnvShadowed)

	items, err := c.collect(context.Background())
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(items) != 0 {
		t.Errorf("items = %v, want none", items)
	}
}

// collectorFor returns the registered collector of kind, so the test drives the
// same entry the queue runs rather than a copy of its function.
func collectorFor(t *testing.T, s *attentionSources, kind attention.Kind) attentionCollector {
	t.Helper()
	for _, c := range s.collectors() {
		if c.kind == kind {
			return c
		}
	}
	t.Fatalf("no collector registered for kind %s", kind)
	return attentionCollector{}
}
