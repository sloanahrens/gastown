package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/ring"
)

// ringTestKey is shaped like a signing key. No verb output may carry it.
const ringTestKey = "literal-ring-signing-key-value"

// ringTestNow is the instant the verbs in these tests run at.
var ringTestNow = time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)

// ringTestEnv is the daemon environment of a town whose key is set.
func ringTestEnv(name string) string {
	if name == "GT_RING_KEY" {
		return ringTestKey
	}
	return ""
}

// ringTestTown makes a town with a mayor/town.json and settings/config.json
// whose ring.key is the reference "${GT_RING_KEY}". It holds no ring yet.
func ringTestTown(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	ringTestTownFile(t, root)
	ringTestSettings(t, root, "${GT_RING_KEY}")
	return root
}

// ringTestTownFile writes mayor/town.json into root.
func ringTestTownFile(t *testing.T, root string) {
	t.Helper()
	town := &config.TownConfig{Type: "town", Version: config.CurrentTownVersion, Name: "ring"}
	if err := config.WriteConfigJSON(filepath.Join(root, filepath.FromSlash(config.MachineConfigFile)), town, 0o600); err != nil {
		t.Fatalf("writing mayor/town.json: %v", err)
	}
}

// ringTestSettings writes settings/config.json into root, with ring.key set
// to keyRef.
func ringTestSettings(t *testing.T, root, keyRef string) {
	t.Helper()
	settings := &config.TownSettings{Type: "town-settings", Version: config.CurrentTownSettingsVersion, Ring: &config.RingConfig{Key: keyRef}}
	data, err := json.Marshal(settings)
	if err != nil {
		t.Fatal(err)
	}
	path := config.TownSettingsPath(root)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("writing settings/config.json: %v", err)
	}
}

// ringTestDeps is the deps a verb runs with: the town, the environment, the
// instant, and a fixed actor.
func ringTestDeps(root string, getenv func(string) string, now time.Time) ringDeps {
	return ringDeps{townRoot: root, getenv: getenv, now: now, actor: "slit"}
}

// ringExit is the process exit status a verb's error maps to, 0 for nil.
func ringExit(err error) int {
	if err == nil {
		return 0
	}
	return exitCodeForError(err)
}

// ringRun runs a verb and returns what it wrote to stdout and its error.
func ringRun(run func(ringDeps, io.Writer) error, d ringDeps) (string, error) {
	var out bytes.Buffer
	err := run(d, &out)
	return out.String(), err
}

func TestRingForgeThenStatusIsValidForALease(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	d := ringTestDeps(root, ringTestEnv, ringTestNow)
	if _, err := ringRun(runRingForge, d); err != nil {
		t.Fatalf("forge: %v", err)
	}
	out, err := ringRun(runRingStatus, d)
	if got := ringExit(err); got != 0 {
		t.Fatalf("status after forge exits %d, want 0 (valid)", got)
	}
	if !strings.Contains(out, "ring: valid") || !strings.Contains(out, "wanes in 36h00m") {
		t.Fatalf("status after forge = %q, want a valid Ring waning in 36h00m", out)
	}
}

func TestRingRenewExtendsAWanedLease(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	forged := ringTestDeps(root, ringTestEnv, ringTestNow)
	if _, err := ringRun(runRingForge, forged); err != nil {
		t.Fatalf("forge: %v", err)
	}
	later := ringTestNow.Add(37 * time.Hour)
	d := ringTestDeps(root, ringTestEnv, later)
	if _, err := ringRun(runRingStatus, d); ringExit(err) != 2 {
		t.Fatalf("status of a waned Ring exits %d, want 2", ringExit(err))
	}
	if _, err := ringRun(runRingRenew, d); err != nil {
		t.Fatalf("renew of a waned Ring: %v", err)
	}
	out, err := ringRun(runRingStatus, d)
	if got := ringExit(err); got != 0 {
		t.Fatalf("status after renew exits %d, want 0 (valid)", got)
	}
	if !strings.Contains(out, "wanes in 36h00m") {
		t.Fatalf("status after renew = %q, want the Ring extended by a lease from now", out)
	}
}

