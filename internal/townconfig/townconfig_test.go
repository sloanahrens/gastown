package townconfig

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
)

// liveTown is the scrubbed copy of the operator town's files that
// internal/config's strict-decoding tests also use.
const liveTown = "../config/testdata/livetown"

// copyLiveTown copies the live town's town-level files into a temp town.
func copyLiveTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, f := range []string{FileTown, FileRigs, FileSettings, FileDaemon, FileDaemonEnv, FileDolt} {
		data, err := os.ReadFile(filepath.Join(liveTown, f))
		if err != nil {
			t.Fatal(err)
		}
		write(t, root, f, string(data))
	}
	return root
}

func write(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadAcceptsTheLiveTown(t *testing.T) {
	t.Parallel()
	town, err := Load(liveTown)
	if err != nil {
		t.Fatalf("Load(live town) = %v", err)
	}
	for _, f := range []string{FileTown, FileRigs, FileSettings, FileDaemon, FileDaemonEnv, FileDolt} {
		if !town.Present(f) {
			t.Errorf("Present(%s) = false", f)
		}
	}
	want := map[string]string{"beads": "be", "gastown": "gt", "hm": "hm", "mango": "ma", "om": "om"}
	if got := strings.Join(town.RigNames(), ","); got != "beads,gastown,hm,mango,om" {
		t.Errorf("RigNames = %s", got)
	}
	for rig, prefix := range want {
		got, err := town.RigPrefix(rig)
		if err != nil || got != prefix {
			t.Errorf("RigPrefix(%s) = %q, %v; want %q", rig, got, err, prefix)
		}
	}
	ep, ok := town.DoltEndpoint()
	if !ok || ep.Port != 3307 {
		t.Errorf("DoltEndpoint = %+v, %v; want port 3307", ep, ok)
	}
	if town.Identity().Name == "" {
		t.Error("Identity().Name is empty")
	}
	if d := town.Daemon(); d == nil || d.Patrols.RolePatrol("witness") == nil {
		t.Error("Daemon() lost the witness patrol")
	}
	if s := town.Settings(); s == nil || s.RoleAgents["mayor"] == "" {
		t.Error("Settings() lost role_agents.mayor")
	}
	if len(town.DaemonEnv()) == 0 {
		t.Error("DaemonEnv() is empty")
	}
}

func TestUnknownRigIsAnErrorNeverGt(t *testing.T) {
	t.Parallel()
	town, err := Load(liveTown)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := town.RigPrefix("nosuchrig")
	if !errors.Is(err, ErrUnknownRig) || prefix != "" {
		t.Fatalf("RigPrefix(unknown) = %q, %v; want \"\", ErrUnknownRig", prefix, err)
	}
	if _, err := town.Rig("nosuchrig"); !errors.Is(err, ErrUnknownRig) {
		t.Fatalf("Rig(unknown) = %v, want ErrUnknownRig", err)
	}
}

func TestRigWithoutPrefixIsAnError(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	write(t, root, FileRigs, `{"version":1,"rigs":{"bare":{"git_url":"x","added_at":"2026-01-01T00:00:00Z"}}}`)
	town, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := town.RigPrefix("bare"); !errors.Is(err, ErrNoRigPrefix) {
		t.Fatalf("RigPrefix(bare) = %v, want ErrNoRigPrefix", err)
	}
}

func TestDuplicateRigPrefixFailsLoad(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	write(t, root, FileRigs, `{"version":1,"rigs":{
		"a":{"git_url":"x","added_at":"2026-01-01T00:00:00Z","beads":{"repo":"local","prefix":"dup-"}},
		"b":{"git_url":"y","added_at":"2026-01-01T00:00:00Z","beads":{"repo":"local","prefix":"dup"}}}}`)
	_, err := Load(root)
	if err == nil || !strings.Contains(err.Error(), `"dup"`) || !strings.Contains(err.Error(), filepath.Join(root, FileRigs)) {
		t.Fatalf("Load with a shared prefix = %v, want an error naming rigs.json and the prefix", err)
	}
}

func TestLoadOutsideATown(t *testing.T) {
	t.Parallel()
	if _, err := Load(t.TempDir()); !errors.Is(err, ErrNotATown) {
		t.Fatalf("Load(empty dir) = %v, want ErrNotATown", err)
	}
}

func TestOptionalFilesMayBeAbsent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, FileTown, `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z"}`)
	town, err := Load(root)
	if err != nil {
		t.Fatalf("Load(town.json only) = %v", err)
	}
	for _, f := range []string{FileRigs, FileSettings, FileDaemon, FileDaemonEnv, FileDolt} {
		if town.Present(f) {
			t.Errorf("Present(%s) = true for an absent file", f)
		}
	}
	if town.Settings() != nil || town.Daemon() != nil {
		t.Error("an absent file must read as absent, not as compiled defaults")
	}
	if _, ok := town.DoltEndpoint(); ok {
		t.Error("DoltEndpoint ok without a managed config.yaml")
	}
	if len(town.RigNames()) != 0 {
		t.Error("RigNames without rigs.json")
	}
	if town.Operational() == nil {
		t.Error("Operational() must never be nil")
	}
}

