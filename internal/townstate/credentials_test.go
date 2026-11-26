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
		{"json exempt suffix, a boolean and a null stay", `{"api_key_file":"/x","secret":null,"password":true}`, nil},
		{"a number under a credential key is found", `{"token_budget":5}`, []string{"json:token_budget"}},
		{"json empty value", `{"api_key":""}`, nil},
		{"name is matched in any case", `{"ApiKey":"v","X_PASSWORD":"p"}`, []string{"json:ApiKey", "json:X_PASSWORD"}},
		{"duplicate key, secret first", `{"api_key":"real","api_key":"x"}`, []string{"json:api_key", "json:api_key"}},
		{"duplicate key, secret last", `{"api_key":"","api_key":"real"}`, []string{"json:api_key"}},
		{"value shape never exempts", `{"api_key":"12345","token":"on"}`, []string{"json:api_key", "json:token"}},
		{"nested object; a tokens array is not exempt by shape", `{"a":[{"password":"p"}],"tokens":["s1","s2"]}`, []string{"json:password", "json:tokens", "json:tokens"}},
		{"an array of strings under a credential key", `{"api_keys":["one","two"]}`, []string{"json:api_keys", "json:api_keys"}},
		{"a number under token", `{"token":123456789}`, []string{"json:token"}},
		{"a number inside an array under api_keys", `{"api_keys":[111,222]}`, []string{"json:api_keys", "json:api_keys"}},
		{"a negative and an exponent number under a credential key", `{"token":-1,"api_key":1.5e-3}`, []string{"json:token", "json:api_key"}},
		{"a number in a nested array under a credential key", `{"api_keys":[[7],8]}`, []string{"json:api_keys", "json:api_keys"}},
		{"an escaped quote inside an array element", `{"api_keys":["a\"b"]}`, []string{"json:api_keys"}},
		{"a duplicate key reports both arrays", `{"api_keys":["one"],"api_keys":["two"]}`, []string{"json:api_keys", "json:api_keys"}},
		{"a nested array under a credential key", `{"api_keys":[["one"],"two"]}`, []string{"json:api_keys", "json:api_keys"}},
		{"a nested object under a credential key", `{"password":{"value":"hunter2"}}`, []string{"json:password"}},
		{"a nested non-credential key under a credential key", `{"api_key":{"outer":{"inner":"v"}}}`, []string{"json:api_key"}},
		{"an array of objects under a credential key", `{"password":[{"secret":"x"},{"name":"y"}]}`, []string{"json:password", "json:password"}},
		{"array elements that are references or empty are not credentials", `{"api_keys":["$GT_X",""]}`, nil},
		{"a keys array of plain names is exempt", `{"keys":["a","b"]}`, nil},
		{"a keys array holding a token-shaped name is found", `{"keys":["` + token + `"]}`, []string{"json:keys"}},
		{"strings in an array under another name are not members", `{"list":["a","b"]}`, nil},
		{"a document with no credential key", `{"models":["a","b"],"n":[1,2]}`, nil},
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

// The detector treats passwd, pwd, authorization, credentials and dsn as a
// credential only when one whole word of the name equals it, the name split on
// underscores, hyphens, dots and camel-case humps, while key, token, secret and
// password keep matching anywhere (gt-06nqnk).
func TestCredentialNameWholeWords(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"the document the bead names", `{"passwd":"x","pwd":"x","authorization":"Bearer z","credentials":"x","dsn":"postgres://u:p@h/db"}`,
			[]string{"json:passwd", "json:pwd", "json:authorization", "json:credentials", "json:dsn"}},
		{"passwd", `{"passwd":"x"}`, []string{"json:passwd"}},
		{"pwd", `{"pwd":"x"}`, []string{"json:pwd"}},
		{"authorization", `{"authorization":"Bearer z"}`, []string{"json:authorization"}},
		{"credentials", `{"credentials":"x"}`, []string{"json:credentials"}},
		{"dsn", `{"dsn":"postgres://u@host/db"}`, []string{"json:dsn"}},
		{"an underscore separates words", `{"DB_PASSWD":"x"}`, []string{"json:DB_PASSWD"}},
		{"a camel-case hump separates words", `{"apiKey":"x"}`, []string{"json:apiKey"}},
		{"a hyphen separates words", `{"X-Authorization":"Bearer z"}`, []string{"json:X-Authorization"}},
		{"a dot separates words", `{"db.passwd":"x"}`, []string{"json:db.passwd"}},
		{"an acronym run splits before its last upper", `{"DBPasswd":"x"}`, []string{"json:DBPasswd"}},
		{"an acronym prefix splits from the word", `{"XAuthorization":"Bearer z"}`, []string{"json:XAuthorization"}},
		{"an acronym prefix that is not a credential word", `{"HTTPSProxy":"http://proxy.example:3128"}`, nil},
		{"OAuthScope is not authorization", `{"OAuthScope":"read"}`, nil},
		{"author is not authorization", `{"author":"x"}`, nil},
		{"authors is not authorization", `{"authors":"x"}`, nil},
		{"oauth_scope is not authorization", `{"oauth_scope":"x"}`, nil},
		{"authority is not authorization", `{"authority":"x"}`, nil},
		{"pwdx is not pwd", `{"pwdx":"x"}`, nil},
		{"dsn_file is exempt by suffix", `{"dsn_file":"/run/dsn"}`, nil},
		{"a token count is still exempt by shape", `{"max_tokens":"8192"}`, nil},
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

