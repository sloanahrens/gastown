package manifestdata

import (
	"strings"
	"testing"
)

// TestParseRefusesInvalidManifests drives every refusal of the split's parser:
// each row that cannot name a town-root path or owner is named in the error.
func TestParseRefusesInvalidManifests(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		yaml string
		want string
	}{
		{"malformed YAML", "entries: [unclosed\n", "reading the town-state manifest"},
		{"unknown owner", "entries:\n  - path: logs\n    owner: root\n    why: who is root\n", `unknown owner "root"`},
		{"empty path", "entries:\n  - path: \"\"\n    owner: daemon\n    why: no path\n", "has an empty path"},
		{"absolute path", "entries:\n  - path: /Users/sloan/gt/settings\n    owner: daemon\n    why: absolute\n", "relative to the town root"},
		{"empty why", "entries:\n  - path: logs\n    owner: daemon\n    why: \"\"\n", "why is empty"},
		{"rollback on a session row", "entries:\n  - path: gastown/crew\n    owner: session\n    why: a tree\n    rollback: true\n", "rollback: true needs a daemon owner"},
		{"private on a shared row", "entries:\n  - path: settings\n    owner: shared\n    why: edited\n    private: true\n", "private: true needs a daemon owner"},
		{"empty path segment", "entries:\n  - path: logs//x\n    owner: daemon\n    why: double slash\n", "empty path segment"},
		{"parent segment", "entries:\n  - path: ../elsewhere\n    owner: daemon\n    why: escapes\n", `".." is not a town-root path segment`},
		{"unknown placeholder", "entries:\n  - path: <rigx>/crew\n    owner: session\n    why: a typo\n", `unknown placeholder "<rigx>"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := Parse([]byte(tc.yaml)); err == nil {
				t.Fatal("Parse accepted a manifest it must refuse")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestParseReadsTheManifest keeps the accepted form pinned: a row that names a
// town-root path parses and validates.
func TestParseReadsTheManifest(t *testing.T) {
	t.Parallel()
	m, err := Parse([]byte("entries:\n  - path: logs\n    owner: daemon\n    why: daemon logs\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(m.Entries) != 1 || m.Entries[0].Path != "logs" || m.Entries[0].Owner != OwnerDaemon {
		t.Errorf("entries = %+v, want the manifest's one row", m.Entries)
	}
	if err := m.Validate(); err != nil {
		t.Errorf("Validate refused a manifest Parse accepted: %v", err)
	}
}

// TestManifestValidateRefusesABadRow proves the manifest-level check reaches
// the row check, so a caller that holds a Manifest rather than YAML still
// refuses a row that cannot name a town-root path.
func TestManifestValidateRefusesABadRow(t *testing.T) {
	t.Parallel()
	m := &Manifest{Entries: []Entry{{Path: "logs", Owner: "root", Why: "who is root"}}}
	if err := m.Validate(); err == nil {
		t.Fatal("Validate accepted a row with an unknown owner")
	}
}

// TestParseSecretsManifestRefusesInvalidManifests drives every refusal of the
// secrets parser: a key the format does not define, a second document, and each
// entry rule the manifest header states.
func TestParseSecretsManifestRefusesInvalidManifests(t *testing.T) {
	t.Parallel()
	const listed = "files:\n  - name: a.env\n    consumer: test\n    mode: \"0600\"\n"
	for _, tc := range []struct {
		name string
		data string
		want string
	}{
		{"malformed YAML", "files: [\n", "secrets manifest"},
		{"an unknown key", listed + "  - name: b.env\n    consumer: test\n    mode: \"0600\"\n    owner: someone\n", "owner"},
		{"a second document", listed + "---\nfiles: []\n", "more than one YAML document"},
		{"a malformed second document", listed + "---\nbad: [\n", "secrets manifest"},
		{"no files", "unclaimed:\n  - name: b.env\n    search: grep\n", "lists no files"},
		{"an empty name", "files:\n  - name: \"\"\n    consumer: test\n    mode: \"0600\"\n", "files[0]"},
		{"an absolute name", "files:\n  - name: /etc/passwd\n    consumer: test\n    mode: \"0600\"\n", `"/etc/passwd"`},
		{"a path name", "files:\n  - name: sub/a.env\n    consumer: test\n    mode: \"0600\"\n", `"sub/a.env"`},
		{"a dot name", "files:\n  - name: .\n    consumer: test\n    mode: \"0600\"\n", `"."`},
		{"a control character", "files:\n  - name: \"a\\x01b\"\n    consumer: test\n    mode: \"0600\"\n", "control character"},
		{"a repeated name", listed + "  - name: a.env\n    consumer: two\n    mode: \"0600\"\n", `"a.env"`},
		{"no consumer", "files:\n  - name: a.env\n    mode: \"0600\"\n", `"a.env"`},
		{"an invalid mode", "files:\n  - name: a.env\n    consumer: test\n    mode: \"0644\"\n", `"a.env"`},
		{"an unclaimed entry with a bad name", listed + "unclaimed:\n  - name: /b.env\n    search: grep\n", `"/b.env"`},
		{"an unclaimed entry with no name", listed + "unclaimed:\n  - name: \"\"\n    search: grep\n", "unclaimed[0]"},
		{"a repeated unclaimed name", listed + "unclaimed:\n  - name: b.env\n    search: one\n  - name: b.env\n    search: two\n", `"b.env"`},
		{"a name in both lists", listed + "unclaimed:\n  - name: a.env\n    search: grep\n", `"a.env"`},
		{"an unclaimed entry with no search", listed + "unclaimed:\n  - name: b.env\n", `"b.env"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if _, err := ParseSecretsManifest([]byte(tc.data)); err == nil {
				t.Fatal("ParseSecretsManifest accepted a manifest it must refuse")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to name %q", err, tc.want)
			}
		})
	}
}

