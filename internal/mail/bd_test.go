package mail

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beads"
)

func TestBeadsMessagesFromIssues(t *testing.T) {
	t.Parallel()
	created := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)

	got := beadsMessagesFromIssues([]*beads.Issue{
		{
			ID:        "msg-1",
			Title:     "Hello",
			Status:    "open",
			Priority:  2,
			CreatedAt: created.Format(time.RFC3339),
			Labels:    []string{"gt:message", "from:mayor/"},
		},
		nil,
	})

	if len(got) != 1 {
		t.Fatalf("beadsMessagesFromIssues() returned %d messages, want 1", len(got))
	}
	if got[0].ID != "msg-1" || got[0].Title != "Hello" || got[0].Status != "open" || got[0].Priority != 2 {
		t.Fatalf("beadsMessagesFromIssues() returned unexpected message: %#v", got[0])
	}
	if !got[0].CreatedAt.Equal(created) {
		t.Fatalf("CreatedAt = %v, want %v", got[0].CreatedAt, created)
	}
	if len(got[0].Labels) != 2 || got[0].Labels[0] != "gt:message" || got[0].Labels[1] != "from:mayor/" {
		t.Fatalf("Labels = %#v, want gt:message/from:mayor/", got[0].Labels)
	}
}

// TestIsMessageNotFoundErr: only bd's own absence answer becomes
// ErrMessageNotFound. Text that merely mentions something missing, an id other
// than the one asked for, and an error that did not come from bd are all read
// failures, not missing messages.
func TestIsMessageNotFoundErr(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		err  error
		id   string
		want bool
	}{
		{name: "typed absence", err: beads.ErrNotFound, id: "gt-msg", want: true},
		{name: "text absence", err: &beads.CLIError{Stderr: []byte("Error: no issue found\n")}, id: "gt-msg", want: true},
		{name: "names the id", err: &beads.CLIError{Stderr: []byte("Error: issue gt-msg not found\n")}, id: "gt-msg", want: true},
		{name: "names another id", err: &beads.CLIError{Stderr: []byte("Error: issue gt-other not found\n")}, id: "gt-msg"},
		{name: "unrelated not found", err: &beads.CLIError{Stderr: []byte("Error: config file not found\n")}, id: "gt-msg"},
		{name: "store failure", err: &beads.CLIError{Stderr: []byte("connection refused\n")}, id: "gt-msg"},
		{name: "not from bd", err: fmt.Errorf("issue gt-msg not found"), id: "gt-msg"},
		{name: "wrapped bd answer", err: fmt.Errorf("read: %w", &beads.CLIError{Stderr: []byte("Error: no issue found\n")}), id: "gt-msg", want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := isMessageNotFoundErr(tt.err, tt.id); got != tt.want {
				t.Fatalf("isMessageNotFoundErr(%v, %q) = %v, want %v", tt.err, tt.id, got, tt.want)
			}
		})
	}
}

// TestBdStoreListReadsOnlyJSON: the parser that read bd's list output is the
// seam's now, and what mail owes is the behavior it had — an answer is a JSON
// array, and anything else is a failed read rather than an empty one. The
// "No issues found." text bd prints without --flat used to read as an empty
// mailbox; it now names the failure, since a mailbox that cannot be read is
// not a mailbox with no mail.
func TestBdStoreListReadsOnlyJSON(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		stdout  string
		wantLen int
		wantErr bool
	}{
		{name: "empty output", stdout: "", wantErr: true},
		{name: "human text", stdout: "No issues found.\n", wantErr: true},
		{name: "null", stdout: "null\n", wantErr: true},
		{name: "malformed", stdout: "[{bad json]", wantErr: true},
		{name: "empty array", stdout: "[]\n"},
		{
			name:    "records",
			stdout:  `[{"id":"msg-1","title":"Hello","status":"open","labels":["gt:message"]}]`,
			wantLen: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			bd := &bdScript{answer: func(c bdCall) (string, string, int) {
				if argv := strings.Join(c.Args, " "); argv != "list --json --label=gt:message --limit=0 --flat" {
					return "", "unexpected bd args: " + argv, 1
				}
				return tt.stdout, "", 0
			}}

			issues, err := bdStore(bd.run, t.TempDir(), t.TempDir()).List(beads.ListOptions{Label: "gt:message"})
			if (err != nil) != tt.wantErr {
				t.Fatalf("List error = %v, wantErr %v", err, tt.wantErr)
			}
			if len(issues) != tt.wantLen {
				t.Fatalf("List returned %d issues, want %d", len(issues), tt.wantLen)
			}
		})
	}
}

