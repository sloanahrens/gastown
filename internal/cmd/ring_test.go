package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
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

// TestRingVerbsNeverPrintTheKey runs every verb through a full cycle and
// checks the key value appears in none of their stdout or error text.
func TestRingVerbsNeverPrintTheKey(t *testing.T) {
	t.Parallel()
	root := ringTestTown(t)
	var all strings.Builder
	steps := []struct {
		run func(ringDeps, io.Writer) error
		at  time.Time
	}{
		{runRingStatus, ringTestNow},
		{runRingForge, ringTestNow},
		{runRingStatus, ringTestNow},
		{runRingStatus, ringTestNow.Add(37 * time.Hour)},
		{runRingRenew, ringTestNow.Add(37 * time.Hour)},
		{runRingRemove, ringTestNow.Add(37 * time.Hour)},
		{runRingStatus, ringTestNow.Add(37 * time.Hour)},
	}
	for _, s := range steps {
		out, err := ringRun(s.run, ringTestDeps(root, ringTestEnv, s.at))
		all.WriteString(out)
		if err != nil {
			all.WriteString(err.Error())
		}
	}
	if strings.Contains(all.String(), ringTestKey) {
		t.Fatalf("a verb printed the signing key (output redacted):\n%s", strings.ReplaceAll(all.String(), ringTestKey, "<key>"))
	}
}

// TestRingVerbRefusesOutsideATown: a verb run where no town is found refuses
// before it touches anything.
func TestRingVerbRefusesOutsideATown(t *testing.T) {
	t.Parallel()
	noTown := func() (string, error) { return "", errors.New("no mayor/town.json above the cwd") }
	err := ringVerb(noTown, runRingForge)(&cobra.Command{}, nil)
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

// TestRingHelpListsEveryVerb: gt ring --help names forge, renew, status and
// remove, each with its one-line summary. The help func prints to the process
// stdout, so the test reads the usage text it is built from.
func TestRingHelpListsEveryVerb(t *testing.T) {
	t.Parallel()
	help := ringCmd.UsageString()
	for _, verb := range []struct{ name, short string }{
		{ringForgeCmd.Name(), ringForgeCmd.Short},
		{ringRenewCmd.Name(), ringRenewCmd.Short},
		{ringStatusCmd.Name(), ringStatusCmd.Short},
		{ringRemoveCmd.Name(), ringRemoveCmd.Short},
	} {
		if !strings.Contains(help, verb.name) || !strings.Contains(help, verb.short) {
			t.Errorf("gt ring --help does not list %q with %q:\n%s", verb.name, verb.short, help)
		}
	}
}
