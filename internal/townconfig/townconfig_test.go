package townconfig

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
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
	if d := town.Daemon(); d == nil || d.Patrols.RolePatrol("handler") == nil {
		t.Error("Daemon() lost the handler patrol")
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

// TestCheckJudgesOnlyTheFilesPresent: Check is the parse gate. A root with no
// files passes (first run), and a broken file fails it even without
// town.json.
func TestCheckJudgesOnlyTheFilesPresent(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if err := Check(root); err != nil {
		t.Fatalf("Check(empty dir) = %v, want nil", err)
	}
	write(t, root, FileDaemon, `{"patrols": {"handler": {"enabled": false, "rigz": []}}}`)
	err := Check(root)
	if !errors.Is(err, config.ErrUnparseable) || errors.Is(err, ErrNotATown) {
		t.Fatalf("Check(broken daemon.json, no town.json) = %v, want only the parse error", err)
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
		FileDaemon:    "{\"patrols\": {\"handler\": {\"enabled\": false,}}}",
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

// TestRefusedPoolPolicyFailsClosedAtLoad: a seat-refill policy value the
// plugin cannot act on is refused at load, with one line naming the file and
// the key, so a hand-edited settings/config.json stops the daemon instead of
// dispatching on a value the plugin would ignore (gt-y3pgh.12).
func TestRefusedPoolPolicyFailsClosedAtLoad(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	write(t, root, FileSettings, `{"type":"town-settings","version":1,"polecat_pool":{"overflow_agent":"deepseek-flash","max_overflow":2,"max_priority":-1}}`)
	_, err := Load(root)
	if err == nil {
		t.Fatal("Load with max_priority -1 = nil, want a refusal")
	}
	if msg := err.Error(); strings.Contains(msg, "\n") ||
		!strings.Contains(msg, filepath.Join(root, FileSettings)) ||
		!strings.Contains(msg, "polecat_pool.max_priority") {
		t.Errorf("refusal %q must be one line naming settings/config.json and the key", msg)
	}
	if err := Check(root); err == nil {
		t.Error("Check = nil for a refused policy value")
	}

	// The keys are optional: the same polecat_pool without them loads.
	write(t, root, FileSettings, `{"type":"town-settings","version":1,"polecat_pool":{"overflow_agent":"deepseek-flash","max_overflow":2}}`)
	if _, err := Load(root); err != nil {
		t.Fatalf("Load without the policy keys = %v", err)
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
	d.Patrols.Handler.Enabled = !d.Patrols.Handler.Enabled
	env := town.DaemonEnv()
	env["PATH"] = "mutated"
	names := town.RigNames()
	names[0] = "mutated"

	if town.Settings().RoleAgents["mayor"] == "mutated" || town.Settings().DefaultAgent == "mutated" {
		t.Error("Settings() shares state with the kernel")
	}
	if town.Daemon().Patrols.Handler.Enabled == d.Patrols.Handler.Enabled {
		t.Error("Daemon() shares state with the kernel")
	}
	if town.DaemonEnv()["PATH"] == "mutated" || town.RigNames()[0] == "mutated" {
		t.Error("DaemonEnv()/RigNames() share state with the kernel")
	}
}

// TestCheckRefusesAnUnreadableFile: a kernel path that exists but cannot be
// read fails the gate; it is never taken for an absent file.
func TestCheckRefusesAnUnreadableFile(t *testing.T) {
	t.Parallel()
	for _, file := range []string{FileTown, FileRigs, FileSettings, FileDaemonEnv, FileDolt, FileDaemon} {
		root := copyLiveTown(t)
		path := filepath.Join(root, file)
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(path, 0o755); err != nil { // a directory cannot be read as a file
			t.Fatal(err)
		}
		err := Check(root)
		if err == nil || !strings.Contains(err.Error(), path) {
			t.Errorf("%s unreadable: Check = %v, want an error naming %s", file, err, path)
		}
	}
}

// The endpoint in town.json is the truth; the managed config.yaml answers
// only for a town.json without one (gt-y3pgh.3).
func TestDoltEndpointTownJSONWins(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	write(t, root, FileTown, `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","dolt":{"host":"127.0.0.2","port":5507}}`)
	town, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	ep, ok := town.DoltEndpoint()
	if !ok || ep.Host != "127.0.0.2" || ep.Port != 5507 {
		t.Fatalf("DoltEndpoint = %+v, %v; want 127.0.0.2:5507 from town.json", ep, ok)
	}
	if want, _ := config.ResolveDoltEndpoint(root); want.Host != ep.Host || want.Port != ep.Port {
		t.Errorf("config.ResolveDoltEndpoint = %+v, kernel = %+v; the two must agree", want, ep)
	}
}

func TestTownJSONRejectsNonPortDolt(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	write(t, root, FileTown, `{"type":"town","version":2,"name":"t","created_at":"2026-01-01T00:00:00Z","dolt":{"port":0}}`)
	if _, err := Load(root); err == nil || !strings.Contains(err.Error(), "dolt.port") {
		t.Fatalf("Load(dolt.port 0) = %v, want a dolt.port error", err)
	}
}

// TestLoadReadsTheTwoFileLayout: after gt config migrate the kernel reads
// the same town from mayor/town.json and settings/config.json alone.
func TestLoadReadsTheTwoFileLayout(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	before, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := config.MigrateLayout(root); err != nil {
		t.Fatalf("MigrateLayout = %v", err)
	}
	for _, f := range []string{FileRigs, FileDaemon} {
		if _, err := os.Stat(filepath.Join(root, f)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s survived the migration (%v)", f, err)
		}
	}
	after, err := Load(root)
	if err != nil {
		t.Fatalf("Load(two-file town) = %v", err)
	}
	if got, want := strings.Join(after.RigNames(), ","), strings.Join(before.RigNames(), ","); got != want {
		t.Errorf("RigNames = %s, want %s", got, want)
	}
	if got, want := after.RigPrefixes(), before.RigPrefixes(); !reflect.DeepEqual(got, want) {
		t.Errorf("RigPrefixes = %v, want %v", got, want)
	}
	for _, rig := range before.RigNames() {
		b, _ := before.Rig(rig)
		a, _ := after.Rig(rig)
		if !reflect.DeepEqual(a, b) {
			t.Errorf("Rig(%s) = %+v, want %+v", rig, a, b)
		}
	}
	if p, _ := ParkState(root, "mango"); p == nil || p.By != "operator" {
		t.Errorf("ParkState(mango) = %+v, want the registry park", p)
	}
	if d := after.Daemon(); d == nil || d.Patrols.RolePatrol("handler") == nil {
		t.Error("Daemon() lost the handler patrol")
	}
	if ep, ok := after.DoltEndpoint(); !ok || ep.Port != 3307 {
		t.Errorf("DoltEndpoint = %+v, %v", ep, ok)
	}
	if err := Check(root); err != nil {
		t.Errorf("Check(two-file town) = %v", err)
	}
}

// The kernel decodes the daemon document strictly, so a patrol_scan.worktree_cleanup
// block only loads because internal/config declares it (gt-rwfua). The two-file
// layout reads the same document from settings/config.json's "daemon" section
// through the same loader, so this covers both layouts.
func TestLoadDecodesWorktreeCleanupBlock(t *testing.T) {
	t.Parallel()
	root := copyLiveTown(t)
	path := filepath.Join(root, FileDaemon)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	patrols, _ := doc["patrols"].(map[string]any)
	patrols["patrol_scan"] = map[string]any{
		"enabled": true,
		"worktree_cleanup": map[string]any{
			"enabled": true, "dry_run": false, "max_per_tick": 3,
		},
	}
	patched, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	write(t, root, FileDaemon, string(patched))

	town, err := Load(root)
	if err != nil {
		t.Fatalf("Load(town with worktree_cleanup) = %v", err)
	}
	wc := town.Daemon().Patrols.PatrolScan.WorktreeCleanup
	if wc == nil {
		t.Fatal("worktree_cleanup did not load")
	}
	if !wc.IsEnabled() || wc.IsDryRun() || wc.Cap() != 3 {
		t.Errorf("loaded block = enabled:%v dry-run:%v cap:%d, want true/false/3",
			wc.IsEnabled(), wc.IsDryRun(), wc.Cap())
	}
}
