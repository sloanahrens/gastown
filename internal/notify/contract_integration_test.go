//go:build integration

package notify_test

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/beads"
	"github.com/steveyegge/gastown/internal/notify"
	"github.com/steveyegge/gastown/internal/notify/notifyfake"
	"github.com/steveyegge/gastown/internal/testutil"
)

var (
	gtOnce sync.Once
	gtBin  string
	gtErr  error
)

// builtGT links a gt binary from this checkout once per test binary, so the
// contract runs against the code under test rather than whatever gt is
// installed on PATH.
func builtGT(t *testing.T) string {
	t.Helper()
	gtOnce.Do(func() {
		_, file, _, ok := runtime.Caller(0)
		if !ok {
			gtErr = os.ErrNotExist
			return
		}
		root := filepath.Join(filepath.Dir(file), "..", "..")
		dir, err := os.MkdirTemp("", "gt-notify-contract-")
		if err != nil {
			gtErr = err
			return
		}
		bin := filepath.Join(dir, "gt")
		cmd := exec.Command("go", "build", "-ldflags", "-X github.com/steveyegge/gastown/internal/cmd.BuiltProperly=1", "-o", bin, "./cmd/gt")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			gtErr = &buildError{err: err, out: out}
			return
		}
		gtBin = bin
	})
	if gtErr != nil {
		t.Fatalf("building gt: %v", gtErr)
	}
	return gtBin
}

type buildError struct {
	err error
	out []byte
}

func (e *buildError) Error() string { return e.err.Error() + "\n" + string(e.out) }

// contractTown is a scratch town whose beads live in a database of the
// package's Dolt container, with gt run against it exactly as notify.CLI
// runs it.
type contractTown struct {
	t    *testing.T
	gt   string
	root string
	env  func() []string
}

// newContractTown builds a scratch town with its own town-level beads
// database on the test container: no GT_*/BD_* identity or socket from the
// caller's session reaches gt, the tmux socket derives from the scratch
// town's path, and Dolt is the container, so nothing it does can reach a live
// town.
func newContractTown(t *testing.T) *contractTown {
	t.Helper()
	gt := builtGT(t)
	port := testutil.DoltContainerPort()
	if port == "" {
		t.Fatal("no Dolt container: the integration tier needs GT_TEST_DOCKER=1 under gt slot run")
	}
	// A town of its own, without testutil.ScratchTown's chdir, so the
	// contract's cases can run in parallel: gt finds the town from its Dir
	// and GT_TOWN_ROOT.
	root := t.TempDir()
	for name, body := range map[string]string{
		"town.json": `{"type":"town","version":2,"name":"notify-contract"}`,
		"rigs.json": `{"version":1,"rigs":{}}`,
	} {
		if err := os.MkdirAll(filepath.Join(root, "mayor"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "mayor", name), []byte(body+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"init", "--quiet", "--prefix", "hq", "--server", "--server-port", port}
	if out, err := beads.RunTestContainerInit(t.Context(), root, args, nil); err != nil {
		t.Fatalf("bd init: %v\n%s", err, out)
	}
	env := func() []string {
		return testutil.CleanGTEnv(
			"GT_TOWN_ROOT="+root,
			"GT_TEST_HERMETIC=1",
			"GT_DOLT_PORT="+port,
			"BEADS_DOLT_PORT="+port,
		)
	}
	return &contractTown{t: t, gt: gt, root: root, env: env}
}

// run runs gt in the town and returns its stdout, failing the test on error.
func (c *contractTown) run(args ...string) []byte {
	c.t.Helper()
	cmd := exec.Command(c.gt, args...)
	cmd.Dir = c.root
	cmd.Env = c.env()
	var stderr []byte
	out, err := cmd.Output()
	if ee, ok := err.(*exec.ExitError); ok {
		stderr = ee.Stderr
	}
	if err != nil {
		c.t.Fatalf("gt %v: %v\n%s%s", args, err, out, stderr)
	}
	return out
}

// Inbox reads addr's mailbox with `gt mail inbox --json`.
func (c *contractTown) Inbox(addr string) []notifyfake.Mail {
	c.t.Helper()
	var msgs []struct {
		From      string `json:"from"`
		Subject   string `json:"subject"`
		Body      string `json:"body"`
		Timestamp string `json:"timestamp"`
	}
	if err := json.Unmarshal(c.run("mail", "inbox", "--json", "--", addr), &msgs); err != nil {
		c.t.Fatalf("parsing gt mail inbox %s: %v", addr, err)
	}
	slices.SortStableFunc(msgs, func(a, b struct {
		From      string `json:"from"`
		Subject   string `json:"subject"`
		Body      string `json:"body"`
		Timestamp string `json:"timestamp"`
	}) int {
		switch {
		case a.Timestamp < b.Timestamp:
			return -1
		case a.Timestamp > b.Timestamp:
			return 1
		}
		return 0
	})
	var out []notifyfake.Mail
	for _, m := range msgs {
		out = append(out, notifyfake.Mail{From: m.From, Subject: m.Subject, Body: m.Body})
	}
	return out
}

// OpenEscalations reads the open escalations with `gt escalate list --json`
// and keeps the ones labeled with key's fingerprint.
func (c *contractTown) OpenEscalations(key string) []notifyfake.OpenEscalation {
	c.t.Helper()
	var issues []*beads.Issue
	if err := json.Unmarshal(c.run("escalate", "list", "--json"), &issues); err != nil {
		c.t.Fatalf("parsing gt escalate list: %v", err)
	}
	label := notify.FingerprintLabel(key)
	var out []notifyfake.OpenEscalation
	for _, issue := range issues {
		if !slices.Contains(issue.Labels, label) {
			continue
		}
		f := beads.ParseEscalationFields(issue.Description)
		occurrences := f.Occurrences
		if occurrences == 0 {
			// A new escalation records no count; its first repeat is its
			// second firing (beads.BumpEscalation).
			occurrences = 1
		}
		out = append(out, notifyfake.OpenEscalation{
			Title:       issue.Title,
			Severity:    f.Severity,
			Reason:      f.Reason,
			Source:      f.Source,
			Occurrences: occurrences,
		})
	}
	return out
}

// TestIntegrationNotifierContract runs the contract against notify.CLI and a
// gt built from this tree, each case in its own scratch town on the Dolt
// container.
func TestIntegrationNotifierContract(t *testing.T) {
	notifyfake.RunNotifierContract(t, func(t *testing.T) notifyfake.Subject {
		town := newContractTown(t)
		return notifyfake.Subject{
			Notifier: &notify.CLI{Bin: town.gt, Dir: town.root, Env: town.env},
			Observer: town,
		}
	})
}
