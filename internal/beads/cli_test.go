package beads

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/beadsql"
)

func newPlainRecorded(t *testing.T, r *recorder) *Beads {
	t.Helper()
	b := NewPlain(t.TempDir(), []string{"K=V"})
	b.exec = r.exec
	return b
}

func TestCLIMethodsSendTheirArgv(t *testing.T) {
	t.Parallel()
	r := newRecorder(func(args []string) reply {
		switch strings.Join(args, " ") {
		case "config get types.custom":
			return reply{stdout: "Note: shared server\nagent,rig\n"}
		case "config get routing.mode":
			return reply{stdout: "routing.mode (not set)\n"}
		case "sql --csv SELECT COUNT(*) as cnt FROM issues":
			return reply{stdout: "cnt\n7\n", stderr: "warning: noise on stderr\n"}
		case "mol wisp gc --dry-run --json --age 1h30m0s":
			return reply{stdout: `{"cleaned_ids":[],"cleaned_count":0,"dry_run":true}`}
		}
		return reply{}
	})
	b := newPlainRecorded(t, r)
	if v, err := b.ConfigGet("types.custom"); err != nil || v != "agent,rig" {
		t.Errorf("ConfigGet = %q, %v", v, err)
	}
	if v, err := b.ConfigGet("routing.mode"); err != nil || v != "" {
		t.Errorf("ConfigGet(unset) = %q, %v; want \"\"", v, err)
	}
	if err := b.ConfigSet("types.infra", "agent"); err != nil {
		t.Fatal(err)
	}
	if n, err := b.CountIssues(); err != nil || n != 7 {
		t.Errorf("CountIssues = %d, %v", n, err)
	}
	if _, err := b.StatsJSON(); err != nil {
		t.Fatal(err)
	}
	if err := b.InitDatabase(InitOptions{Prefix: "gt", Database: "gastown", ServerPort: 3307, Force: true, DestroyToken: "DESTROY-gt"}); err != nil {
		t.Fatal(err)
	}
	if err := b.InitDatabase(InitOptions{Database: "hq", ServerHost: "dolt.example", ServerPort: 3307}); err != nil {
		t.Fatal(err)
	}
	if err := b.InitDatabase(InitOptions{Prefix: "gt", SkipAgents: true, ReinitLocal: true, DiscardRemote: true, DestroyToken: "DESTROY-gt"}); err != nil {
		t.Fatal(err)
	}
	if err := b.MigrateRepoID(); err != nil {
		t.Fatal(err)
	}
	if _, err := b.WispGCCandidates(90 * time.Minute); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"config get types.custom",
		"config get routing.mode",
		"config set types.infra agent",
		"sql --csv SELECT COUNT(*) as cnt FROM issues",
		"stats --json",
		"init --prefix gt --database gastown --server --server-port 3307 --force --destroy-token=DESTROY-gt",
		"init --database hq --server --server-host dolt.example --server-port 3307",
		"init --prefix gt --server --skip-agents --reinit-local --discard-remote --destroy-token=DESTROY-gt",
		"migrate --update-repo-id",
		"mol wisp gc --dry-run --json --age 1h30m0s",
	}
	if got := r.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("argv:\n got %q\nwant %q", got, want)
	}
	for _, c := range r.calls() {
		// Machine mode rides on every call but bd sql (machineExempt); a bd
		// init also carries the new-database opt-in, pinned by
		// new_database_optin_test.go.
		wantEnv := []string{"K=V", "BD_MACHINE=1"}
		if c.args[0] == "sql" {
			wantEnv = []string{"K=V"}
		}
		wantEnv = applyNewDatabaseOptIn(wantEnv, c.args)
		if !c.plain || !reflect.DeepEqual(c.env, wantEnv) {
			t.Errorf("call %v not plain with the given env: plain=%v env=%v, want %v", c.args, c.plain, c.env, wantEnv)
		}
	}
}

