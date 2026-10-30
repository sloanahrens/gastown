package forgejoresync

import (
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/forgejoprov"
	"github.com/steveyegge/gastown/internal/forgejotest"
)

// liftRepo is the repository the lift tests protect, and liftBackupDir the directory
// the fake files write the backups to.
const (
	liftRepo      = "acme/rig"
	liftBackupDir = "/backups"
)

// liftNow is the clock the backup stamp is read from.
var liftNow = time.Date(2026, time.October, 30, 6, 32, 7, 0, time.UTC)

// liftRule is a protection rule as the provisioning run leaves it on main.
func liftRule(name string) map[string]any {
	return map[string]any{
		"rule_name":                 name,
		"enable_push":               false,
		"apply_to_admins":           true,
		"enable_status_check":       true,
		"status_check_contexts":     []string{"ci"},
		"block_on_outdated_branch":  true,
		"merge_whitelist_usernames": []string{"bot-landing"},
	}
}

// memFiles is an in-memory Files: it records each write and fails the step named
// in failStep.
type memFiles struct {
	files    map[string][]byte
	events   *[]string
	failStep string
}

// MkdirAll records the directory and fails when the test asks it to.
func (m *memFiles) MkdirAll(path string, _ fs.FileMode) error {
	*m.events = append(*m.events, "mkdir "+path)
	if m.failStep == "mkdir" {
		return errors.New("disk full")
	}
	return nil
}

// WriteFile stores data at path and fails when the test asks it to.
func (m *memFiles) WriteFile(path string, data []byte, _ fs.FileMode) error {
	*m.events = append(*m.events, "write "+path)
	if m.failStep == "write" {
		return errors.New("disk full")
	}
	m.files[path] = slices.Clone(data)
	return nil
}

// liftTransport sends each request to the fake, recording every DELETE in events. A
// method listed in answer is answered with that status instead, without reaching the
// fake, so a test can drive a status the fake would not return. A request of the method
// named in failMethod fails in transport, as a dropped connection does.
type liftTransport struct {
	fake       *forgejotest.Fake
	events     *[]string
	answer     map[string]int
	body       string
	failMethod string
}