func TestRingRenewRefusesAnAbsentRing(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	d := ringTestDeps(root, ringTestEnv, ringTestNow)
	_, err := ringRun(runRingRenew, d)
	if err == nil || !strings.Contains(err.Error(), "no Ring; forge it") {
		t.Fatalf("renew with no Ring = %v, want a refusal to forge one", err)
	}
}

func TestRingRemoveThenStatusIsAbsent(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	d := ringTestDeps(root, ringTestEnv, ringTestNow)
	if _, err := ringRun(runRingForge, d); err != nil {
		t.Fatalf("forge: %v", err)
	}
	if _, err := ringRun(runRingRemove, d); err != nil {
		t.Fatalf("remove: %v", err)
	}
	out, err := ringRun(runRingStatus, d)
	if got := ringExit(err); got != 3 {
		t.Fatalf("status after remove exits %d, want 3 (absent)", got)
	}
	if !strings.Contains(out, "absent") {
		t.Fatalf("status after remove = %q, want an absent Ring", out)
	}
}

func TestRingRemoveOfNoRingSaysSo(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	out, err := ringRun(runRingRemove, ringTestDeps(root, ringTestEnv, ringTestNow))
	if err != nil {
		t.Fatalf("remove with no Ring: %v", err)
	}
	if !strings.Contains(out, "no Ring to remove") {
		t.Fatalf("remove with no Ring printed %q, want it to say there is none", out)
	}
}

// TestRingForgeRefusesALiteralKeyInSettings: a literal ring.key in settings is
// refused by the verb, which writes nothing and never prints the literal.
func TestRingForgeRefusesALiteralKeyInSettings(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	ringTestSettings(t, root, ringTestKey)
	out, err := ringRun(runRingForge, ringTestDeps(root, ringTestEnv, ringTestNow))
	if ringExit(err) != 1 {
		t.Fatalf("forge with a literal ring.key exits %d, want 1", ringExit(err))
	}
	if strings.Contains(out+err.Error(), ringTestKey) {
		t.Fatal("forge with a literal ring.key echoed the literal")
	}
	st, rerr := ring.Read(root, []byte(ringTestKey), ringTestNow)
	if rerr != nil || st.Kind != ring.Absent {
		t.Fatalf("forge with a literal ring.key left a Ring behind: %v (%v)", st, rerr)
	}
}

// TestRingForgeWithoutAKeyExitsOneAndWritesNothing: a write whose key does not
// resolve is refused before the town is touched.
func TestRingForgeWithoutAKeyExitsOneAndWritesNothing(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	unset := func(string) string { return "" }
	_, err := ringRun(runRingForge, ringTestDeps(root, unset, ringTestNow))
	if got := ringExit(err); got != 1 {
		t.Fatalf("forge with no key exits %d, want 1", got)
	}
	st, rerr := ring.Read(root, []byte(ringTestKey), ringTestNow)
	if rerr != nil {
		t.Fatalf("reading the town back: %v", rerr)
	}
	if st.Kind != ring.Absent {
		t.Fatalf("forge with no key left a Ring behind: %v", st)
	}
}

// ringRunVerb runs one verb of the set newRingVerbs builds, the constructor
// gt's own registration uses, under a parent command as cobra runs it: cobra
// prints a returned error to stderr. It returns what stdout and stderr got
// together, and the error.
func ringRunVerb(find func() (string, error), getenv func(string) string, verb string) (string, error) {
	var out bytes.Buffer
	parent := &cobra.Command{Use: "ring", SilenceUsage: true}
	parent.AddCommand(newRingVerbs(find, getenv)...)
	parent.SetOut(&out)
	parent.SetErr(&out)
	parent.SetArgs([]string{verb})
	err := parent.Execute()
	return out.String(), err
}

