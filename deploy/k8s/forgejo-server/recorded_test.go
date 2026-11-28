package forgejoserver

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type object struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name string `json:"name"`
	} `json:"metadata"`
	Spec struct {
		Template struct {
			Spec struct {
				Containers []struct {
					Env []struct {
						Name  string  `json:"name"`
						Value *string `json:"value"`
					} `json:"env"`
				} `json:"containers"`
			} `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// checkRecorded refuses raw unless it is one JSON object of kind and name with
// no PASSWD or __PASSWD env entry set to a literal value.
func checkRecorded(raw []byte, kind, name string) error {
	var obj object
	if err := json.Unmarshal(raw, &obj); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if obj.Kind != kind || obj.Metadata.Name != name {
		return fmt.Errorf("decodes as %s/%s, want %s/%s", obj.Kind, obj.Metadata.Name, kind, name)
	}
	for _, c := range obj.Spec.Template.Spec.Containers {
		for _, e := range c.Env {
			if (e.Name == "PASSWD" || strings.HasSuffix(e.Name, "__PASSWD")) && e.Value != nil {
				return fmt.Errorf("env %s carries a literal value", e.Name)
			}
		}
	}
	return nil
}

func TestRecordedRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ file, kind, name string }{
		{"deployment.json", "Deployment", "forgejo"},
		{"service.json", "Service", "forgejo"},
		{"claim.json", "PersistentVolumeClaim", "data"},
		{"namespace.json", "Namespace", "forgejo"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "live-2026-11-27", tc.file))
			if err == nil {
				err = checkRecorded(raw, tc.kind, tc.name)
			}
			if err != nil {
				t.Fatal(tc.file, err)
			}
		})
	}
}

func TestCheckRecordedRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, raw string }{
		{"not JSON", `{"kind":`},
		{"two objects", `{"kind":"Deployment","metadata":{"name":"forgejo"}} {}`},
		{"wrong kind", `{"kind":"Service","metadata":{"name":"forgejo"}}`},
		{"wrong name", `{"kind":"Deployment","metadata":{"name":"other"}}`},
		{"literal PASSWD", `{"kind":"Deployment","metadata":{"name":"forgejo"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"PASSWD","value":"x"}]}]}}}}`},
		{"literal __PASSWD suffix", `{"kind":"Deployment","metadata":{"name":"forgejo"},"spec":{"template":{"spec":{"containers":[{"env":[{"name":"FORGEJO__database__PASSWD","value":"x"}]}]}}}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := checkRecorded([]byte(tc.raw), "Deployment", "forgejo"); err == nil {
				t.Fatal("checkRecorded accepted the input, want an error")
			}
		})
	}
}
