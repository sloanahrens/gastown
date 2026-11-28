package forgejoserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"slices"
	"strings"
	"testing"
)

// mustObject decodes a JSON object literal.
func mustObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(raw), &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

// shortDigest is the form a line carries for the canonical JSON lit.
func shortDigest(lit string) string {
	sum := sha256.Sum256([]byte(lit))
	return "sha256:" + hex.EncodeToString(sum[:6])
}

// deployment returns a Deployment whose spec and container are JSON fragments
// spliced in, so a row varies only the fields it names.
func deployment(spec, container string) string {
	return `{"kind":"Deployment","metadata":{"name":"forgejo"},"spec":{` + spec +
		`,"template":{"spec":{"containers":[{"name":"forgejo",` + container + `}]}}}}`
}

const (
	baseEnv       = `"env":[{"name":"A","value":"a"}]`
	baseContainer = `"image":"forgejo:1",` + baseEnv
)

func TestDifferences(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		rendered string
		observed string
		want     []string
	}{
		{
			name:     "identical objects give no line",
			rendered: deployment(`"replicas":1`, baseContainer),
			observed: deployment(`"replicas":1`, baseContainer),
		},
		{
			name:     "server-set fields give no line",
			rendered: deployment(`"replicas":1`, baseContainer),
			observed: `{"kind":"Deployment","metadata":{"name":"forgejo","uid":"u-1","resourceVersion":"42","generation":3,` +
				`"creationTimestamp":"2026-11-01T00:00:00Z","managedFields":[{"manager":"kubectl"}],` +
				`"annotations":{"deployment.kubernetes.io/revision":"7"}},` +
				`"spec":{"replicas":1,"template":{"spec":{"containers":[{"name":"forgejo",` + baseContainer + `}]}}},` +
				`"status":{"replicas":1}}`,
		},
		{
			name:     "changed value gives one line with its path",
			rendered: deployment(`"replicas":1`, baseContainer),
			observed: deployment(`"replicas":2`, baseContainer),
			want: []string{"spec.replicas: rendered sha256:6b86b273ff34" +
				", observed sha256:d4735e3a265e"},
		},
		{
			name:     "manifest-only field gives one line with its path",
			rendered: deployment(`"replicas":1,"strategy":{"type":"Recreate"}`, baseContainer),
			observed: deployment(`"replicas":1`, baseContainer),
			want: []string{"spec.strategy: rendered " + shortDigest(`{"type":"Recreate"}`) +
				", observed absent"},
		},
		{
			name:     "object on one side and string on the other gives one line at that key",
			rendered: deployment(`"replicas":1,"strategy":{"type":"Recreate"}`, baseContainer),
			observed: deployment(`"replicas":1,"strategy":"Recreate"`, baseContainer),
			want: []string{"spec.strategy: rendered " + shortDigest(`{"type":"Recreate"}`) +
				", observed " + shortDigest(`"Recreate"`)},
		},
		{
			name:     "changed image gives one line with its path",
			rendered: deployment(`"replicas":1`, baseContainer),
			observed: deployment(`"replicas":1`, `"image":"forgejo:2",`+baseEnv),
			want: []string{"spec.template.spec.containers[0].image: rendered " + shortDigest(`"forgejo:1"`) +
				", observed " + shortDigest(`"forgejo:2"`)},
		},
		{
			name:     "live-only env entry gives one line at its position",
			rendered: deployment(`"replicas":1`, baseContainer),
			observed: deployment(`"replicas":1`, `"image":"forgejo:1","env":[{"name":"A","value":"a"},{"name":"B","value":"b"}]`),
			want: []string{"spec.template.spec.containers[0].env[1]: rendered absent, observed " +
				shortDigest(`{"name":"B","value":"b"}`)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Differences(mustObject(t, tt.rendered), mustObject(t, tt.observed))
			if err != nil {
				t.Fatalf("Differences() error: %v", err)
			}
			if !slices.Equal(got, tt.want) {
				t.Errorf("Differences() lines:\n got %q\nwant %q", got, tt.want)
			}
		})
	}
}

func TestDifferencesRefusesEmptySide(t *testing.T) {
	t.Parallel()
	base := mustObject(t, deployment(`"replicas":1`, baseContainer))
	// Nested past the 10000 levels encoding/json accepts, so json.Marshal
	// refuses the side.
	var nested any = "leaf"
	for i := 0; i < 10001; i++ {
		nested = map[string]any{"k": nested}
	}
	tests := []struct {
		name     string
		rendered map[string]any
		observed map[string]any
		wantErr  string
	}{
		{name: "empty manifest", rendered: map[string]any{}, observed: base, wantErr: "no fields to compare"},
		{name: "empty live", rendered: base, observed: map[string]any{}, wantErr: "no fields to compare"},
		{name: "live holds only server-set fields", rendered: base, wantErr: "no fields to compare",
			observed: mustObject(t, `{"metadata":{"uid":"u-1"},"status":{"replicas":1}}`)},
		{name: "manifest holds only server-set fields", observed: base, wantErr: "no fields to compare",
			rendered: mustObject(t, `{"metadata":{"uid":"u-1"}}`)},
		{name: "value that does not encode as JSON", rendered: map[string]any{"spec": math.NaN()},
			observed: base, wantErr: "does not encode as JSON"},
		{name: "value nested past the encoder's depth limit", rendered: map[string]any{"spec": nested},
			observed: base, wantErr: "does not encode as JSON"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Differences(tt.rendered, tt.observed)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("Differences() = %q, %v; want an error containing %q, not no difference", got, err, tt.wantErr)
			}
		})
	}
}

// TestDifferencesNoValueInLine holds that a differing value is a digest in the
// line, never the value itself, even at its position inside a list.
func TestDifferencesNoValueInLine(t *testing.T) {
	t.Parallel()
	const secret = "hunter2-secret"
	rendered := mustObject(t, deployment(`"replicas":1`, baseContainer))
	observed := mustObject(t, deployment(`"replicas":1`,
		`"image":"forgejo:1","env":[{"name":"A","value":"`+secret+`"}]`))
	got, err := Differences(rendered, observed)
	if err != nil {
		t.Fatalf("Differences() error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Differences() lines = %q, want exactly one", got)
	}
	if strings.Contains(got[0], secret) {
		t.Errorf("line carries a value: %q", got[0])
	}
	if want := "spec.template.spec.containers[0].env[0].value: rendered " + shortDigest(`"a"`) +
		", observed " + shortDigest(`"`+secret+`"`); got[0] != want {
		t.Errorf("line = %q, want %q", got[0], want)
	}
}

func TestDecodeNumbersRefusesBadInput(t *testing.T) {
	t.Parallel()
	if _, err := decodeNumbers([]byte(`{"unfinished":`)); err == nil {
		t.Fatal("decodeNumbers() accepted the bytes of an unfinished object; want an error")
	}
}
