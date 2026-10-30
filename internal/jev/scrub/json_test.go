package scrub

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestScrubJSON checks that every string value is scrubbed at any depth, that
// keys, numbers and structure survive, and that the output is valid JSON.
func TestScrubJSON(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
  "model": "jev",
  "contact": "mail alice@example.com",
  "api_key": "hunter2",
  "counts": {"attempts": 3, "ok": true},
  "items": ["token=hunter2", {"deep": "Bearer abcdefgh12345678"}, 7, null],
  "markup": "a < b & c"
}`)

	out, report := Default().ScrubJSON(raw)
	if !json.Valid(out) {
		t.Fatalf("scrubbed document is not valid JSON: %s", out)
	}

	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode scrubbed document: %v", err)
	}
	for _, key := range []string{"model", "contact", "api_key", "counts", "items", "markup"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("key %q was lost or renamed", key)
		}
	}
	if got, want := doc["model"], any("jev"); got != want {
		t.Errorf("model = %v, want %v", got, want)
	}
	if got, want := doc["contact"], any("mail "+marker(RuleEmail)); got != want {
		t.Errorf("contact = %v, want %v", got, want)
	}
	if got, want := doc["api_key"], any(marker(RuleAssignment)); got != want {
		t.Errorf("api_key value = %v, want %v", got, want)
	}
	if got, want := doc["markup"], any("a < b & c"); got != want {
		t.Errorf("markup = %v, want %v", got, want)
	}

	counts := doc["counts"].(map[string]any)
	if got, want := counts["attempts"], any(float64(3)); got != want {
		t.Errorf("attempts = %v, want %v", got, want)
	}
	if got, want := counts["ok"], any(true); got != want {
		t.Errorf("ok = %v, want %v", got, want)
	}

	items := doc["items"].([]any)
	if got, want := items[0], any("token="+marker(RuleAssignment)); got != want {
		t.Errorf("items[0] = %v, want %v", got, want)
	}
	if got, want := items[1].(map[string]any)["deep"], any("Bearer "+marker(RuleAuthHeader)); got != want {
		t.Errorf("items[1].deep = %v, want %v", got, want)
	}
	if got, want := items[2], any(float64(7)); got != want {
		t.Errorf("items[2] = %v, want %v", got, want)
	}
	if items[3] != nil {
		t.Errorf("items[3] = %v, want null", items[3])
	}

	want := map[string]int{RuleAssignment: 2, RuleAuthHeader: 1, RuleEmail: 1}
	for _, rule := range DefaultRules() {
		if got := report.Count(rule.Name); got != want[rule.Name] {
			t.Errorf("report counts %q %d times, want %d", rule.Name, got, want[rule.Name])
		}
	}
	if got, wantTotal := report.Total(), 4; got != wantTotal {
		t.Errorf("report total %d, want %d", got, wantTotal)
	}
}

// TestScrubJSONCredentialValues checks that a credential-named key redacts its
// whole value whatever its JSON type, so a secret nested under the key cannot
// reach the output as structure, and that a look-alike field beside it is kept.
func TestScrubJSONCredentialValues(t *testing.T) {
	t.Parallel()
	raw := []byte(`{
  "token": {"refresh": "hunter2", "scopes": ["read"]},
  "secret": ["hunter2", "hunter3"],
  "password": 1234,
  "api_key": true,
  "nested": {"tokens_used": 12, "author": "alice", "items": [1, 2], "input_tokens": 900, "maxTokens": 4096, "key": "cache-key-1"}
}`)

	out, report := Default().ScrubJSON(raw)
	if !json.Valid(out) {
		t.Fatalf("scrubbed document is not valid JSON: %s", out)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("decode scrubbed document: %v", err)
	}
	for _, key := range []string{"token", "secret", "password", "api_key"} {
		if got, want := doc[key], any(marker(RuleAssignment)); got != want {
			t.Errorf("%s = %v, want the whole value %v", key, got, want)
		}
	}
	if strings.Contains(string(out), "hunter") {
		t.Errorf("a secret survived under a credential key: %s", out)
	}

	nested := doc["nested"].(map[string]any)
	if got, want := nested["tokens_used"], any(float64(12)); got != want {
		t.Errorf("nested.tokens_used = %v, want %v", got, want)
	}
	if got, want := nested["author"], any("alice"); got != want {
		t.Errorf("nested.author = %v, want %v", got, want)
	}
	for key, want := range map[string]any{
		"input_tokens": float64(900),
		"maxTokens":    float64(4096),
	} {
		if got := nested[key]; got != want {
			t.Errorf("nested.%s = %v, want the count %v", key, got, want)
		}
	}
	items := nested["items"].([]any)
	if len(items) != 2 || items[0] != any(float64(1)) || items[1] != any(float64(2)) {
		t.Errorf("nested.items = %v, want [1 2]", items)
	}

	if got, want := nested["key"], any(marker(RuleAssignment)); got != want {
		t.Errorf("nested.key = %v, want a bare key redacted as %v", got, want)
	}

	if got, want := report.Count(RuleAssignment), 5; got != want {
		t.Errorf("report counts assignment %d times, want %d", got, want)
	}
	if got, want := report.Total(), 5; got != want {
		t.Errorf("report total %d, want %d", got, want)
	}
}

// TestScrubJSONFallback checks that input which is not one JSON document is
// scrubbed as text rather than passed through unscrubbed.
func TestScrubJSONFallback(t *testing.T) {
	t.Parallel()
	s := Default()
	cases := map[string]string{
		"truncated": `{"a": "token=hunter2",`,
		"trailing":  `{"a": 1} {"b": "token=hunter2"}`,
		"not JSON":  "token=hunter2",
		"empty":     "",
	}
	for name, raw := range cases {
		raw := raw
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			out, _ := s.ScrubJSON([]byte(raw))
			if strings.Contains(string(out), "hunter2") {
				t.Errorf("ScrubJSON(%q) leaked the secret: %q", raw, out)
			}
			if raw != "" && !strings.Contains(string(out), marker(RuleAssignment)) {
				t.Errorf("ScrubJSON(%q) = %q, want the assignment marker", raw, out)
			}
		})
	}
}