// A string value that is a URL with a non-empty password in its userinfo is a
// finding under any key, and the rule carries the key and never the value
// (gt-06nqnk). A URL with a user and no password, or no userinfo, is not. A URL
// url.Parse rejects or leaves without a userinfo still fails closed on its shape.
func TestFindCredentialsUserinfoURL(t *testing.T) {
	t.Parallel()
	const url = "https://user:pw@host/x.git"
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"a repository URL under one key", `{"repo":"` + url + `"}`, []string{"json:repo"}},
		{"under another key", `{"origin":"` + url + `"}`, []string{"json:origin"}},
		{"under a credential key the name rule reports once", `{"token":"` + url + `"}`, []string{"json:token"}},
		{"a percent-encoded password", `{"repo":"https://user:p%40ss@host/x.git"}`, []string{"json:repo"}},
		{"an invalid percent escape in the password fails closed", `{"repo":"https://user:p%zz@host/x.git"}`, []string{"json:repo"}},
		{"an unescaped slash in the password fails closed", `{"repo":"https://user:pa/ss@host/x.git"}`, []string{"json:repo"}},
		{"a literal space in the password fails closed", `{"repo":"https://user:pa ss@host/x.git"}`, []string{"json:repo"}},
		{"a URL with a port and an @ in its path has no userinfo", `{"repo":"https://registry.npmjs.org:443/@scope/pkg"}`, nil},
		{"an env value with an invalid percent escape", "GIT_URL=https://user:p%zz@host/x.git\n", []string{"env:GIT_URL"}},
		{"an element of an array names the array's key", `{"repos":["` + url + `"]}`, []string{"json:repos"}},
		{"an element of a nested array names the outer key", `{"repos":[["` + url + `"]]}`, []string{"json:repos"}},
		{"a top-level string has no key", `"` + url + `"`, []string{"json:"}},
		{"an exempt field's value is still a finding", `{"tolerations":[{"key":"` + url + `"}]}`, []string{"json:key"}},
		{"an env value", "GIT_URL=" + url + "\n", []string{"env:GIT_URL"}},
		{"a quoted env value", "GIT_URL=\"" + url + "\"\n", []string{"env:GIT_URL"}},
		{"truncated JSON is still scanned", `{"repo":"` + url + `"`, []string{"json:repo"}},
		{"no password", `{"repo":"https://user@host/x.git"}`, nil},
		{"no userinfo", `{"repo":"https://host/x.git"}`, nil},
		{"a userinfo-shaped value with no scheme", `{"repo":"user:pw@host/x.git"}`, nil},
		{"an empty password part", `{"repo":"https://user:@host/x.git"}`, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindCredentials([]byte(c.in))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("FindCredentials(%q) = %q, want %q", c.in, got, c.want)
			}
			// A rule names the key, never any part of the value.
			for _, rule := range got {
				if strings.Contains(rule, "pw@") || strings.Contains(rule, "host") {
					t.Fatalf("rule %q holds part of the URL's value", rule)
				}
			}
		})
	}
}