// TestRingVerbsNeverPrintTheKey runs every verb through the commands
// newRingVerbs builds, with the key in the environment the verbs read, and
// checks that each step printed what it should (so an empty capture cannot
// pass) and that the key value appears in none of the output, stderr and
// refusals included.
func TestRingVerbsNeverPrintTheKey(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	find := func() (string, error) { return root, nil }
	steps := []struct{ verb, want string }{
		{"status", "ring:"},
		{"forge", "forged: the Ring is valid until"},
		{"status", "wanes in"},
		{"renew", "renewed: the Ring is valid until"},
		{"remove", "removed the Ring"},
		{"status", "ring:"},
		{"renew", "no Ring; forge it"}, // no Ring left: refused, so the refusal reaches stderr
	}
	var all strings.Builder
	for _, step := range steps {
		out, _ := ringRunVerb(find, ringTestEnv, step.verb)
		if !strings.Contains(out, step.want) {
			t.Errorf("gt ring %s printed %q, want it to contain %q", step.verb, out, step.want)
		}
		all.WriteString(out)
	}
	// A waned Ring reports its state and the time since, and never the key.
	waned := ringTestTown(t)
	rec, err := ring.Sign(ring.Record{ForgedAt: ringTestNow.Add(-72 * time.Hour), ExpiresAt: ringTestNow.Add(-36 * time.Hour), ForgedBy: "test"}, []byte(ringTestKey))
	if err != nil {
		t.Fatalf("signing the waned Ring: %v", err)
	}
	if err := ring.Write(waned, rec); err != nil {
		t.Fatalf("writing the waned Ring: %v", err)
	}
	wanedOut, _ := ringRunVerb(func() (string, error) { return waned, nil }, ringTestEnv, "status")
	if !strings.Contains(wanedOut, "waned") || !strings.Contains(wanedOut, "ago") {
		t.Errorf("gt ring status of a waned Ring printed %q, want it to say it waned and how long ago", wanedOut)
	}
	all.WriteString(wanedOut)
	// A literal ring.key in settings is refused, and the refusal says so
	// without echoing the literal.
	literal := ringTestTown(t)
	ringTestSettings(t, literal, ringTestKey)
	out, err := ringRunVerb(func() (string, error) { return literal, nil }, ringTestEnv, "forge")
	if err == nil || !strings.Contains(out, "Error:") {
		t.Errorf("forge with a literal ring.key = %q, %v; want a refusal on stderr", out, err)
	}
	all.WriteString(out)
	if strings.Contains(all.String(), ringTestKey) {
		t.Fatalf("a verb printed the signing key (output redacted):\n%s", strings.ReplaceAll(all.String(), ringTestKey, "<key>"))
	}
}

// TestRingVerbRefusesOutsideATown: a verb run where no town is found refuses
// before it touches anything.
func TestRingVerbRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	noTown := func() (string, error) { return "", errors.New("no mayor/town.json above the cwd") }
	err := ringVerb(noTown, ringTestEnv, runRingForge)(&cobra.Command{}, nil)
	if err == nil || !strings.Contains(err.Error(), "not in a Gas Town workspace") {
		t.Fatalf("a verb outside a town = %v, want a refusal", err)
	}
}

// TestRingLeaseRefusesNoKey: a lease under no key is refused by ring.Sign, and
// nothing is stored.
func TestRingLeaseRefusesNoKey(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	_, err := ringTestDeps(root, ringTestEnv, ringTestNow).lease(nil)
	if !errors.Is(err, ring.ErrNoKey) {
		t.Fatalf("lease with no key = %v, want ring.ErrNoKey", err)
	}
	st, rerr := ring.Read(root, []byte(ringTestKey), ringTestNow)
	if rerr != nil || st.Kind != ring.Absent {
		t.Fatalf("lease with no key stored a Ring: %v (%v)", st, rerr)
	}
}