func TestBdSubprocessEnv_SuppressesAutoImport(t *testing.T) {
	t.Parallel()
	got := bdSubprocessEnv([]string{"PATH=/usr/bin"}, "/tmp/.beads", true)

	if !envContains(got, "BEADS_NO_AUTO_IMPORT=1") {
		t.Fatalf("expected BEADS_NO_AUTO_IMPORT=1 in env, got %v", got)
	}
	if !envContains(got, "BEADS_DIR=/tmp/.beads") {
		t.Fatalf("expected BEADS_DIR to be passed through, got %v", got)
	}
	if !envContains(got, "PATH=/usr/bin") {
		t.Fatalf("expected base env to be preserved, got %v", got)
	}
}

func TestBdSubprocessEnv_BaseEnvCannotOverrideCanonicalPolicy(t *testing.T) {
	t.Parallel()
	got := bdSubprocessEnv([]string{"BEADS_NO_AUTO_IMPORT=0"}, "/tmp/.beads", true)

	value, ok := envLastValue(got, "BEADS_NO_AUTO_IMPORT")
	if !ok {
		t.Fatalf("expected BEADS_NO_AUTO_IMPORT in env, got %v", got)
	}
	if value != "1" {
		t.Fatalf("expected canonical env to win, got BEADS_NO_AUTO_IMPORT=%s in %v", value, got)
	}
}

func TestBdSubprocessEnv_DoesNotMutateBaseEnv(t *testing.T) {
	t.Parallel()
	base := make([]string, 1, 4)
	base[0] = "PATH=/usr/bin"
	backing := base[:cap(base)]
	backing[1] = "SENTINEL=keep"

	_ = bdSubprocessEnv(base, "/tmp/.beads", true)

	if len(base) != 1 {
		t.Fatalf("baseEnv length changed to %d", len(base))
	}
	if backing[1] != "SENTINEL=keep" {
		t.Fatalf("baseEnv backing array was mutated: got %q", backing[1])
	}
}

func TestBdSubprocessEnv_FiltersStaleBdTargetEnv(t *testing.T) {
	t.Parallel()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	metadata := []byte(`{"dolt_database":"rigdb"}`)
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), metadata, 0644); err != nil {
		t.Fatal(err)
	}

	got := bdSubprocessEnv([]string{
		"PATH=/usr/bin",
		"BEADS_DIR=/wrong",
		"BEADS_DB=/wrong.db",
		"BEADS_DOLT_SERVER_DATABASE=wrong",
	}, beadsDir, true)

	if envContains(got, "BEADS_DIR=/wrong") || envContains(got, "BEADS_DB=/wrong.db") || envContains(got, "BEADS_DOLT_SERVER_DATABASE=wrong") {
		t.Fatalf("stale bd target env was not filtered: %v", got)
	}
	if !envContains(got, "BEADS_DIR="+beadsDir) {
		t.Fatalf("expected current BEADS_DIR in env, got %v", got)
	}
	if value, ok := envLastValue(got, "BEADS_DOLT_SERVER_DATABASE"); !ok || value != "rigdb" {
		t.Fatalf("database selector = %q present=%v, want rigdb in %v", value, ok, got)
	}
	for _, want := range []string{"BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off", "BD_EXPORT_AUTO=false", "BD_BACKUP_ENABLED=false", "BD_DOLT_AUTO_PUSH=false", "BD_NO_PUSH=true", "BD_EXPORT_GIT_ADD=false", "BD_NO_GIT_OPS=true"} {
		if !envContains(got, want) {
			t.Fatalf("expected %s in env, got %v", want, got)
		}
	}
}