// ConfigGet answers from the store the wrapper is pinned to: bd runs with
// BEADS_DIR set to that store and any inherited BD_EVENTS_JOURNAL cleared, so
// the caller's environment cannot make a store whose journal is off answer as
// one whose journal is on (gt-7iwy0.7). An unset key is "" with no error —
// bd's contract, which gt tail reports as a journal that is off — and a failed
// bd is that failure, never "".
func TestConfigGetPinsTheStoreAndPassesBdFailureThrough(t *testing.T) {
	t.Parallel()
	beadsDir := t.TempDir()
	r := newRecorder(func([]string) reply {
		return reply{stdout: `{"key":"events-journal","value":"false"}`}
	})
	b := newBeads(beadsFields{
		workDir:  t.TempDir(),
		beadsDir: beadsDir,
		baseEnv:  []string{"BD_EVENTS_JOURNAL=1", "BEADS_DIR=/somewhere/else", "PATH=/usr/bin"},
		exec:     r.exec,
	})
	v, err := b.ConfigGet("events-journal")
	if err != nil || v != "false" {
		t.Fatalf("ConfigGet = %q, %v", v, err)
	}
	calls := r.calls()
	if len(calls) != 1 {
		t.Fatalf("bd calls = %v", r.argvs())
	}
	if want := []string{"config", "get", "events-journal"}; !reflect.DeepEqual(calls[0].args, want) {
		t.Fatalf("argv = %v, want %v", calls[0].args, want)
	}
	if got, ok := lastEnvValue(calls[0].env, "BEADS_DIR"); !ok || got != ResolveBeadsDir(beadsDir) {
		t.Errorf("BEADS_DIR = %q (set %v), want the store's %q", got, ok, ResolveBeadsDir(beadsDir))
	}
	if got, ok := lastEnvValue(calls[0].env, "BD_EVENTS_JOURNAL"); ok {
		t.Errorf("BD_EVENTS_JOURNAL = %q reached bd; the store's config must answer alone", got)
	}
	if got, _ := lastEnvValue(calls[0].env, "BD_MACHINE"); got != "1" {
		t.Errorf("BD_MACHINE = %q, want 1: an unset key then reads as value \"\"", got)
	}

	failing := newRecorder(func([]string) reply {
		return reply{stderr: "database not found\n", err: errors.New("exit status 1")}
	})
	f := newBeads(beadsFields{workDir: t.TempDir(), beadsDir: beadsDir, exec: failing.exec})
	if _, err := f.ConfigGet("events-journal"); err == nil || !strings.Contains(err.Error(), "database not found") {
		t.Fatalf("bd failure = %v, want bd's stderr in the error", err)
	}
}

func TestSQLCSVParsesStdoutOnly(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stdout: "id,title\ngt-1,\"a, b\"\n", stderr: "Note: routed via shared server\n"}
	})
	got, err := newPlainRecorded(t, r).SQLCSV(beadsql.EphemeralIssues())
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"id", "title"}, {"gt-1", "a, b"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("records = %q, want %q", got, want)
	}
}

func TestSQLCSVErrorCarriesStderr(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stderr: "Error: syntax error near FROM\n", err: exitError{1}}
	})
	_, err := newPlainRecorded(t, r).SQLCSV(beadsql.EphemeralIssues())
	if err == nil || !strings.Contains(err.Error(), "syntax error near FROM") {
		t.Errorf("err = %v, want bd's stderr in it", err)
	}
}