// TestEachBrokenFileFailsClosedWithOneLine: a parse error in any file fails
// the load with one line naming that file and a position; nothing falls back.
func TestEachBrokenFileFailsClosedWithOneLine(t *testing.T) {
	t.Parallel()
	broken := map[string]string{
		FileTown:      "{\"type\": \"town\",}",
		FileRigs:      "{\"version\": 1, \"rigz\": {}}",
		FileSettings:  "{\"type\": \"town-settings\", \"role_agent\": {}}",
		FileDaemon:    "{\"patrols\": {\"witness\": {\"enabled\": false,}}}",
		FileDaemonEnv: "PATH=/bin\nnot a pair\n",
		FileDolt:      "listener:\n  port: 3307\n  prot: 1\n",
	}
	for file, body := range broken {
		root := copyLiveTown(t)
		write(t, root, file, body)
		_, err := Load(root)
		if !errors.Is(err, config.ErrUnparseable) {
			t.Errorf("%s broken: Load = %v, want ErrUnparseable", file, err)
			continue
		}
		msg := err.Error()
		if strings.Count(msg, "\n") != 0 || !strings.Contains(msg, filepath.Join(root, file)) || !strings.Contains(msg, "line ") {
			t.Errorf("%s broken: message %q must be one line naming the file and a line", file, msg)
		}
		if err := Check(root); err == nil {
			t.Errorf("%s broken: Check = nil", file)
		}
	}
}

func TestTwoBrokenFilesGiveTwoLines(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	write(t, root, FileSettings, "{,}")
	write(t, root, FileDaemon, "{,}")
	_, err := Load(root)
	if err == nil {
		t.Fatal("Load with two broken files = nil")
	}
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], FileSettings) || !strings.Contains(lines[1], FileDaemon) {
		t.Fatalf("want one line per broken file (settings, then daemon), got %q", lines)
	}
}

func TestAccessorsReturnCopies(t *testing.T) {
	t.Parallel()
	town, err := Load(liveTown)
	if err != nil {
		t.Fatal(err)
	}
	s := town.Settings()
	s.RoleAgents["mayor"] = "mutated"
	s.DefaultAgent = "mutated"
	d := town.Daemon()
	d.Patrols.Witness.Enabled = !d.Patrols.Witness.Enabled
	env := town.DaemonEnv()
	env["PATH"] = "mutated"
	names := town.RigNames()
	names[0] = "mutated"

	if town.Settings().RoleAgents["mayor"] == "mutated" || town.Settings().DefaultAgent == "mutated" {
		t.Error("Settings() shares state with the kernel")
	}
	if town.Daemon().Patrols.Witness.Enabled == d.Patrols.Witness.Enabled {
		t.Error("Daemon() shares state with the kernel")
	}
	if town.DaemonEnv()["PATH"] == "mutated" || town.RigNames()[0] == "mutated" {
		t.Error("DaemonEnv()/RigNames() share state with the kernel")
	}
}
