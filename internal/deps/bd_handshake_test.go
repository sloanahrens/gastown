package deps

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

// fakeExit is a bd exit status as a BDRunner reports it.
type fakeExit int

func (e fakeExit) Error() string { return fmt.Sprintf("exit status %d", int(e)) }
func (e fakeExit) ExitCode() int { return int(e) }

// Payloads captured from real builds on 2026-09-29.
const (
	// The town's installed fork build before the D1 machine surface.
	bdVersion537accb = `{"build":"537accb","commit":"537accbea9ca6f2be8d478ac97c5137bce4ac1b2","schema_version":1,"version":"1.2.2"}`
	// The fork build at beads da4983e.
	bdVersionDa4983e = `{"build":"da4983e","build_id":"da4983e","commit":"da4983e","contract_version":1,"db_schema_version":66,"schema_ceiling":{"ignored":25,"main":66},"schema_version":1,"version":"1.2.2"}`
	// The same payload under BD_MACHINE=1.
	bdVersionDa4983eEnvelope = `{"schema_version":1,"contract_version":1,"data":` + bdVersionDa4983e + `,"pagination":null,"error":null}`
	// Upstream steveyegge/beads: a semver and nothing else.
	bdVersionUpstream = `{"version":"1.0.5","build":"release"}`
)

type fakeBD struct {
	version    string
	versionErr error
	sqlOut     string
	sqlErr     error
	sqlStderr  string
	calls      [][]string
	envs       [][]string
}

func (f *fakeBD) run(_ context.Context, env []string, args ...string) ([]byte, []byte, error) {
	f.calls = append(f.calls, args)
	f.envs = append(f.envs, env)
	switch args[0] {
	case "version":
		return []byte(f.version), nil, f.versionErr
	case "sql":
		return []byte(f.sqlOut), []byte(f.sqlStderr), f.sqlErr
	}
	return nil, nil, fmt.Errorf("unexpected bd call %v", args)
}

func dbLevel(n int) string { return fmt.Sprintf(`[{"version": %d}]`, n) }