// TestRingForgeRefusesATownWithNoRingFile: a town whose mayor/town.json is
// missing refuses the write, and the forge reports it without creating the file.
func TestRingForgeRefusesATownWithNoRingFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ringTestSettings(t, root, "${GT_RING_KEY}")
	_, err := ringRun(runRingForge, ringTestDeps(root, ringTestEnv, ringTestNow))
	if ringExit(err) != 1 || !strings.Contains(err.Error(), "writing the Ring") {
		t.Fatalf("forge into a town with no mayor/town.json = %v, want a refused write", err)
	}
	if _, serr := os.Stat(filepath.Join(root, filepath.FromSlash(config.MachineConfigFile))); !os.IsNotExist(serr) {
		t.Fatalf("forge created mayor/town.json: %v", serr)
	}
}

// TestRingRemoveRefusesATownWithNoRingFile: remove reports a town whose
// mayor/town.json is missing rather than calling it removed.
func TestRingRemoveRefusesATownWithNoRingFile(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	ringTestSettings(t, root, "${GT_RING_KEY}")
	_, err := ringRun(runRingRemove, ringTestDeps(root, ringTestEnv, ringTestNow))
	if err == nil || !strings.Contains(err.Error(), "removing the Ring") {
		t.Fatalf("remove from a town with no mayor/town.json = %v, want a refusal", err)
	}
}

// TestRingVerbsRefuseWithoutAKey: every verb that touches the Ring refuses when
// its key does not resolve.
func TestRingVerbsRefuseWithoutAKey(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	unset := func(string) string { return "" }
	verbs := map[string]func(ringDeps, io.Writer) error{
		"forge":  runRingForge,
		"renew":  runRingRenew,
		"status": runRingStatus,
		"remove": runRingRemove,
	}
	for name, run := range verbs {
		if _, err := ringRun(run, ringTestDeps(root, unset, ringTestNow)); err == nil {
			t.Errorf("%s with no resolvable key = nil error, want a refusal", name)
		}
	}
}

// TestRingRenewAndStatusRefuseAnUnreadableRing: a mayor/town.json that does not
// parse is an error to renew and status, never an absent Ring.
func TestRingRenewAndStatusRefuseAnUnreadableRing(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(config.MachineConfigFile)), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := ringTestDeps(root, ringTestEnv, ringTestNow)
	for name, run := range map[string]func(ringDeps, io.Writer) error{"renew": runRingRenew, "status": runRingStatus} {
		_, err := ringRun(run, d)
		if ringExit(err) != 1 || !strings.Contains(err.Error(), "reading the Ring") {
			t.Errorf("%s of an unreadable town = %v, want a read refusal", name, err)
		}
	}
}

// TestRingHelpListsEveryVerb: gt ring --help lists forge, renew, status and
// remove under Available Commands, each with its one-line summary. The verbs
// are the ones newRingVerbs builds, under a parent that carries gt ring's own
// short and long text, and cobra renders the help into a buffer. The line must
// start with the verb name on its own line, so the Long text, which names the
// verbs inside an indented sentence-like line, cannot satisfy it.
func TestRingHelpListsEveryVerb(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	parent := &cobra.Command{Use: "ring", Short: ringCmd.Short, Long: ringCmd.Long}
	verbs := newRingVerbs(func() (string, error) { return "", nil }, ringTestEnv)
	parent.AddCommand(verbs...)
	parent.SetOut(&out)
	parent.SetErr(&out)
	parent.SetArgs([]string{"--help"})
	if err := parent.Execute(); err != nil {
		t.Fatalf("gt ring --help: %v", err)
	}
	help := out.String()
	short := map[string]string{}
	for _, verb := range verbs {
		short[verb.Name()] = verb.Short
	}
	for _, name := range []string{"forge", "renew", "status", "remove"} {
		summary, built := short[name]
		if !built {
			t.Errorf("newRingVerbs builds no %q verb", name)
			continue
		}
		listed := regexp.MustCompile(`(?m)^[ \t]+` + regexp.QuoteMeta(name) + `[ \t]+` + regexp.QuoteMeta(summary) + `$`)
		if !listed.MatchString(help) {
			t.Errorf("gt ring --help does not list %q with %q:\n%s", name, summary, help)
		}
	}
}