// TestParseSecretsManifestReadsTheEntries keeps the accepted form pinned: the
// fields a manifest names arrive on the parsed entry.
func TestParseSecretsManifestReadsTheEntries(t *testing.T) {
	t.Parallel()
	m, err := ParseSecretsManifest([]byte(`
files:
  - name: forgejo-landing.env
    consumer: the landing worker
    mode: "0600"
    required: true
unclaimed:
  - name: hm-config.json
    search: "grep -rn hm-config internal cmd"
`))
	if err != nil {
		t.Fatalf("ParseSecretsManifest: %v", err)
	}
	if len(m.Files) != 1 || m.Files[0] != (SecretFile{Name: "forgejo-landing.env", Consumer: "the landing worker", Mode: "0600", Required: true}) {
		t.Errorf("files = %+v, want the manifest's one entry", m.Files)
	}
	if len(m.Unclaimed) != 1 || m.Unclaimed[0].Name != "hm-config.json" || m.Unclaimed[0].Search == "" {
		t.Errorf("unclaimed = %+v, want the unclaimed file with its search", m.Unclaimed)
	}
}

// TestOpenShellEntryIsGuarded holds the openshell entry to the same guard as a
// listed file: its consumer citation is present, and the guard refuses the entry
// once the citation is removed, naming it (gt-nr548n).
func TestOpenShellEntryIsGuarded(t *testing.T) {
	t.Parallel()
	if err := ValidateSecretsManifest(SecretsManifest{Files: []SecretFile{OpenShellEntry}}); err != nil {
		t.Fatalf("ValidateSecretsManifest(OpenShellEntry) = %v, want the entry accepted", err)
	}
	uncited := OpenShellEntry
	uncited.Consumer = ""
	err := ValidateSecretsManifest(SecretsManifest{Files: []SecretFile{uncited}})
	if err == nil || !strings.Contains(err.Error(), `"openshell"`) {
		t.Errorf("ValidateSecretsManifest on an uncited openshell entry = %v, want a refusal naming it", err)
	}
}