// A YAML mapping entry in text that is not valid JSON is a finding when the
// last dotted segment of its key is a credential name and its value is a
// secret (gt-b4uopc). The rule names the key, never the value.
func TestFindCredentialsReadsYAMLLines(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"a plain entry", "password: hunter2\n", []string{"yaml:password"}},
		{"an unquoted api key", "api_key: sk-live-123\n", []string{"yaml:api_key"}},
		{"a double-quoted token", "token: \"abc123\"\n", []string{"yaml:token"}},
		{"a single-quoted token", "token: 'abc123'\n", []string{"yaml:token"}},
		{"an indented nested key", "  dolt.password: hunter2\n", []string{"yaml:dolt.password"}},
		{"a list item", "- api_key: abc123\n", []string{"yaml:api_key"}},
		{"an indented list item", "  - api_key: abc123\n", []string{"yaml:api_key"}},
		{"only the last dotted segment decides", "password.count: 3\n", nil},
		{"the last segment is a credential", "vault.secret: abc123\n", []string{"yaml:vault.secret"}},
		{"a trailing comment is cut", "token: abc123 # rotate monthly\n", []string{"yaml:token"}},
		{"a quoted value keeps its hashes", "token: \"abc#123\"\n", []string{"yaml:token"}},
		{"a commented-out line is not a finding", "# password: hunter2\n", nil},
		{"an indented commented-out line is not a finding", "  # password: hunter2\n", nil},
		{"an empty value is not a finding", "password:\npassword: \"\"\n", nil},
		{"an environment reference is not a finding", "password: ${DOLT_PASSWORD}\n", nil},
		{"a _file name is not a finding", "api_key_file: /run/key\n", nil},
		{"a value that is only a comment is not a finding", "password: # none\n", nil},
		{"a config with no credential key", "prefix: gt\nport: 3307\n", nil},
		{"a block scalar header under a credential name is a finding (its body is the secret)", "password: |\n  hunter2\n", []string{"yaml:password"}},
		{"a folded block header with a chomping sign is a finding", "password: >-\n  hunter2\n", []string{"yaml:password"}},
		{"a block header with a comment is a finding", "token: | # rotate quarterly\n  abc123\n", []string{"yaml:token"}},
		{"a mapping entry inside a block scalar is a finding too", "password: |\n  token: abc123\n", []string{"yaml:password", "yaml:token"}},
		{"a quoted bar is a value", "password: \"|\"\n", []string{"yaml:password"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindCredentials([]byte(c.in))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("FindCredentials(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	t.Run("a rule names the key and never the value", func(t *testing.T) {
		got := FindCredentials([]byte("password: hunter2\napi_key: sk-live-123\n"))
		for _, rule := range got {
			if strings.Contains(rule, "hunter2") || strings.Contains(rule, "sk-live-123") {
				t.Fatalf("rule %q holds a value", rule)
			}
		}
	})
}