func TestParseBDVersionJSON(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  string
		want BDVersionInfo
	}{
		{"fork da4983e", bdVersionDa4983e, BDVersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}},
		{"machine envelope", bdVersionDa4983eEnvelope, BDVersionInfo{Version: "1.2.2", Build: "da4983e", BuildID: "da4983e", Commit: "da4983e", ContractVersion: 1, DBSchemaVersion: 66}},
		{"installed 537accb", bdVersion537accb, BDVersionInfo{Version: "1.2.2", Build: "537accb", Commit: "537accbea9ca6f2be8d478ac97c5137bce4ac1b2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := ParseBDVersionJSON([]byte(tc.out))
			if err != nil {
				t.Fatalf("ParseBDVersionJSON: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
	for _, bad := range []string{"", "bd version 1.2.2 (537accb)", "[]", `{"version":""}`} {
		if _, err := ParseBDVersionJSON([]byte(bad)); err == nil {
			t.Errorf("ParseBDVersionJSON(%q): want error", bad)
		}
	}
}

func TestCheckBDHandshake_AcceptsForkBuildAtDBLevel(t *testing.T) {
	t.Parallel()
	f := &fakeBD{version: bdVersionDa4983e, sqlOut: dbLevel(66)}
	hs, err := CheckBDHandshake(context.Background(), "/bin/bd", f.run)
	if err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
	if hs.Found.BuildID != "da4983e" || hs.DBSchema != 66 || hs.Found.ContractVersion != 1 {
		t.Fatalf("handshake = %+v", hs)
	}
	sql := f.calls[1]
	if sql[0] != "sql" || !strings.Contains(strings.Join(sql, " "), "schema_migrations") {
		t.Fatalf("DB level not read through bd sql: %v", f.calls)
	}
	if !containsEnv(f.envs[1], "BD_MACHINE=1") {
		t.Fatalf("bd sql must run in machine mode so failures carry a typed exit: env %v", f.envs[1])
	}
}

func TestCheckBDHandshake_DBLevelAsString(t *testing.T) {
	t.Parallel()
	f := &fakeBD{version: bdVersionDa4983e, sqlOut: `{"schema_version":1,"contract_version":1,"data":[{"version":"66"}],"pagination":null,"error":null}`}
	if _, err := CheckBDHandshake(context.Background(), "/bin/bd", f.run); err != nil {
		t.Fatalf("CheckBDHandshake: %v", err)
	}
}

func TestCheckBDHandshake_Refusals(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		bd        *fakeBD
		wantParts []string
		noDBRead  bool
	}{
		{
			name:      "installed pre-D1 fork build",
			bd:        &fakeBD{version: bdVersion537accb},
			wantParts: []string{"537accb", "no contract_version", "no db_schema_version"},
			noDBRead:  true,
		},
		{
			name:      "upstream build",
			bd:        &fakeBD{version: bdVersionUpstream},
			wantParts: []string{"1.0.5", "no build_id"},
			noDBRead:  true,
		},
		{
			name:      "unknown contract",
			bd:        &fakeBD{version: strings.Replace(bdVersionDa4983e, `"contract_version":1`, `"contract_version":2`, 1)},
			wantParts: []string{"contract 2", "known: 1"},
			noDBRead:  true,
		},
		{
			name:      "database behind bd",
			bd:        &fakeBD{version: bdVersionDa4983e, sqlOut: dbLevel(65)},
			wantParts: []string{"schema<=66", "database at 65"},
		},
		{
			name:      "database ahead of bd",
			bd:        &fakeBD{version: bdVersionDa4983e, sqlErr: fakeExit(26), sqlStderr: "schema skew"},
			wantParts: []string{"database schema is ahead of this bd"},
		},
		{
			name:      "database unreadable",
			bd:        &fakeBD{version: bdVersionDa4983e, sqlErr: fakeExit(25), sqlStderr: "store unavailable"},
			wantParts: []string{"could not read the database migration level", "store unavailable"},
		},
		{
			name:      "database answer not JSON",
			bd:        &fakeBD{version: bdVersionDa4983e, sqlOut: "No rows."},
			wantParts: []string{"could not read the database migration level", "No rows."},
		},
		{
			name:      "version command fails",
			bd:        &fakeBD{versionErr: fakeExit(2)},
			wantParts: []string{"bd version --json failed"},
			noDBRead:  true,
		},
		{
			name:      "bd missing",
			bd:        &fakeBD{versionErr: &exec.Error{Name: "bd", Err: exec.ErrNotFound}},
			wantParts: []string{"not found on PATH"},
			noDBRead:  true,
		},
		{
			name:      "version text only",
			bd:        &fakeBD{version: "bd version 0.57.0"},
			wantParts: []string{"not JSON"},
			noDBRead:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := CheckBDHandshake(context.Background(), "/usr/local/bin/bd", tc.bd.run)
			if err == nil {
				t.Fatal("want refusal, got nil")
			}
			if !errors.Is(err, ErrBDHandshake) {
				t.Errorf("refusal does not wrap ErrBDHandshake: %v", err)
			}
			msg := err.Error()
			for _, part := range append(tc.wantParts, "/usr/local/bin/bd", "make safe-install", "contract 1") {
				if !strings.Contains(msg, part) {
					t.Errorf("refusal missing %q:\n%s", part, msg)
				}
			}
			if strings.Contains(msg, "go install") {
				t.Errorf("refusal must never suggest go install:\n%s", msg)
			}
			if tc.noDBRead && len(tc.bd.calls) > 1 {
				t.Errorf("database was read after the version was refused: %v", tc.bd.calls)
			}
		})
	}
}

func containsEnv(env []string, kv string) bool {
	for _, e := range env {
		if e == kv {
			return true
		}
	}
	return false
}