func TestBdSubprocessEnv_WriteCommandsAreNotReadonly(t *testing.T) {
	t.Parallel()
	got := bdSubprocessEnv([]string{"PATH=/usr/bin", "BD_READONLY=true"}, "/tmp/.beads", false)
	if value, ok := envLastValue(got, "BD_READONLY"); ok {
		t.Fatalf("write command env should not inherit or set BD_READONLY, got %q in %v", value, got)
	}
	for _, want := range []string{"BD_DOLT_AUTO_COMMIT=on", "BD_EXPORT_AUTO=false", "BD_BACKUP_ENABLED=false", "BD_DOLT_AUTO_PUSH=false", "BD_NO_PUSH=true", "BD_EXPORT_GIT_ADD=false", "BD_NO_GIT_OPS=true"} {
		if !envContains(got, want) {
			t.Fatalf("expected %s in env, got %v", want, got)
		}
	}
}

func TestBdSubprocessEnv_ReadonlyCannotBeOverridden(t *testing.T) {
	t.Parallel()
	got := bdSubprocessEnv([]string{"PATH=/usr/bin", "BD_READONLY=false"}, "/tmp/.beads", true)
	if value, ok := envLastValue(got, "BD_READONLY"); !ok || value != "true" {
		t.Fatalf("read command env should force BD_READONLY=true, got %q present=%v in %v", value, ok, got)
	}
}

func TestArgsAreReadOnlyForMailCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		args []string
		want bool
	}{
		{[]string{"list", "--json"}, true},
		{[]string{"show", "hq-abc"}, true},
		{[]string{"sql", "--json", "SELECT * FROM wisps"}, true},
		{[]string{"mol", "wisp", "list", "--json"}, true},
		{[]string{"message", "thread", "hq-abc", "--json"}, true},
		{[]string{"sql", "UPDATE issues SET status='closed'"}, false},
		{[]string{"sql", "--json", "WITH x AS (SELECT 1) SELECT * FROM x"}, false},
		{[]string{"mol", "wisp", "create", "mol-test"}, false},
		{[]string{"message", "send", "mayor", "--body", "hi"}, false},
		{[]string{"create", "title"}, false},
		{[]string{"close", "hq-abc"}, false},
		{[]string{"label", "add", "hq-abc", "read"}, false},
	}
	for _, tt := range tests {
		if got := beads.ArgsAreReadOnly(tt.args); got != tt.want {
			t.Fatalf("beads.ArgsAreReadOnly(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

// TestBdStoresHandBdTheCentralEnvPolicy: each store type hands bd the
// environment its access mode promises, over whatever the process carries, and
// the directory bd runs in. The stores no longer build their own argv, so what
// is pinned here is the environment and cwd bd receives.
func TestBdStoresHandBdTheCentralEnvPolicy(t *testing.T) {
	t.Parallel()
	bd := &bdScript{answer: func(c bdCall) (string, string, int) {
		if strings.HasPrefix(strings.Join(c.Args, " "), "update ") {
			return "", "", 0
		}
		return "[]\n", "", 0
	}}

	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"maildb"}`), 0644); err != nil {
		t.Fatal(err)
	}
	workDir := filepath.Dir(beadsDir)

	if _, err := bdStore(bd.run, workDir, beadsDir).List(beads.ListOptions{Label: "gt:message"}); err != nil {
		t.Fatalf("read through the store: %v", err)
	}
	if err := bdWriteStore(bd.run, workDir, beadsDir).Update("hq-msg", beads.UpdateOptions{AddLabels: []string{"read"}}); err != nil {
		t.Fatalf("write through the store: %v", err)
	}
	calls := bd.recorded()
	if len(calls) != 2 {
		t.Fatalf("bd calls = %q, want 2", bd.argvs())
	}
	read, write := calls[0], calls[1]
	for _, call := range calls {
		if call.Dir != workDir {
			t.Errorf("bd ran in %q, want %q", call.Dir, workDir)
		}
	}
	// BD_MACHINE and PWD are what beads.WithMachineEnvIn contributes to the
	// environment bd sees; the rest is bdSubprocessEnv's routing policy.
	for _, want := range []string{"BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off", "BEADS_NO_AUTO_IMPORT=1", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb", "BD_MACHINE=1", "PWD=" + workDir} {
		if v, _ := envLastValue(read.Env, strings.SplitN(want, "=", 2)[0]); strings.SplitN(want, "=", 2)[0]+"="+v != want {
			t.Errorf("read env %s = %q, want %q", strings.SplitN(want, "=", 2)[0], v, want)
		}
	}
	for _, want := range []string{"BD_DOLT_AUTO_COMMIT=on", "BEADS_NO_AUTO_IMPORT=1", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb", "BD_MACHINE=1", "PWD=" + workDir} {
		if v, _ := envLastValue(write.Env, strings.SplitN(want, "=", 2)[0]); strings.SplitN(want, "=", 2)[0]+"="+v != want {
			t.Errorf("write env %s = %q, want %q", strings.SplitN(want, "=", 2)[0], v, want)
		}
	}
	if v, ok := envLastValue(write.Env, "BD_READONLY"); ok && v != "" {
		t.Errorf("write env BD_READONLY = %q, want unset or empty", v)
	}
}

func TestBdSubprocessEnv_AllowsRoutingWhenBeadsDirEmpty(t *testing.T) {
	t.Parallel()
	got := bdSubprocessEnv([]string{
		"PATH=/usr/bin",
		"GT_DOLT_HOST=127.0.0.2",
		"GT_DOLT_PORT=5507",
		"BEADS_DIR=/wrong",
		"BEADS_DB=/wrong.db",
		"BEADS_DOLT_SERVER_DATABASE=wrong",
		"BEADS_DOLT_SERVER_HOST=wrong-host",
		"BEADS_DOLT_SERVER_PORT=9999",
		"BEADS_DOLT_PORT=9999",
	}, "", true)

	for _, key := range []string{"BEADS_DIR", "BEADS_DB", "BEADS_DOLT_SERVER_DATABASE"} {
		if value, ok := envLastValue(got, key); ok {
			t.Fatalf("expected %s to be absent for routed command, got %q in %v", key, value, got)
		}
	}
	if !envContains(got, "BEADS_NO_AUTO_IMPORT=1") {
		t.Fatalf("expected BEADS_NO_AUTO_IMPORT=1 in env, got %v", got)
	}
	// Outside a town the inherited bd endpoint passes through; GT_DOLT_* is
	// never translated (gt-y3pgh.3).
	if !envContains(got, "BEADS_DOLT_SERVER_HOST=wrong-host") || !envContains(got, "BEADS_DOLT_SERVER_PORT=9999") || envContains(got, "BEADS_DOLT_SERVER_PORT=5507") {
		t.Fatalf("expected the inherited bd endpoint for routed command, got %v", got)
	}
}

// TestBdSubprocessEnvPinnedReadAndWritePolicy is a pure, subprocess-free unit
// test of the env-policy decision TestBdStoresHandBdTheCentralEnvPolicy
// exercises through the stores. This one proves the policy itself
// deterministically, whatever the host.
func TestBdSubprocessEnvPinnedReadAndWritePolicy(t *testing.T) {
	t.Parallel()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	if err := os.MkdirAll(beadsDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(beadsDir, "metadata.json"), []byte(`{"dolt_database":"maildb"}`), 0644); err != nil {
		t.Fatal(err)
	}

	readEnv := bdSubprocessEnv([]string{"BD_READONLY=false", "BD_DOLT_AUTO_COMMIT=on"}, beadsDir, true)
	for _, want := range []string{"BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off", "BEADS_NO_AUTO_IMPORT=1", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb"} {
		if !envContains(readEnv, want) {
			t.Fatalf("read env missing %q:\n%v", want, readEnv)
		}
	}

	writeEnv := bdSubprocessEnv([]string{"BD_READONLY=true", "BD_DOLT_AUTO_COMMIT=off"}, beadsDir, false)
	for _, want := range []string{"BD_DOLT_AUTO_COMMIT=on", "BEADS_NO_AUTO_IMPORT=1", "BEADS_DIR=" + beadsDir, "BEADS_DOLT_SERVER_DATABASE=maildb"} {
		if !envContains(writeEnv, want) {
			t.Fatalf("write env missing %q:\n%v", want, writeEnv)
		}
	}
	if value, ok := envLastValue(writeEnv, "BD_READONLY"); ok && value != "" {
		t.Fatalf("expected BD_READONLY unset/empty for write env, got %q", value)
	}
}

func envContains(env []string, kv string) bool {
	for _, entry := range env {
		if entry == kv {
			return true
		}
	}
	return false
}

func envLastValue(env []string, key string) (string, bool) {
	prefix := key + "="
	var value string
	found := false
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			value = strings.TrimPrefix(entry, prefix)
			found = true
		}
	}
	return value, found
}