// A block scalar under a credential name is judged by its body, not its header
// (gt-ultan3). The body is the value: a secret body is a finding for every header
// form, and an empty, reference-only or comment-only body is clean. The body ends
// at the first line at or below the key's indent. The rule names the key only.
func TestFindCredentialsYAMLBlockScalar(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"the exact leak: a secret body under a literal header", "password: |\n  hunter2\n", []string{"yaml:password"}},
		{"a literal header with a strip sign", "password: |-\n  hunter2\n", []string{"yaml:password"}},
		{"a literal header with a keep sign", "password: |+\n  hunter2\n", []string{"yaml:password"}},
		{"a folded header", "password: >\n  hunter2\n", []string{"yaml:password"}},
		{"a folded header with a strip sign", "password: >-\n  hunter2\n", []string{"yaml:password"}},
		{"a folded header with a keep sign", "password: >+\n  hunter2\n", []string{"yaml:password"}},
		{"an indentation digit before the sign", "password: |2-\n  hunter2\n", []string{"yaml:password"}},
		{"an indentation digit after the sign", "password: >-2\n  hunter2\n", []string{"yaml:password"}},
		{"an indentation digit alone", "password: |2\n  hunter2\n", []string{"yaml:password"}},
		{"a folded header with a digit and a sign", "password: >1-\n  hunter2\n", []string{"yaml:password"}},
		{"a list item's key", "- password: |\n    hunter2\n", []string{"yaml:password"}},
		{"a nested key's body", "dolt:\n  password: |\n    hunter2\n", []string{"yaml:password"}},
		{"a secret body ends at the dedent", "password: |\n  hunter2\nprefix: gt\n", []string{"yaml:password"}},
		{"a reference line beside a literal is judged by the literal", "password: |\n  ${A}\n  hunter2\n", []string{"yaml:password"}},
		{"an empty body is clean", "password: |\n", nil},
		{"an empty body before a sibling is clean", "password: |\nprefix: gt\n", nil},
		{"a body of one environment reference is clean", "password: |\n  ${DOLT_PASSWORD}\n", nil},
		{"a multi-line reference body is clean", "password: |\n  ${A}\n  ${B}\n", nil},
		{"a comment-only body is clean", "password: |\n  # rotate me\n", nil},
		{"a folded comment-only body is clean", "password: >-\n  # rotate me\n  # and me\n", nil},
		{"a '#'-prefixed line in a body that is not comment-only is content", "password: |\n  # hunter2\n  ${A}\n", []string{"yaml:password"}},
		{"a sibling key is not read as body", "password: |\ntoken: abc123\n", []string{"yaml:token"}},
		{"a sibling key at the key's indent ends the body", "dolt:\n  password: |\n    hunter2\n  token: abc123\n", []string{"yaml:password", "yaml:token"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FindCredentials([]byte(c.in))
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("FindCredentials(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
	t.Run("a rule names the key and never the body", func(t *testing.T) {
		for _, rule := range FindCredentials([]byte("password: |\n  hunter2\n")) {
			if strings.Contains(rule, "hunter2") {
				t.Fatalf("rule %q holds a body line", rule)
			}
		}
	})
}

// TestFlaggedContentRefusesYAMLCredential walks the new rule through a caller:
// FlaggedContent is the check the export and the backup archive verifier share,
// so a YAML config must be omitted there, not only found by FindCredentials. It
// runs the production entry point, so deleting the yamlLine loop turns every
// refusal row red.
func TestFlaggedContentRefusesYAMLCredential(t *testing.T) {
	t.Parallel()
	refused := []struct {
		name string
		in   string
	}{
		{"a plain password", "prefix: gt\npassword: hunter2\n"},
		{"an indented nested key", "dolt:\n  password: hunter2\n"},
		{"a quoted token", "token: \"abc123\"\n"},
		{"a block scalar secret", "password: |\n  hunter2\n"},
	}
	for _, c := range refused {
		t.Run(c.name, func(t *testing.T) {
			if res := FlaggedContent(strings.NewReader(c.in)); !res.IsFail() {
				t.Fatalf("FlaggedContent(%q) = %v, want a refusal", c.in, res)
			}
		})
	}
	clean := []struct {
		name string
		in   string
	}{
		{"a commented-out line", "prefix: gt\n# password: hunter2\n"},
		{"an environment reference", "password: ${DOLT_PASSWORD}\n"},
		{"an empty block scalar", "password: |\nprefix: gt\n"},
	}
	for _, c := range clean {
		t.Run(c.name, func(t *testing.T) {
			if res := FlaggedContent(strings.NewReader(c.in)); !res.IsPass() {
				t.Fatalf("FlaggedContent(%q) = %v, want a pass", c.in, res)
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
		{"a toleration key holding an array is exempt", `{"tolerations":[{"key":["abc"]}]}`, nil},
		{"the daemon's om_token_meta holding an array is exempt", `{"daemon":{"om_token_meta":["abc"]}}`, nil},
		{"a toleration key array holding a token is still found", `{"tolerations":[{"key":["` + token + `"]}]}`, []string{"value:shape"}},
		{"a toleration key holding an object is exempt", `{"tolerations":[{"key":{"x":"abc"}}]}`, nil},
		{"a toleration key holding a number is exempt", `{"tolerations":[{"key":5}]}`, nil},
		{"a toleration key array holding a number is exempt", `{"tolerations":[{"key":[5]}]}`, nil},
		{"a number under a key elsewhere is found", `{"other":{"key":5}}`, []string{"json:key"}},
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
		{"a credential-named array redacts each element", `{"tokens":["a","b"]}`, `{"tokens":["<redacted>","<redacted>"]}`, 2},
		{"a URL with a password is redacted under any key", `{"repo":"https://user:pw@host/x.git","n":1}`,
			`{"repo":"<redacted>","n":1}`, 1},
		{"a URL with an unparseable password is redacted under any key", `{"repo":"https://user:p%zz@host/x.git","n":1}`,
			`{"repo":"<redacted>","n":1}`, 1},
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

// A credential-named key owns its array: every string element is a json:key
// finding and its whole literal is replaced, so the document stays valid JSON and
// no element survives in it (gt-ourmlj).
func TestRedactJSONRedactsEveryStringInAnArrayUnderACredentialKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		want  string
		wantN int
	}{
		{"an array of strings under api_keys", `{"api_keys":["sk-live-abc","sk-live-def"]}`,
			`{"api_keys":["<redacted>","<redacted>"]}`, 2},
		{"an escaped quote in an element is redacted whole", `{"api_keys":["a\"b","c"],"n":1}`,
			`{"api_keys":["<redacted>","<redacted>"],"n":1}`, 2},
		{"a duplicate key redacts both arrays", `{"api_keys":["one"],"api_keys":["two","three"]}`,
			`{"api_keys":["<redacted>"],"api_keys":["<redacted>","<redacted>"]}`, 3},
		{"a reference, an empty string and a plain keys array stay", `{"api_keys":["$GT_X",""],"keys":["a","b"]}`,
			`{"api_keys":["$GT_X",""],"keys":["a","b"]}`, 0},
		{"a document with no credential key is unchanged", `{"models":["a","b"],"n":[1,2]}`,
			`{"models":["a","b"],"n":[1,2]}`, 0},
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
			if !json.Valid(out) {
				t.Fatalf("output %q is not valid JSON", out)
			}
			if got := FindCredentials(out); len(got) != 0 {
				t.Fatalf("output %q still holds credentials: %q", out, got)
			}
		})
	}
}

// A credential-named key owns a number leaf too: the literal is a json:key finding
// and replaced by the marker, so the document stays valid JSON and no number survives
// in it. Only a number is one: a boolean and a null under such a key stay, and a number
// under another key stays (gt-6seq32).
func TestRedactJSONRedactsANumberUnderACredentialKey(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		want  string
		wantN int
	}{
		{"a number under token", `{"token":123456789}`,
			`{"token":"<redacted>"}`, 1},
		{"a number inside an array under api_keys", `{"api_keys":[111,222],"n":1}`,
			`{"api_keys":["<redacted>","<redacted>"],"n":1}`, 2},
		{"a negative and a decimal number are replaced whole", `{"token":-42,"password":3.5}`,
			`{"token":"<redacted>","password":"<redacted>"}`, 2},
		{"every other byte around the number is kept", `{"token": 123456789 , "b": 1}`,
			`{"token": "<redacted>" , "b": 1}`, 1},
		{"a boolean and a null under a credential key stay", `{"password":true,"secret":null}`,
			`{"password":true,"secret":null}`, 0},
		{"a number under another name stays", `{"n":1,"models":[1,2]}`,
			`{"n":1,"models":[1,2]}`, 0},
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
			if !json.Valid(out) {
				t.Fatalf("output %q is not valid JSON", out)
			}
			if got := FindCredentials(out); len(got) != 0 {
				t.Fatalf("output %q still holds credentials: %q", out, got)
			}
		})
	}
}

