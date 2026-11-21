package townstate

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestFindCredentials(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"env assignment", "DEEPSEEK_API_KEY=abc\n", []string{"env:DEEPSEEK_API_KEY"}},
		{"env export and quotes", "export GH_TOKEN='x'\nPATH=/bin\n", []string{"env:GH_TOKEN"}},
		{"env empty and quoted empty", "API_KEY=\nAPI_KEY=''\nAPI_KEY=\"\"\n", nil},
		{"env exempt suffixes", "KEY_FILE=/x\nTOKEN_PATH=/y\nSECRET_ENV=Z\nKEY_NAME=k\n", nil},
		{"json member", `{"api_key":"abc"}`, []string{"json:api_key"}},
		{"json exempt and non-string", `{"api_key_file":"/x","token_budget":5,"secret":null,"password":true}`, nil},
		{"json empty value", `{"api_key":""}`, nil},
		{"name is matched in any case", `{"ApiKey":"v","X_PASSWORD":"p"}`, []string{"json:ApiKey", "json:X_PASSWORD"}},
		{"duplicate key, secret first", `{"api_key":"real","api_key":"x"}`, []string{"json:api_key", "json:api_key"}},
		{"duplicate key, secret last", `{"api_key":"","api_key":"real"}`, []string{"json:api_key"}},
		{"value shape never exempts", `{"api_key":"12345","token":"on"}`, []string{"json:api_key", "json:token"}},
		{"nested object; strings in an array are not members", `{"a":[{"password":"p"}],"tokens":["s1","s2"]}`, []string{"json:password"}},
		{"tokens exemption needs no other credential word", `{"api_key_tokens":"123","secret_tokens":"on","password_tokens":"1"}`, []string{"json:api_key_tokens", "json:secret_tokens", "json:password_tokens"}},
		{"already redacted", `{"api_key":"` + redactedMarker + `"}`, nil},
		{"placeholder is not a secret", `{"ANTHROPIC_AUTH_TOKEN":"REDACTED"}`, nil},
		{"plural tokens count or flag is exempt", `{"CLAUDE_CODE_MAX_OUTPUT_TOKENS":"8192","CLAUDE_CODE_TOTAL_TOKENS_REMINDER":"off"}`, nil},
		{"plural tokens holding a real value reports", `{"auth_tokens":"abcDEF123"}`, []string{"json:auth_tokens"}},
		{"singular names are never exempt by shape", `{"token":"12345","password":"off","api_key":"true"}`, []string{"json:token", "json:password", "json:api_key"}},
		{"content after a valid document is still scanned", `{"api_key":"abc"} trailing`, []string{"json:api_key"}},
		{"truncated JSON is still scanned", `{"api_key":"abc"`, []string{"json:api_key"}},
		{"not JSON, no credentials", "hello\nworld\n", nil},
		{"free text holds a token-shaped word", `{"note":"use sk-abcdefghijklmnopqrst for the build"}`, []string{"value:shape"}},
		{"token-shaped words in a string and an array, one finding each", `{"a":"ghp_abcdefghijklmnopqr and AKIAABCDEFGHIJKLMNOP","list":["sk-abcdefghijklmnopqrst"]}`, []string{"value:shape", "value:shape", "value:shape"}},
		{"top-level string holding a token-shaped word", `"sk-abcdefghijklmnopqrst"`, []string{"value:shape"}},
		{"a sentence with no token-shaped word is not found", `{"note":"the build ran for twenty minutes without any error"}`, nil},
		{"a URL is not found", `{"docs":"https://example.com/guide/getting-started"}`, nil},
		{"a short prefix is not a token", `{"note":"sk-abc"}`, nil},
		{"a credential key with a token value is one finding", `{"api_key":"sk-abcdefghijklmnopqrst"}`, []string{"json:api_key"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindCredentials([]byte(c.in))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("FindCredentials(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestRedactJSON(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		want  string
		wantN int
	}{
		{"keeps every other byte", "{ \"a\": 1,  \"api_key\": \"abc\" , \"b\":[\"x\"], \"token\":\"t\"}",
			"{ \"a\": 1,  \"api_key\": \"<redacted>\" , \"b\":[\"x\"], \"token\":\"<redacted>\"}", 2},
		{"duplicate keys", `{"api_key":"one","api_key":"two"}`, `{"api_key":"<redacted>","api_key":"<redacted>"}`, 2},
		{"escaped quote and backslash in the value", `{"password":"p\"q\\","k":"v"}`, `{"password":"<redacted>","k":"v"}`, 1},
		{"strings in an array are not members", `{"tokens":["a","b"]}`, `{"tokens":["a","b"]}`, 0},
		{"nothing to redact", `{"api_key_file":"/x","n":1}`, `{"api_key_file":"/x","n":1}`, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, n, err := RedactJSON([]byte(c.in))
			if err != nil {
				t.Fatalf("RedactJSON(%q): %v", c.in, err)
			}
			if string(out) != c.want || n != c.wantN {
				t.Fatalf("RedactJSON(%q) = %q, %d; want %q, %d", c.in, out, n, c.want, c.wantN)
			}
			if !json.Valid(out) || len(FindCredentials(out)) != 0 {
				t.Fatalf("output %q is not valid JSON or still holds a credential", out)
			}
		})
	}
}

func TestRedactJSONRefusesWhatIsNotJSON(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"API_KEY=abc\n", `{"api_key":"abc"`, ""} {
		out, n, err := RedactJSON([]byte(in))
		if err == nil || out != nil || n != 0 {
			t.Fatalf("RedactJSON(%q) = %q, %d, %v; want an error and no output", in, out, n, err)
		}
	}
}

// A free-text string holding a token-shaped word is not rewritten (its key is not a
// credential name), so the redactor refuses the whole document and names no part of it.
func TestRedactJSONRefusesFreeTextCredential(t *testing.T) {
	t.Parallel()
	const token = "sk-abcdefghijklmnopqrst"
	in := `{"note":"use ` + token + ` for the build","model":"m"}`
	out, n, err := RedactJSON([]byte(in))
	if err == nil || out != nil || n != 0 {
		t.Fatalf("RedactJSON = %q, %d, %v; want an error and no output", out, n, err)
	}
	if strings.Contains(err.Error(), token) {
		t.Errorf("error %q holds the token", err)
	}
}

func TestFindCredentialsLivetownSettings(t *testing.T) {
	t.Parallel()
	files, err := filepath.Glob(filepath.Join("..", "config", "testdata", "livetown", "settings", "*"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixture settings found: %v (%d files)", err, len(files))
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if got := FindCredentials(data); len(got) != 0 {
			t.Errorf("%s: a normal config reads as holding credentials: %q", f, got)
		}
		if _, n, err := RedactJSON(data); json.Valid(data) && (err != nil || n != 0) {
			t.Errorf("%s: RedactJSON = %d, %v; want 0 and no error", f, n, err)
		}
	}
}