// RoundTrip answers req through the fake, or with the status a test set for its method.
func (lt *liftTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete {
		*lt.events = append(*lt.events, "DELETE "+req.URL.Path)
	}
	if req.Method == lt.failMethod {
		return nil, errors.New("connection reset by peer")
	}
	if status, ok := lt.answer[req.Method]; ok {
		body := lt.body
		if body == "" {
			body = `{"message":"boom"}`
		}
		return &http.Response{
			StatusCode: status,
			Header:     http.Header{},
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	}
	return lt.fake.RoundTrip(req)
}

// liftFixture is one lift run wired to a fake Forgejo and in-memory files.
type liftFixture struct {
	fake   *forgejotest.Fake
	files  *memFiles
	events []string
	logs   []string
	resync *Resync
}

// newLiftFixture returns a run over a fake that holds liftRepo with the rules given.
// The admin token is resolved the way production resolves it, from a getenv that
// answers only the token.
func newLiftFixture(t *testing.T, branch string, dryRun bool, rules ...map[string]any) *liftFixture {
	t.Helper()
	fx := &liftFixture{fake: forgejotest.New()}
	fx.fake.AddRepo(liftRepo)
	for _, rule := range rules {
		fx.fake.SeedRule(liftRepo, rule)
	}
	secret, err := forgejoprov.ResolveAdminToken(
		func(string) string { return forgejotest.AdminToken },
		func(string) ([]byte, error) { return nil, fs.ErrNotExist },
		"")
	if err != nil {
		t.Fatalf("resolve admin token: %v", err)
	}
	fx.files = &memFiles{files: map[string][]byte{}, events: &fx.events}
	client := &forgejoprov.Client{
		API:   forgejotest.APIRoot + forgejotest.APIPath,
		HTTP:  &http.Client{Transport: &liftTransport{fake: fx.fake, events: &fx.events}},
		Token: secret,
	}
	fx.resync = &Resync{
		Opts: &Options{
			Repo:       liftRepo,
			MainBranch: branch,
			BackupDir:  liftBackupDir,
			DryRun:     dryRun,
		},
		Client: client,
		Files:  fx.files,
		Now:    func() time.Time { return liftNow },
		Log:    func(line string) { fx.logs = append(fx.logs, line) },
	}
	return fx
}

// assertNoToken fails when the admin token shows in any of the texts: the run's log,
// its error, or the backup it wrote.
func assertNoToken(t *testing.T, texts ...string) {
	t.Helper()
	for _, text := range texts {
		if strings.Contains(text, forgejotest.AdminToken) {
			t.Fatalf("admin token leaked into output: %q", text)
		}
	}
}

// TestLiftSavesThenDeletes checks the rule is saved to the backup and then deleted, in
// that order. Mutation: move the DELETE before the save and this goes red.
func TestLiftSavesThenDeletes(t *testing.T) {
	t.Parallel()
	fx := newLiftFixture(t, "main", false, liftRule("main"))
	if err := fx.resync.LiftProtection(); err != nil {
		t.Fatalf("LiftProtection: %v", err)
	}
	backup := liftBackupDir + "/acme__rig__main.json.bak-20261030-063207"
	saved, ok := fx.files.files[backup]
	if !ok {
		t.Fatalf("no backup written at %s; writes: %v", backup, fx.files.files)
	}
	var got map[string]any
	if err := json.Unmarshal(saved, &got); err != nil {
		t.Fatalf("backup is not the rule JSON: %v", err)
	}
	if got["rule_name"] != "main" {
		t.Fatalf("backup holds rule %v, want main", got["rule_name"])
	}
	if fx.fake.Rule(liftRepo, "main") != nil {
		t.Fatal("rule still on the repository after the lift")
	}
	wantOrder := []string{"mkdir " + liftBackupDir, "write " + backup, "DELETE /api/v1/repos/acme/rig/branch_protections/main"}
	if !slices.Equal(fx.events, wantOrder) {
		t.Fatalf("call order = %v, want %v", fx.events, wantOrder)
	}
	assertNoToken(t, fx.logs...)
}

// TestLiftNoRuleIsANoop checks a repository whose rules do not name main is left alone:
// nothing is saved and nothing is deleted.
func TestLiftNoRuleIsANoop(t *testing.T) {
	t.Parallel()
	fx := newLiftFixture(t, "main", false, liftRule("land/*"))
	if err := fx.resync.LiftProtection(); err != nil {
		t.Fatalf("LiftProtection: %v", err)
	}
	if len(fx.files.files) != 0 || len(fx.events) != 0 {
		t.Fatalf("no-rule lift wrote %v and sent %v", fx.files.files, fx.events)
	}
	if !slices.ContainsFunc(fx.logs, func(l string) bool { return strings.Contains(l, "nothing to lift") }) {
		t.Fatalf("logs %v do not say nothing to lift", fx.logs)
	}
	assertNoToken(t, fx.logs...)
}

// TestLiftTreats404AsAbsent checks a DELETE answered 404 is reported as already lifted,
// not as a failure.
func TestLiftTreats404AsAbsent(t *testing.T) {
	t.Parallel()
	fx := newLiftFixture(t, "main", false, liftRule("main"))
	fx.resync.Client.HTTP = &http.Client{Transport: &liftTransport{fake: fx.fake, events: &fx.events, answer: map[string]int{http.MethodDelete: http.StatusNotFound}}}
	if err := fx.resync.LiftProtection(); err != nil {
		t.Fatalf("LiftProtection: %v", err)
	}
	if !slices.ContainsFunc(fx.logs, func(l string) bool { return strings.HasSuffix(l, "(already absent)") }) {
		t.Fatalf("logs %v do not report the rule already absent", fx.logs)
	}
	assertNoToken(t, fx.logs...)
}

// TestLiftFailsOnOtherStatus checks a status or reply the lift does not expect fails the
// run, on the read and on the delete alike.
func TestLiftFailsOnOtherStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		method string
		status int
		body   string
		want   string
	}{
		{"delete refused", http.MethodDelete, http.StatusInternalServerError, "", "DELETE /repos/acme/rig/branch_protections/main: unexpected HTTP 500"},
		{"read refused", http.MethodGet, http.StatusInternalServerError, "", "GET /repos/acme/rig/branch_protections: unexpected HTTP 500"},
		{"read is not a list", http.MethodGet, http.StatusOK, "", "GET /repos/acme/rig/branch_protections: reply is not a list of rules"},
		{"read holds a non-object rule", http.MethodGet, http.StatusOK, `[1]`, "GET /repos/acme/rig/branch_protections: reply is not a list of rules"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fx := newLiftFixture(t, "main", false, liftRule("main"))
			fx.resync.Client.HTTP = &http.Client{Transport: &liftTransport{fake: fx.fake, events: &fx.events, answer: map[string]int{c.method: c.status}, body: c.body}}
			err := fx.resync.LiftProtection()
			if err == nil {
				t.Fatal("LiftProtection succeeded on an unexpected status")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not contain %q", err, c.want)
			}
			assertNoToken(t, err.Error())
		})
	}
}