// The exemptions of the number rule, pinned from the value that stays: the tokens
// segment exemption in isSecret leaves a token count whole, a number under a
// non-credential key is never owned, and a boolean or null under a credential key
// never reaches numberHit. Each row checks the input as well as the output, so a
// row that starts redacting cannot pass on a clean-looking document (gt-sqxfuc).
func TestRedactJSONNumberExemptions(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
	}{
		{"a max_tokens count is exempt by shape", `{"max_tokens":4096}`},
		{"a number under a non-credential key stays", `{"n":1,"count":42}`},
		{"a boolean and a null under a credential key stay", `{"password":true,"secret":null}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := FindCredentials([]byte(c.in)); len(got) != 0 {
				t.Fatalf("FindCredentials(%q) = %q, want no findings", c.in, got)
			}
			out, n, err := RedactJSON([]byte(c.in))
			if err != nil {
				t.Fatalf("RedactJSON(%q): %v", c.in, err)
			}
			if string(out) != c.in || n != 0 {
				t.Fatalf("RedactJSON(%q) = %q, %d; want the input untouched, 0", c.in, out, n)
			}
			if got := FindCredentials(out); len(got) != 0 {
				t.Fatalf("output %q still holds credentials: %q", out, got)
			}
		})
	}
}

// A credential-named key owns the whole value under it, nested objects and arrays
// included: every string and number leaf is a json:key finding and is replaced by
// the marker, whatever the name of the key that holds it. A boolean, a null, an
// empty string, a reference and a leaf under a plain key stay (gt-pwfy1z).
func TestRedactJSONRedactsEveryStringLeafOfANestedObject(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		want  string
		wantN int
	}{
		{"a nested object under password", `{"password":{"value":"hunter2"}}`,
			`{"password":{"value":"<redacted>"}}`, 1},
		{"an array of objects under password", `{"password":[{"secret":"x"},{"name":"y"}]}`,
			`{"password":[{"secret":"<redacted>"},{"name":"<redacted>"}]}`, 2},
		{"a nested non-credential key stays under the credential key", `{"api_key":{"outer":{"inner":"v"}}}`,
			`{"api_key":{"outer":{"inner":"<redacted>"}}}`, 1},
		{"a number leaf of a nested object is replaced whole", `{"token":{"n":5,"m":"x"}}`,
			`{"token":{"n":"<redacted>","m":"<redacted>"}}`, 2},
		{"a boolean, a null, an empty string and a reference stay", `{"password":{"on":true,"off":null,"e":"","r":"$GT_X"}}`,
			`{"password":{"on":true,"off":null,"e":"","r":"$GT_X"}}`, 0},
		{"a nested object under another name stays", `{"n":{"name":"y"}}`,
			`{"n":{"name":"y"}}`, 0},
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
			if !json.Valid(out) {
				t.Fatalf("output %q is not valid JSON", out)
			}
			if got := FindCredentials(out); len(got) != 0 {
				t.Fatalf("output %q still holds credentials: %q", out, got)
			}
		})
	}
}

func TestRedactJSONRefusesWhatIsNotJSON(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"API_KEY=abc\n", "password: hunter2\n", `{"api_key":"abc"`, ""} {
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

// A number literal outside float64's range (9e99999) must not hide the document: the
// decoder reads it as a number token, so a secret nested under a credential key is
// still a finding, and the literal under a plain key is not one (gt-xctije).
func TestFindCredentialsOutOfRangeNumber(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"a secret in a nested object survives an out-of-range literal elsewhere",
			`{"x":9e99999,"password":{"v":"hunter2hunter2"}}`, []string{"json:password"}},
		{"a secret in an array under api_key survives an out-of-range literal elsewhere",
			`{"x":9e99999,"api_key":["hunter2hunter2"]}`, []string{"json:api_key"}},
		{"an out-of-range literal under a credential key is a finding",
			`{"password":9e99999}`, []string{"json:password"}},
		{"an out-of-range literal under a plain key is not a finding",
			`{"x":9e99999,"y":1}`, nil},
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

// RedactJSON on a document with an out-of-range literal replaces the secret and the
// literal under a credential key, leaves the literal under a plain key, and returns
// valid JSON that no longer holds a finding (gt-xctije).
func TestRedactJSONOutOfRangeNumber(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		in    string
		want  string
		wantN int
	}{
		{"the nested secret is replaced and the plain literal kept",
			`{"x":9e99999,"password":{"v":"hunter2hunter2"}}`,
			`{"x":9e99999,"password":{"v":"<redacted>"}}`, 1},
		{"the array secret under api_key is replaced",
			`{"x":9e99999,"api_key":["hunter2hunter2"]}`,
			`{"x":9e99999,"api_key":["<redacted>"]}`, 1},
		{"the out-of-range literal under a credential key is replaced",
			`{"password":9e99999}`,
			`{"password":"<redacted>"}`, 1},
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
			if !json.Valid(out) {
				t.Fatalf("output %q is not valid JSON", out)
			}
			if got := FindCredentials(out); len(got) != 0 {
				t.Fatalf("output %q still holds credentials: %q", out, got)
			}
		})
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