// TestMolWispListReadsBdObject pins the shape bd 1.2.2 prints for
// `bd mol wisp list --json` (captured from the Dolt test container). The
// doctor's wisp-gc check unmarshalled it as a bare array, which always
// failed, so it counted zero abandoned wisps on every run.
func TestMolWispListReadsBdObject(t *testing.T) {
	t.Parallel()
	const bd122 = `{
  "count": 1,
  "schema_version": 1,
  "wisps": [
    {
      "created_at": "2026-09-29T00:33:04Z",
      "id": "gt-wisp-u7r",
      "priority": 2,
      "status": "open",
      "title": "wisp",
      "type": "task",
      "updated_at": "2026-09-29T00:33:04Z"
    }
  ]
}`
	for name, tt := range map[string]struct {
		out      string
		wantType string
	}{
		"object": {bd122, "task"},
		"array":  {`[{"id":"gt-wisp-u7r","status":"open","updated_at":"2026-09-29T00:33:04Z"}]`, ""},
		// A wisp printed under both spellings keeps issue_type, the one bd
		// list uses and the field Issue.Type documents.
		"both spellings": {`{"wisps":[{"id":"gt-wisp-u7r","status":"open","updated_at":"2026-09-29T00:33:04Z","type":"task","issue_type":"feature"}]}`, "feature"},
		"no wisp":        {`{"count":0,"schema_version":1,"wisps":[]}`, ""},
	} {
		r := newRecorder(func([]string) reply { return reply{stdout: tt.out} })
		got, err := newPlainRecorded(t, r).MolWispList()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if name == "no wisp" {
			if len(got) != 0 {
				t.Errorf("%s: %v", name, got)
			}
			continue
		}
		if len(got) != 1 || got[0].ID != "gt-wisp-u7r" || got[0].Status != "open" || got[0].UpdatedAt != "2026-09-29T00:33:04Z" || !got[0].Ephemeral {
			t.Errorf("%s: MolWispList = %+v", name, got)
		}
		// bd prints a wisp's type under "type", not list's "issue_type"; a
		// caller reading Issue.Type must see it either way.
		if got[0].Type != tt.wantType {
			t.Errorf("%s: Type = %q, want %q", name, got[0].Type, tt.wantType)
		}
		if argv := r.argvs(); len(argv) != 1 || argv[0] != "mol wisp list --json" {
			t.Errorf("%s: argv = %q", name, argv)
		}
	}
	r := newRecorder(func([]string) reply { return reply{stderr: "Error: no database", err: exitError{1}} })
	var cliErr *CLIError
	if _, err := newPlainRecorded(t, r).MolWispList(); !errors.As(err, &cliErr) {
		t.Errorf("failure = %v, want *CLIError", err)
	}
}

// TestWispGCCandidatesReadsDryRun pins the dry-run object bd prints
// (cmd/bd/wisp.go WispGCResult) and that only a dry run is ever sent.
func TestWispGCCandidatesReadsDryRun(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stdout: `{"cleaned_ids":["gt-wisp-a","gt-wisp-b"],"cleaned_count":0,"candidates":2,"dry_run":true}`}
	})
	got, err := newPlainRecorded(t, r).WispGCCandidates(time.Hour)
	if err != nil || !reflect.DeepEqual(got, []string{"gt-wisp-a", "gt-wisp-b"}) {
		t.Fatalf("WispGCCandidates = %v, %v", got, err)
	}
	if argv := r.argvs(); len(argv) != 1 || argv[0] != "mol wisp gc --dry-run --json --age 1h0m0s" {
		t.Errorf("argv = %q", argv)
	}
}

// SetIssuePrefix runs bd's config-only rename: bd config set refuses the key,
// and a plain rename rewrites every issue id (beads be-qr3, gt-fcxe9.11).
// gastown passes the prefix without the trailing hyphen; bd requires it.
func TestSetIssuePrefixSendsConfigOnlyRename(t *testing.T) {
	t.Parallel()
	r := newRecorder(nil)
	if err := newPlainRecorded(t, r).SetIssuePrefix("gt"); err != nil {
		t.Fatalf("SetIssuePrefix: %v", err)
	}
	want := "rename-prefix --config-only gt-"
	if argv := r.argvs(); len(argv) != 1 || argv[0] != want {
		t.Errorf("argv = %q, want [%s]", argv, want)
	}
}

// bd refuses the config-only rename (exit 21) when an issue id would have to
// be rewritten. The refusal is the answer the caller acts on, not a silent
// half-migration of the config cell.
func TestSetIssuePrefixReportsBDRefusal(t *testing.T) {
	t.Parallel()
	r := newRecorder(func([]string) reply {
		return reply{stderr: "Error: prefix gt- would rewrite 3 issue ids", err: exitError{21}}
	})
	err := newPlainRecorded(t, r).SetIssuePrefix("gt")
	if err == nil || !strings.Contains(err.Error(), "rewrite 3 issue ids") {
		t.Fatalf("SetIssuePrefix = %v, want bd's refusal", err)
	}
}