// TestLiftStopsOnTransportFailure checks a request that fails in transport is an error,
// not a rule judged absent or a delete judged done: the rule stays, and no lift is logged.
func TestLiftStopsOnTransportFailure(t *testing.T) {
	t.Parallel()
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			fx := newLiftFixture(t, "main", false, liftRule("main"))
			fx.resync.Client.HTTP = &http.Client{Transport: &liftTransport{fake: fx.fake, events: &fx.events, failMethod: method}}
			if err := fx.resync.LiftProtection(); err == nil {
				t.Fatalf("LiftProtection succeeded with the %s failing in transport", method)
			}
			if fx.fake.Rule(liftRepo, "main") == nil {
				t.Fatal("rule deleted although the lift failed")
			}
			if slices.ContainsFunc(fx.logs, func(l string) bool { return strings.HasPrefix(l, "lifted ") }) {
				t.Fatalf("logs %v report a lift that failed", fx.logs)
			}
		})
	}
}

// TestLiftRepoMissingMessage checks a 404 on the read names both causes the operator
// can act on: the API URL and the token's visibility.
func TestLiftRepoMissingMessage(t *testing.T) {
	t.Parallel()
	fx := newLiftFixture(t, "main", false, liftRule("main"))
	fx.fake.RepoMissing = true
	err := fx.resync.LiftProtection()
	if err == nil {
		t.Fatal("LiftProtection succeeded on a repository it cannot see")
	}
	for _, want := range []string{"returned 404", "the repository is not on", "the admin token cannot see it"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not contain %q", err, want)
		}
	}
	if len(fx.events) != 0 {
		t.Fatalf("a missing repository still sent %v", fx.events)
	}
}

// TestLiftDryRunWritesNothing checks a dry run logs the backup it would write, writes no
// file and sends no DELETE: the rule stays on the repository.
func TestLiftDryRunWritesNothing(t *testing.T) {
	t.Parallel()
	fx := newLiftFixture(t, "main", true, liftRule("main"))
	if err := fx.resync.LiftProtection(); err != nil {
		t.Fatalf("LiftProtection: %v", err)
	}
	if len(fx.files.files) != 0 {
		t.Fatalf("dry run wrote %v", fx.files.files)
	}
	if fx.fake.Rule(liftRepo, "main") == nil {
		t.Fatal("dry run deleted the rule")
	}
	if len(fx.fake.Writes()) != 0 {
		t.Fatalf("dry run sent writes %v", fx.fake.Writes())
	}
	if !slices.ContainsFunc(fx.logs, func(l string) bool { return strings.HasPrefix(l, "+ write the main rule to ") }) {
		t.Fatalf("logs %v do not show the backup it would write", fx.logs)
	}
	assertNoToken(t, fx.logs...)
}

// TestLiftBackupPathEscapesSlash checks a branch with a slash gives %2F in the backup's
// file name and in the DELETE route, so the name stays one path element.
func TestLiftBackupPathEscapesSlash(t *testing.T) {
	t.Parallel()
	fx := newLiftFixture(t, "release/1", false, liftRule("release/1"))
	if err := fx.resync.LiftProtection(); err != nil {
		t.Fatalf("LiftProtection: %v", err)
	}
	backup := liftBackupDir + "/acme__rig__release%2F1.json.bak-20261030-063207"
	if _, ok := fx.files.files[backup]; !ok {
		t.Fatalf("no backup at %s; writes: %v", backup, fx.files.files)
	}
	if fx.fake.Rule(liftRepo, "release/1") != nil {
		t.Fatal("rule release/1 still on the repository")
	}
}

// TestLiftBackupFailureSendsNoDelete checks a backup that cannot be written stops the lift
// before the DELETE, so the rule is never removed without a copy of it.
func TestLiftBackupFailureSendsNoDelete(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"mkdir", "write"} {
		t.Run(step, func(t *testing.T) {
			fx := newLiftFixture(t, "main", false, liftRule("main"))
			fx.files.failStep = step
			err := fx.resync.LiftProtection()
			if err == nil {
				t.Fatal("LiftProtection succeeded with no backup written")
			}
			if fx.fake.Rule(liftRepo, "main") == nil {
				t.Fatal("rule deleted although its backup failed")
			}
			for _, event := range fx.events {
				if strings.HasPrefix(event, "DELETE") {
					t.Fatalf("DELETE sent after a failed backup: %v", fx.events)
				}
			}
		})
	}
}
