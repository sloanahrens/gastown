package townstate

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFindCredentials(t *testing.T) {
	t.Parallel()
	// Built from parts so this file holds no token-shaped literal.
	const token = "sk-" + "abcdefghijklmnop1234"
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
		// A ${NAME} reference was a finding before the reference rule; it names
		// where the token lives, as the town config does.
		{"env reference ${NAME} was a finding, now exempt", `{"ANTHROPIC_AUTH_TOKEN":"${GT_X}"}`, nil},
		{"env reference $NAME was a finding, now exempt", `{"api_key":"$GT_X"}`, nil},
		{"truncated JSON: env reference was a finding, now exempt", `{"api_key":"$GT_X"`, nil},
		// A field named keys holds names, not a credential, unless the value is
		// token-shaped; a longer name such as api_keys keeps the old behaviour.
		{"keys with a plain value was a finding, now exempt", `{"keys":"abc-def-123"}`, nil},
		{"keys is exempt in any case", `{"KEYS":"abc-def-123"}`, nil},
		{"truncated JSON: keys with a plain value was a finding, now exempt", `{"keys":"abc-def-123"`, nil},
		{"api_keys is not exempt", `{"api_keys":"abc"}`, []string{"json:api_keys"}},
		{"keys holding a token-shaped value is found", `{"keys":"` + token + `"}`, []string{"json:keys"}},
		{"a bare dollar is not a reference", `{"api_key":"$"}`, []string{"json:api_key"}},
		{"a value with a space is not a reference", `{"password":"$ab cd"}`, []string{"json:password"}},
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

// The name rule is exempt at exactly two JSON paths: a Kubernetes toleration's
// key under any parent, and the daemon's om_token_meta at the document root.
// The json:key and json:om_token_meta rows are the ones a widened shape turns
// red (matching any key, or om_token_meta at any depth); that mutation was run
// by hand and its failing lines are in the commit body, nothing here runs it.
func TestFindCredentialsExemptPaths(t *testing.T) {
	t.Parallel()
	// Built from parts so this file holds no token-shaped literal.
	const token = "sk-" + "abcdefghijklmnop1234"
	exempt := `{"sandboxes":{` +
		`"polecat-k8s":{"tolerations":[{"key":"node.kubernetes.io/unreachable","operator":"Exists"}]},` +
		`"sbx-haiku":{"spec":{"tolerations":[{"key":"dedicated","value":"gpu-train"}]}}},` +
		`"list":[{"tolerations":[{"key":"pool"}]}],` +
		`"daemon":{"om_token_meta":"provider=deepseek; model=flash; window=200000; scope=town"}}`
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"toleration keys under three parents and the root descriptor", exempt, nil},
		{"a token-shaped toleration key is still found", `{"tolerations":[{"key":"` + token + `"}]}`, []string{"value:shape"}},
		{"a token-shaped om_token_meta is still found", `{"daemon":{"om_token_meta":"` + token + `"}}`, []string{"value:shape"}},
		{"a key under any other parent is found", `{"other":{"key":"abc"}}`, []string{"json:key"}},
		{"a key at the document root is found", `{"key":"abc"}`, []string{"json:key"}},
		{"a key deeper under a toleration is found", `{"tolerations":[{"metadata":{"key":"abc"}}]}`, []string{"json:key"}},
		{"another credential-named field of a toleration is found", `{"tolerations":[{"token":"abc"}]}`, []string{"json:token"}},
		{"a key in an array under another name is found", `{"items":[{"key":"abc"}]}`, []string{"json:key"}},
		{"a key in a root array is found", `[{"key":"abc"}]`, []string{"json:key"}},
		{"tolerations as an object is not exempt", `{"tolerations":{"key":"abc"}}`, []string{"json:key"}},
		{"an om_token_meta under another parent is found", `{"x":{"om_token_meta":"abc"}}`, []string{"json:om_token_meta"}},
		{"an om_token_meta below daemon at another depth is found", `{"x":{"daemon":{"om_token_meta":"abc"}}}`, []string{"json:om_token_meta"}},
		{"api_keys is not exempt", `{"api_keys":"abc"}`, []string{"json:api_keys"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindCredentials([]byte(c.in))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("FindCredentials(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	// An object member whose key is spelled like an array position is not one: the
	// path keeps a key and an index apart by type, so none of these is a toleration.
	for _, forged := range []string{"[*]", "[0]", "*", "[ * ]", "[]"} {
		for _, shape := range []string{`{"tolerations":{%q:{"key":"abc"}}}`, `{"x":{"tolerations":{%q:{"key":"abc"}}}}`} {
			in := fmt.Sprintf(shape, forged)
			t.Run("a forged key "+forged+" in "+in, func(t *testing.T) {
				if got := FindCredentials([]byte(in)); !reflect.DeepEqual(got, []string{"json:key"}) {
					t.Fatalf("FindCredentials(%q) = %q, want the name rule to find the key", in, got)
				}
			})
		}
	}
	t.Run("redaction leaves exempt fields unchanged", func(t *testing.T) {
		out, n, err := RedactJSON([]byte(exempt))
		if err != nil || n != 0 || string(out) != exempt {
			t.Fatalf("RedactJSON(exempt document) = %q, %d, %v; want it unchanged with no error", out, n, err)
		}
	})
}

// TestValueShape: a token joined to its name or set in brackets is found, and
// a string with no token-shaped word is not. Each finding is the rule name alone,
// so an exact match also shows that no part of the token reaches a finding.
func TestValueShape(t *testing.T) {
	t.Parallel()
	const token = "sk-abcdefghijklmnop1234"
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"KEY=value", `{"note":"OPENAI_API_KEY=` + token + `"}`, []string{"value:shape"}},
		{"key:value", `{"note":"token:` + token + `"}`, []string{"value:shape"}},
		{"[token]", `{"note":"[` + token + `]"}`, []string{"value:shape"}},
		{"{token}", `{"note":"{` + token + `}"}`, []string{"value:shape"}},
		{"<token>", `{"note":"<` + token + `>"}`, []string{"value:shape"}},
		{"URL with the token in its query", `{"note":"curl https://x/?k=` + token + `"}`, []string{"value:shape"}},
		{"no token-shaped word: URL, path and sentence", `{"note":"see https://example.com/guide?lang=en and ~/notes/[draft]: ok"}`, nil},
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
		{"a reference and a plain keys field are left alone",
			`{"ANTHROPIC_AUTH_TOKEN":"${GT_X}","api_key":"$GT_X","keys":"abc-def-123"}`,
			`{"ANTHROPIC_AUTH_TOKEN":"${GT_X}","api_key":"$GT_X","keys":"abc-def-123"}`, 0},
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

// TestFindCredentialsDeepNestingIsLinear: a document nested to the decoder's
// limit with a few hundred thousand empty arrays at that depth (about 1 MB) is
// scanned in time proportional to its size: the walk keeps a parent link and a
// depth for each container and compares the last three steps, instead of copying
// the whole path for each container it opens (which took seconds per megabyte).
func TestFindCredentialsDeepNestingIsLinear(t *testing.T) {
	t.Parallel()
	const depth = 9990
	doc := strings.Repeat("[", depth) + strings.TrimSuffix(strings.Repeat("[],", 330000), ",") + strings.Repeat("]", depth)
	// The 5 s bound is a hang guard, not a measure of linearity: the quadratic walk
	// takes about 10 s on this shape, so a return inside the bound is the evidence.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan []string, 1)
	go func() { done <- FindCredentials([]byte(doc)) }()
	select {
	case got := <-done:
		if len(got) != 0 {
			t.Errorf("FindCredentials found %q in a document with no strings", got)
		}
	case <-ctx.Done():
		t.Fatalf("FindCredentials did not return within the bound on a %d-byte document nested %d deep", len(doc), depth)
	}
}
