package townstate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/manifestdata"
)

// TestSecretShape is the shape itself: the four acceptance names are caught,
// config.json and town.json are not, and the test is on the lower-cased base
// name whatever directory the file sits in.
func TestSecretShape(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label string
		name  string
		want  bool
	}{
		{"an env file", "daemon.env", true},
		{"a key file", "x.key", true},
		{"a token file", "forgejo-token.txt", true},
		{"a pem file", "a.pem", true},
		{"a config file", "config.json", false},
		{"the town file", "town.json", false},
		{"a bare env name", ".env", true},
		{"a dsn file", "ledger-reader.dsn", true},
		{"a path below a directory", "settings/daemon.env", true},
		{"upper case", "DAEMON.ENV", true},
		{"a mixed-case path", "MAYOR/Token.txt", true},
		{"a secret in the middle", "my-secret-notes.md", true},
		{"a note", "notes.md", false},
		{"an ignore file", ".gitignore", false},
		{"the empty name", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			if got := SecretShape(tc.name); got != tc.want {
				t.Errorf("SecretShape(%q) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}

// secretsManifestShapeViolations names every mounted secret a backup would
// carry verbatim because its name does not satisfy SecretShape. It reads the
// mounted files list alone: an unclaimed name is recorded, never mounted and
// never in a backup row, so the shape is not applied to it.
func secretsManifestShapeViolations(m manifestdata.SecretsManifest) []string {
	var out []string
	for _, f := range m.Files {
		if !SecretShape(f.Name) {
			out = append(out, fmt.Sprintf("mounted secret %q is not secret-shaped; rename it or extend SecretShape, since a backup carries it verbatim", f.Name))
		}
	}
	return out
}

// TestSecretsManifestShapeGuard drives the manifest guard on fixture manifests,
// so a rule that stops refusing turns the guard red: a mounted secret without
// the shape fails, and one of a non-credential kind is not excused.
func TestSecretsManifestShapeGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label string
		m     manifestdata.SecretsManifest
		want  string
	}{
		{
			label: "a mounted secret with the shape passes",
			m:     manifestdata.SecretsManifest{Files: []manifestdata.SecretFile{{Name: "forgejo-landing.env"}}},
		},
		{
			label: "a mounted secret with an odd name fails",
			m:     manifestdata.SecretsManifest{Files: []manifestdata.SecretFile{{Name: "forgejo-landing"}}},
			want:  "forgejo-landing",
		},
		{
			label: "a mounted name of a non-credential kind still fails",
			m:     manifestdata.SecretsManifest{Files: []manifestdata.SecretFile{{Name: "cloud-creds.json"}}},
			want:  "cloud-creds.json",
		},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			violations := secretsManifestShapeViolations(tc.m)
			if tc.want == "" {
				if len(violations) != 0 {
					t.Fatalf("violations = %v, want none", violations)
				}
				return
			}
			if len(violations) == 0 {
				t.Fatalf("violations = none, want one naming %q", tc.want)
			}
			if !strings.Contains(strings.Join(violations, "\n"), tc.want) {
				t.Errorf("violations = %v, want one naming %q", violations, tc.want)
			}
		})
	}
}

// TestSecretsManifestNamesAreSecretShaped holds deploy/daemon/secrets.yaml to the
// guard: a mounted secret with an odd name fails here, so a new secret is
// caught at the manifest before a backup carries it verbatim.
func TestSecretsManifestNamesAreSecretShaped(t *testing.T) {
	t.Parallel()
	file := filepath.Join("..", "..", "deploy", "daemon", "secrets.yaml")
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	m, err := manifestdata.ParseSecretsManifest(data)
	if err != nil {
		t.Fatalf("parsing %s: %v", file, err)
	}
	for _, violation := range secretsManifestShapeViolations(m) {
		t.Error(violation)
	}
}

// secretShapedRows names the rows whose own path the backup's exclusion rules
// catch. The export drops such a row whole, counting one skip and naming
// nothing, so a tagged row of that shape leaves the archive without a word.
func secretShapedRows(rows []Entry) []string {
	var out []string
	for _, e := range rows {
		if secretPath(e.Path) {
			out = append(out, e.Path)
		}
	}
	return out
}

// TestBackupSetRowsAreNotSecretShaped holds the manifest's backup set to the
// shape: the rows are the manifest's own (placeholders unexpanded), and one a
// backup would drop whole must lose the tag in deploy/daemon/town-state.yaml or
// be renamed so the shape no longer matches it.
func TestBackupSetRowsAreNotSecretShaped(t *testing.T) {
	t.Parallel()
	for _, p := range secretShapedRows(Default().Backup()) {
		t.Errorf("the backup row %q has a secret shape; a backup drops it whole and names nothing, so untag it or make the shape exact", p)
	}
}

// TestBackupSetRowShapeGuard drives both branches of the backup-set check on
// fixture rows: a row the shape catches fails the guard, and a row it does not
// pass.
func TestBackupSetRowShapeGuard(t *testing.T) {
	t.Parallel()
	cases := []struct {
		label string
		rows  []Entry
		want  []string
	}{
		{"an ordinary row passes", []Entry{{Path: "settings"}}, nil},
		{"a secret-shaped row fails", []Entry{{Path: "secrets"}}, []string{"secrets"}},
		{"a row below a secret-shaped segment fails", []Entry{{Path: "store/token-store/session.json"}}, []string{"store/token-store/session.json"}},
		{"a rig row passes unexpanded", []Entry{{Path: "<rig>/settings"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			t.Parallel()
			got := secretShapedRows(tc.rows)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("secretShapedRows(%v) = %v, want %v", tc.rows, got, tc.want)
			}
		})
	}
}
