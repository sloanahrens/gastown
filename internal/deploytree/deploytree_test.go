package deploytree

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

const digestA = "registry.local/gt/daemon@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "registry.local/gt/trackerd@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// valuesFile is a valid values file for the workspace id.
func valuesFile(ws string) []byte {
	return []byte(fmt.Sprintf(`workspace: %s
namespace: gt-%s
trackerDSNSecret: %s-tracker-dsn
images:
  daemon: %s
  trackerd: %s
`, ws, ws, ws, digestA, digestB))
}

func mustRender(t *testing.T, ws string) []Resource {
	t.Helper()
	v, err := Load(valuesFile(ws))
	if err != nil {
		t.Fatalf("Load(%s): %v", ws, err)
	}
	res, err := Render(v)
	if err != nil {
		t.Fatalf("Render(%s): %v", ws, err)
	}
	return res
}

func identities(res []Resource) map[string]bool {
	m := map[string]bool{}
	for _, r := range res {
		m[r.Kind+"/"+r.Namespace+"/"+r.Name] = true
	}
	return m
}

func TestTwoWorkspacesShareNoResourceName(t *testing.T) {
	t.Parallel()
	a, b := identities(mustRender(t, "alpha")), identities(mustRender(t, "beta"))
	if len(a) < 6 {
		t.Fatalf("alpha rendered %d resources, want a whole town", len(a))
	}
	for id := range a {
		if b[id] {
			t.Errorf("both workspaces render %s", id)
		}
	}
	for _, r := range mustRender(t, "alpha") {
		if !strings.Contains(r.Name, "alpha") {
			t.Errorf("%s %s does not carry the workspace id", r.Kind, r.Name)
		}
		if r.Kind != "Namespace" && !strings.Contains(string(r.YAML), "gt.dev/workspace: alpha") {
			t.Errorf("%s %s has no workspace label", r.Kind, r.Name)
		}
	}
}

func TestNoPostgresServiceRendered(t *testing.T) {
	t.Parallel()
	for _, r := range mustRender(t, "alpha") {
		low := strings.ToLower(string(r.YAML))
		if strings.Contains(low, "postgres") || r.Kind == "StatefulSet" {
			t.Errorf("%s %s is or mentions Postgres:\n%s", r.Kind, r.Name, r.YAML)
		}
	}
}

func TestDSNIsASecretReferenceOnly(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{
		"postgres://user:pw@db.example:5432/gt",
		"user:pw@db",
		"Not A Name",
		"",
	} {
		file := strings.Replace(string(valuesFile("alpha")), "alpha-tracker-dsn", bad, 1)
		if bad == "" {
			file = strings.Replace(string(valuesFile("alpha")), "trackerDSNSecret: alpha-tracker-dsn\n", "", 1)
		}
		_, err := Load([]byte(file))
		if !errors.Is(err, ErrBadValues) {
			t.Errorf("Load with trackerDSNSecret %q returned %v, want ErrBadValues", bad, err)
		}
	}
	// The rendered town refers to the Secret by name and holds no DSN.
	for _, r := range mustRender(t, "alpha") {
		if strings.Contains(string(r.YAML), "://") {
			t.Errorf("%s %s carries a URL:\n%s", r.Kind, r.Name, r.YAML)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	t.Parallel()
	first, second := YAML(mustRender(t, "alpha")), YAML(mustRender(t, "alpha"))
	if !bytes.Equal(first, second) {
		t.Fatal("two renders of one values file differ")
	}
}

func TestValuesAreValidated(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"namespace of another workspace": strings.Replace(string(valuesFile("alpha")), "namespace: gt-alpha", "namespace: gt-beta", 1),
		"image by tag":                   strings.Replace(string(valuesFile("alpha")), digestA, "registry.local/gt/daemon:latest", 1),
		"unknown key":                    string(valuesFile("alpha")) + "extra: 1\n",
		"unknown component":              string(valuesFile("alpha")) + "  postgres: registry.local/pg@sha256:" + strings.Repeat("c", 64) + "\n",
		"bad workspace id":               strings.ReplaceAll(string(valuesFile("alpha")), "alpha", "Al-pha"),
	}
	for name, file := range cases {
		if _, err := Load([]byte(file)); !errors.Is(err, ErrBadValues) {
			t.Errorf("%s: Load returned %v, want ErrBadValues", name, err)
		}
	}
}

func TestMarshalFailureFailsTheRender(t *testing.T) {
	t.Parallel()
	v, err := Load(valuesFile("alpha"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	boom := errors.New("marshal refused")
	res, err := render(v, func(any) ([]byte, error) { return nil, boom })
	if !errors.Is(err, boom) {
		t.Fatalf("render returned %v, want the marshal error", err)
	}
	if res != nil {
		t.Errorf("failed render returned %d resources, want none", len(res))
	}
}

func TestRenderRefusesInvalidValues(t *testing.T) {
	t.Parallel()
	if _, err := Render(Values{Workspace: "alpha"}); !errors.Is(err, ErrBadValues) {
		t.Fatalf("Render of incomplete values returned %v, want ErrBadValues", err)
	}
}

func TestUnknownImageErrorNamesTheFirstKeyEveryTime(t *testing.T) {
	t.Parallel()
	file := string(valuesFile("alpha")) + "  zeta: registry.local/z@sha256:" + strings.Repeat("c", 64) +
		"\n  beta: registry.local/b@sha256:" + strings.Repeat("c", 64) + "\n"
	for i := 0; i < 20; i++ {
		_, err := Load([]byte(file))
		if err == nil || !strings.Contains(err.Error(), "images.beta") {
			t.Fatalf("Load = %v, want the error to name images.beta every time", err)
		}
	}
}

func TestTwoWorkspacesCannotNameOneDSNSecret(t *testing.T) {
	t.Parallel()
	for _, ws := range []string{"alpha", "beta"} {
		file := strings.ReplaceAll(string(valuesFile(ws)), ws+"-tracker-dsn", "shared-tracker-dsn")
		if _, err := Load([]byte(file)); !errors.Is(err, ErrBadValues) {
			t.Errorf("workspace %s naming shared-tracker-dsn: Load = %v, want ErrBadValues", ws, err)
		}
	}
}
