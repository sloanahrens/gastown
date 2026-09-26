package cmd

import (
	"regexp"
	"strings"
	"testing"
)

func TestAutoKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "basic words",
			content: "Refinery uses worktree for merges",
			want:    "refinery-uses-worktree-for-merges",
		},
		{
			name:    "more than 5 words truncated",
			content: "Always use stdin for multi line mail messages",
			want:    "always-use-stdin-for-multi",
		},
		{
			name:    "strips punctuation",
			content: "Don't use rm -rf on .dolt-data/",
			want:    "dont-use-rm-rf-on",
		},
		{
			name:    "single word",
			content: "important",
			want:    "important",
		},
		{
			name:    "mixed case",
			content: "Hooks Package Structure",
			want:    "hooks-package-structure",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := autoKey(tt.content)
			if got != tt.want {
				t.Errorf("autoKey(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestSanitizeKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		key  string
		want string
	}{
		{
			name: "already clean",
			key:  "refinery-worktree",
			want: "refinery-worktree",
		},
		{
			name: "spaces to hyphens",
			key:  "refinery worktree",
			want: "refinery-worktree",
		},
		{
			name: "dots to hyphens",
			key:  "gt.slug",
			want: "gt-slug",
		},
		{
			name: "uppercase to lower",
			key:  "MyKey",
			want: "mykey",
		},
		{
			name: "strip special chars",
			key:  "key@#$%value",
			want: "keyvalue",
		},
		{
			name: "collapse multiple hyphens",
			key:  "key---value",
			want: "key-value",
		},
		{
			name: "trim leading/trailing hyphens",
			key:  "-key-value-",
			want: "key-value",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := sanitizeKey(tt.key)
			if got != tt.want {
				t.Errorf("sanitizeKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

func TestParseMemoryKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		kvKey    string
		wantType string
		wantKey  string
	}{
		{
			name:     "typed feedback key",
			kvKey:    "gt.feedback.dont-mock-db",
			wantType: "feedback",
			wantKey:  "dont-mock-db",
		},
		{
			name:     "typed project key",
			kvKey:    "gt.project.merge-freeze",
			wantType: "project",
			wantKey:  "merge-freeze",
		},
		{
			name:     "typed user key",
			kvKey:    "gt.user.senior-go-dev",
			wantType: "user",
			wantKey:  "senior-go-dev",
		},
		{
			name:     "typed reference key",
			kvKey:    "gt.reference.grafana-dashboard",
			wantType: "reference",
			wantKey:  "grafana-dashboard",
		},
		{
			name:     "typed general key",
			kvKey:    "gt.general.some-insight",
			wantType: "general",
			wantKey:  "some-insight",
		},
		{
			name:     "legacy untyped key",
			kvKey:    "gt.refinery-worktree",
			wantType: "general",
			wantKey:  "refinery-worktree",
		},
		{
			name:     "legacy key with dots in slug",
			kvKey:    "gt.hooks-package-structure",
			wantType: "general",
			wantKey:  "hooks-package-structure",
		},
		{
			name:     "unknown type treated as legacy",
			kvKey:    "gt.banana.split",
			wantType: "general",
			wantKey:  "banana.split",
		},
		{
			name:     "typed key with hyphens in value",
			kvKey:    "gt.feedback.always-use-race-flag",
			wantType: "feedback",
			wantKey:  "always-use-race-flag",
		},
		{
			name:     "memory. typed feedback key",
			kvKey:    "memory.feedback.always-use-race-flag",
			wantType: "feedback",
			wantKey:  "always-use-race-flag",
		},
		{
			name:     "memory. typed project key",
			kvKey:    "memory.project.merge-freeze",
			wantType: "project",
			wantKey:  "merge-freeze",
		},
		{
			name:     "memory. legacy untyped key",
			kvKey:    "memory.refinery-worktree",
			wantType: "general",
			wantKey:  "refinery-worktree",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotType, gotKey := parseMemoryKey(tt.kvKey)
			if gotType != tt.wantType {
				t.Errorf("parseMemoryKey(%q) type = %q, want %q", tt.kvKey, gotType, tt.wantType)
			}
			if gotKey != tt.wantKey {
				t.Errorf("parseMemoryKey(%q) key = %q, want %q", tt.kvKey, gotKey, tt.wantKey)
			}
		})
	}
}

func TestMemTypeRank(t *testing.T) {
	t.Parallel()
	// feedback should come before general
	if memTypeRank("feedback") >= memTypeRank("general") {
		t.Error("feedback should rank before general")
	}
	// user should come before project
	if memTypeRank("user") >= memTypeRank("project") {
		t.Error("user should rank before project")
	}
	// unknown types should sort last
	if memTypeRank("unknown") <= memTypeRank("general") {
		t.Error("unknown type should rank after general")
	}
}

func TestParseBdKvListJSON(t *testing.T) {
	t.Parallel()
	got, err := parseBdKvListJSON([]byte(`{
		"gt.project.note":"keep me",
		"gt.project.empty":"",
		"gt.project.count":12,
		"gt.project.enabled":true,
		"gt.project.tags":["one"],
		"gt.project.config":{"nested":"value"},
		"schema_version":1,
		"other":"keep string kvs",
		"enabled":true,
		"tags":["one"],
		"config":{"nested":"value"},
		"gt.project.null":null
	}`))
	if err != nil {
		t.Fatalf("parseBdKvListJSON() error = %v", err)
	}

	want := map[string]string{
		"gt.project.note":    "keep me",
		"gt.project.empty":   "",
		"gt.project.count":   "12",
		"gt.project.enabled": "true",
		"gt.project.tags":    `["one"]`,
		"gt.project.config":  `{"nested":"value"}`,
		"other":              "keep string kvs",
	}
	if len(got) != len(want) {
		t.Fatalf("parseBdKvListJSON() returned %d entries, want %d: %#v", len(got), len(want), got)
	}
	for k, wantValue := range want {
		if got[k] != wantValue {
			t.Errorf("parseBdKvListJSON()[%q] = %q, want %q", k, got[k], wantValue)
		}
	}
	if _, ok := got["gt.project.null"]; ok {
		t.Error("parseBdKvListJSON() kept null memory value")
	}
	for _, k := range []string{"schema_version", "enabled", "tags", "config"} {
		if _, ok := got[k]; ok {
			t.Errorf("parseBdKvListJSON() kept non-memory non-string value for %q", k)
		}
	}
}

func TestParseBdKvListJSONMalformed(t *testing.T) {
	t.Parallel()
	if _, err := parseBdKvListJSON([]byte(`{"gt.project.note":`)); err == nil {
		t.Fatal("parseBdKvListJSON() error = nil, want malformed JSON error")
	}
}

func TestResolveMemoryAuthor(t *testing.T) {
	tests := []struct {
		name       string
		bdActor    string
		gtRole     string
		wantAuthor string
	}{
		{
			name:       "BD_ACTOR takes precedence",
			bdActor:    "gastown/refinery",
			gtRole:     "gastown/witness",
			wantAuthor: "gastown/refinery",
		},
		{
			name:       "falls back to GT_ROLE when BD_ACTOR unset",
			bdActor:    "",
			gtRole:     "gastown/witness",
			wantAuthor: "gastown/witness",
		},
		{
			name:       "empty when neither set",
			bdActor:    "",
			gtRole:     "",
			wantAuthor: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("BD_ACTOR", tt.bdActor)
			t.Setenv("GT_ROLE", tt.gtRole)
			if got := resolveMemoryAuthor(); got != tt.wantAuthor {
				t.Errorf("resolveMemoryAuthor() = %q, want %q", got, tt.wantAuthor)
			}
		})
	}
}

func TestAttributionSuffix(t *testing.T) {
	t.Parallel()
	if got := attributionSuffix("", nil); got != "" {
		t.Errorf("attributionSuffix(\"\", nil) = %q, want empty (no fabricated identity)", got)
	}

	got := attributionSuffix("gastown/refinery", nil)
	want := "\n\n[recorded by: gastown/refinery @ "
	if !strings.HasPrefix(got, want) {
		t.Errorf("attributionSuffix(%q, nil) = %q, want prefix %q", "gastown/refinery", got, want)
	}

	got = attributionSuffix("gastown/refinery", []string{"gastown/witness", " "})
	want = "\n\n[recorded by: gastown/refinery + gastown/witness @ "
	if !strings.HasPrefix(got, want) {
		t.Errorf("attributionSuffix with --with = %q, want prefix %q", got, want)
	}
}

func TestDetectUnattributedJointVoice(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "matches the gt-04h incident phrasing",
			content: "Note refinery and I ran this independently and converged, which is the only control we managed all night",
			want:    "refinery",
		},
		{
			name:    "case-insensitive role, still requires capital I",
			content: "Witness and I disagreed at first.",
			want:    "witness",
		},
		{
			name:    "no match without a known role name",
			content: "Sloan and I discussed this over coffee.",
			want:    "",
		},
		{
			name:    "no match for role name alone",
			content: "The refinery gates every MR before merging.",
			want:    "",
		},
		{
			name:    "no false positive on 'and it'",
			content: "The polecat and it agreed on nothing.",
			want:    "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := detectUnattributedJointVoice(tt.content); got != tt.want {
				t.Errorf("detectUnattributedJointVoice(%q) = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

func TestOrElse(t *testing.T) {
	t.Parallel()
	if got := orElse("gastown/refinery", "no one"); got != "gastown/refinery" {
		t.Errorf("orElse() = %q, want %q", got, "gastown/refinery")
	}
	if got := orElse("", "no one"); got != "no one" {
		t.Errorf("orElse() = %q, want %q", got, "no one")
	}
}

// TestRememberHelpNamesExactlyTheGatedRoles pins the injection claim in
// `gt remember --help` to the gate that implements it (shouldRenderMemories).
// The claim once read "injected during gt prime" with no mention of the role
// gate, so agents outside mayor and crew stored memories no prime of theirs
// would ever show (gt-n976). The same drift recurs if a role enters or leaves
// the gate without the help text following, in either direction.
func TestRememberHelpNamesExactlyTheGatedRoles(t *testing.T) {
	t.Parallel()

	// A role named in the Examples as a --with target is not a claim about
	// injection, so the check covers the text ahead of that section.
	claim, _, _ := strings.Cut(rememberCmd.Long, "Examples:")

	for _, role := range AllRoles() {
		name := string(role)
		named := regexp.MustCompile(`\b` + regexp.QuoteMeta(name) + `\b`).MatchString(claim)
		if want := shouldRenderMemories(name); named != want {
			t.Errorf("`gt remember --help` names role %q: %v; shouldRenderMemories(%q) = %v — the claim and the gate must agree",
				name, named, name, want)
		}
	}
}
