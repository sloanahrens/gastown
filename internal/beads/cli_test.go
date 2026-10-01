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
		case "sql SELECT 1 FROM `nosuch` LIMIT 1":
			return reply{stderr: "Error: table not found: nosuch", err: exitError{1}}
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
	if !b.TableExists("issues") || b.TableExists("nosuch") {
		t.Error("TableExists wrong")
	}
	if _, err := b.StatsJSON(); err != nil {
		t.Fatal(err)
	}
	if err := b.InitDatabase(InitOptions{Prefix: "gt", Database: "gastown", ServerPort: 3307, Force: true, DestroyToken: "DESTROY-gt"}); err != nil {
		t.Fatal(err)
	}
	if err := b.InitDatabase(InitOptions{Database: "hq"}); err != nil {
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
		"sql SELECT 1 FROM `issues` LIMIT 1",
		"sql SELECT 1 FROM `nosuch` LIMIT 1",
		"stats --json",
		"init --prefix gt --database gastown --server --server-port 3307 --force --destroy-token=DESTROY-gt",
		"init --database hq --server",
		"init --prefix gt --server --skip-agents --reinit-local --discard-remote --destroy-token=DESTROY-gt",
		"migrate --update-repo-id",
		"mol wisp gc --dry-run --json --age 1h30m0s",
	}
	if got := r.argvs(); !reflect.DeepEqual(got, want) {
		t.Errorf("argv:\n got %q\nwant %q", got, want)
	}
	for _, c := range r.calls() {
		// Machine mode rides on every call but bd sql (machineExempt).
		wantEnv := []string{"K=V", "BD_MACHINE=1"}
		if c.args[0] == "sql" {
			wantEnv = []string{"K=V"}
		}
		if !c.plain || !reflect.DeepEqual(c.env, wantEnv) {
			t.Errorf("call %v not plain with the given env: plain=%v env=%v, want %v", c.args, c.plain, c.env, wantEnv)
		}
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
	for name, out := range map[string]string{
		"object":  bd122,
		"array":   `[{"id":"gt-wisp-u7r","status":"open","updated_at":"2026-09-29T00:33:04Z"}]`,
		"no wisp": `{"count":0,"schema_version":1,"wisps":[]}`,
	} {
		r := newRecorder(func([]string) reply { return reply{stdout: out} })
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
