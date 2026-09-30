package doctor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const brokenDaemonJSONForDoctor = "{\"patrols\": {\"dog\": {\"enabled\": false},}}"

func writeDoctorTownFile(t *testing.T, town, rel, body string) string {
	t.Helper()
	p := filepath.Join(town, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTownConfigParseCheck(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	check := NewTownConfigParseCheck()
	if check.CanFix() {
		t.Error("an unparseable file is fixed by hand; the check must not offer --fix")
	}
	if r := check.Run(&CheckContext{TownRoot: town}); r.Status != StatusOK {
		t.Fatalf("no files: %s %s", r.Status, r.Message)
	}
	settings := writeDoctorTownFile(t, town, "settings/config.json", "{\"default_agent\": \"x\",}")
	r := check.Run(&CheckContext{TownRoot: town})
	if r.Status != StatusError {
		t.Fatalf("broken settings: status %s, want error", r.Status)
	}
	all := r.Message + " " + strings.Join(r.Details, " ")
	if !strings.Contains(all, settings) || !strings.Contains(all, "offset") {
		t.Errorf("result %q / %v must name %s and the offset", r.Message, r.Details, settings)
	}
}

// TestLifecycleDefaultsCheck_UnparseableConfig: a broken daemon.json is not
// "not found", and --fix must not replace it with defaults (G3-03).
func TestLifecycleDefaultsCheck_UnparseableConfig(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	path := writeDoctorTownFile(t, town, "mayor/daemon.json", brokenDaemonJSONForDoctor)
	check := NewLifecycleDefaultsCheck()
	ctx := &CheckContext{TownRoot: town}
	r := check.Run(ctx)
	if r.Status != StatusError || !strings.Contains(r.Message, "does not parse") {
		t.Fatalf("Run = %s %q, want an error saying the file does not parse", r.Status, r.Message)
	}
	if err := check.Fix(ctx); err == nil {
		t.Error("Fix on an unparseable daemon.json must fail")
	}
	if got, _ := os.ReadFile(path); string(got) != brokenDaemonJSONForDoctor {
		t.Fatalf("doctor fix rewrote the broken daemon.json: %q", got)
	}
}

// TestTownConfigParseCheckReportsEachBrokenFile: with both files broken the
// check lists one detail per file, each naming its path.
func TestTownConfigParseCheckReportsEachBrokenFile(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	settings := writeDoctorTownFile(t, town, "settings/config.json", "{\"default_agent\": \"x\",}")
	daemonJSON := writeDoctorTownFile(t, town, "mayor/daemon.json", brokenDaemonJSONForDoctor)
	r := NewTownConfigParseCheck().Run(&CheckContext{TownRoot: town})
	if r.Status != StatusError || len(r.Details) != 2 {
		t.Fatalf("both broken: status %s, details %v; want error with 2 details", r.Status, r.Details)
	}
	joined := strings.Join(r.Details, "\n")
	for _, p := range []string{settings, daemonJSON} {
		if !strings.Contains(joined, p) {
			t.Errorf("details %v do not name %s", r.Details, p)
		}
	}
}
