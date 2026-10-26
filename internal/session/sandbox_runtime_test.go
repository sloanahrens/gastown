package session

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/sandbox"
)

// runtimeEntry is one extracted archive member: its body and tar mode.
type runtimeEntry struct {
	body string
	mode int64
}

func untarRuntime(t *testing.T, data []byte) map[string]runtimeEntry {
	t.Helper()
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]runtimeEntry{}
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		got[h.Name] = runtimeEntry{body: string(body), mode: h.Mode}
	}
}

// The sandbox copy of the system prompt ends with the rule that keeps a
// placeholder out of the conversation.
func TestBuildRuntimeArchiveAppendsTheSandboxNoteToThePrompt(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	data, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, openshellPromptRender)
	if err != nil {
		t.Fatalf("buildRuntimeArchive: %v", err)
	}
	prompt := untarRuntime(t, data)["gt/"+openshellTestRig+"/polecats/.claude/system-prompt-"+openshellTestPolecat+".md"].body
	if !strings.HasPrefix(prompt, "town at ") {
		t.Fatalf("the polecat's own prompt is missing from the sandbox prompt:\n%s", prompt)
	}
	if !strings.HasSuffix(prompt, sandboxPromptNote) {
		t.Errorf("the sandbox note must follow the polecat's own prompt and end it:\n%s", prompt)
	}
}

func TestBuildRuntimeArchiveHoldsTheRenderedSettingsAndPrompt(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	data, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, openshellPromptRender)
	if err != nil {
		t.Fatalf("buildRuntimeArchive: %v", err)
	}
	got := untarRuntime(t, data)
	dir := "gt/" + openshellTestRig + "/polecats/.claude/"
	if len(got) != 5 {
		t.Fatalf("archive entries = %v, want .claude.json, .gitconfig, .githelper, settings.json and the system prompt", got)
	}
	if !strings.Contains(got[dir+"settings.json"].body, "/usr/local/bin/gt prime --hook") {
		t.Errorf("settings.json was not rendered for the sandbox:\n%s", got[dir+"settings.json"].body)
	}
	if prompt := got[dir+"system-prompt-"+openshellTestPolecat+".md"].body; !strings.HasPrefix(prompt, "town at /sandbox/gt/mayor\n") {
		t.Errorf("system prompt = %q, want the town root rewritten", prompt)
	}
}

// A polecat without its system prompt file fails the build and the error names
// the file: a polecat must not start with none of its policy (gt-uoo5j).
func TestBuildRuntimeArchiveFailsWithoutAPromptNamingTheFile(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	claudeDir := filepath.Join(town, openshellTestRig, "polecats", ".claude")
	promptName := "system-prompt-" + openshellTestPolecat + ".md"
	if err := os.Remove(filepath.Join(claudeDir, promptName)); err != nil {
		t.Fatal(err)
	}
	_, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, openshellPromptRender)
	if err == nil {
		t.Fatal("buildRuntimeArchive without a system prompt = nil error; the polecat would start with none of its policy")
	}
	if !strings.Contains(err.Error(), promptName) {
		t.Errorf("error does not name the missing file %s: %v", promptName, err)
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("error does not wrap the not-exist cause: %v", err)
	}
}

// The uploaded prompt's hash line is the hash of the bytes the archive carries:
// the town root rewritten and the sandbox note appended, not the file as written.
func TestBuildRuntimeArchiveLogsTheHashOfTheUploadedPrompt(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	claudeDir := filepath.Join(town, openshellTestRig, "polecats", ".claude")
	raw, err := os.ReadFile(filepath.Join(claudeDir, "system-prompt-"+openshellTestPolecat+".md"))
	if err != nil {
		t.Fatal(err)
	}
	// The real build, with its spawn log captured: the line it writes must be
	// the hash of the entry it put in the archive, read back from the archive.
	var logged bytes.Buffer
	data, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, &logged, openshellPromptRender)
	if err != nil {
		t.Fatalf("buildRuntimeArchive: %v", err)
	}
	uploaded := untarRuntime(t, data)["gt/"+openshellTestRig+"/polecats/.claude/system-prompt-"+openshellTestPolecat+".md"].body
	want := RewriteTownRoot(string(raw), town, sandboxTownRoot) + sandboxPromptNote
	if uploaded != want {
		t.Fatalf("uploaded prompt is not the rewritten prompt with the sandbox note:\n%s", uploaded)
	}
	sum := sha256.Sum256([]byte(uploaded))
	line := "system prompt " + openshellTestPolecat + ": " + strconv.Itoa(len(uploaded)) + " bytes sha256 " + hex.EncodeToString(sum[:]) + "\n"
	if logged.String() != line {
		t.Errorf("spawn log = %q, want the hash of the uploaded entry %q", logged.String(), line)
	}
}

func TestBuildRuntimeArchiveFailsWithoutSettings(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	claudeDir := filepath.Join(town, openshellTestRig, "polecats", ".claude")
	if err := os.Remove(filepath.Join(claudeDir, "settings.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, openshellPromptRender); err == nil {
		t.Fatal("buildRuntimeArchive without settings.json = nil error; the agent command names that file")
	}
}

// A prompt that exists but cannot be read stops the archive; only a missing
// one is skipped.
func TestBuildRuntimeArchiveReportsAnUnreadablePrompt(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	prompt := filepath.Join(town, openshellTestRig, "polecats", ".claude", "system-prompt-"+openshellTestPolecat+".md")
	if err := os.Remove(prompt); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(prompt, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, openshellPromptRender); err == nil {
		t.Fatal("buildRuntimeArchive with a directory for the system prompt = nil error")
	}
}

// Settings that do not render stop the archive rather than upload a
// half-rendered file.
func TestBuildRuntimeArchiveReportsSettingsThatDoNotRender(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	writeOpenshellFile(t, filepath.Join(town, openshellTestRig, "polecats", ".claude", "settings.json"), "{not json")
	if _, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, openshellPromptRender); err == nil {
		t.Fatal("buildRuntimeArchive with invalid settings JSON = nil error")
	}
}

// A rig or polecat name that is not one path component must not reach the
// host settings path or the tar entry names.
func TestBuildRuntimeArchiveRefusesTraversalInRigOrPolecat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		rig     string
		polecat string
	}{
		{"rig dotdot", "..", openshellTestPolecat},
		{"rig traversal", "../escape", openshellTestPolecat},
		{"rig separator", "a/b", openshellTestPolecat},
		{"polecat dotdot", openshellTestRig, ".."},
		{"polecat traversal", openshellTestRig, "../escape"},
		{"polecat backslash", openshellTestRig, `a\b`},
		{"empty polecat", openshellTestRig, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			town, work := openshellTown(t)
			spec := openshellSpawn(town, work)
			spec.Rig, spec.Polecat = tc.rig, tc.polecat
			_, err := buildRuntimeArchiveLogging(spec, openshellTestGitEmail, io.Discard, openshellPromptRender)
			if err == nil || !strings.Contains(err.Error(), "not a single path segment") {
				t.Fatalf("buildRuntimeArchive with rig=%q polecat=%q: err=%v, want a path-segment refusal", tc.rig, tc.polecat, err)
			}
		})
	}
}

// A polecat whose settings are missing must stop Prepare before the branch is
// pushed or any sandbox exists.
func TestOpenShellPrepareWithoutSettingsTouchesNothing(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	if err := os.Remove(filepath.Join(town, openshellTestRig, "polecats", ".claude", "settings.json")); err != nil {
		t.Fatal(err)
	}
	fake := &sandbox.Fake{Reply: map[string]string{"get": openshellReadyReply}}
	b, pushes := openshellTestBackend(town, openshellTestProfile, fake)

	if _, err := b.Prepare(context.Background(), openshellSpawn(town, work)); err == nil {
		t.Fatal("Prepare = nil error without settings.json")
	}
	if len(*pushes) != 0 || len(fake.Calls()) != 0 {
		t.Fatalf("pushes=%v calls=%q; a bad polecat must touch nothing", *pushes, fake.Calls())
	}
}

// A fresh sandbox stops at Claude Code's theme picker unless ~/.claude.json
// says onboarding is done; the worktree must be trusted too (sling 7).
func TestBuildRuntimeArchiveSeedsClaudeJSONForTheSandboxWorktree(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	spec := openshellSpawn(town, work)
	data, err := buildRuntimeArchiveLogging(spec, openshellTestGitEmail, io.Discard, openshellPromptRender)
	if err != nil {
		t.Fatalf("buildRuntimeArchive: %v", err)
	}
	entry, ok := untarRuntime(t, data)[".claude.json"]
	if !ok {
		t.Fatal("the archive has no .claude.json")
	}
	body := entry.body
	rel, err := sandboxRel(spec)
	if err != nil {
		t.Fatalf("sandboxRel: %v", err)
	}
	wantWorkDir := path.Join(sandboxTownRoot, rel)
	var cfg struct {
		Onboarded bool   `json:"hasCompletedOnboarding"`
		Theme     string `json:"theme"`
		Projects  map[string]struct {
			Trusted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatalf(".claude.json is not JSON: %v\n%s", err, body)
	}
	if !cfg.Onboarded || cfg.Theme != "dark" {
		t.Errorf("onboarded=%v theme=%q, want onboarding done and the dark theme", cfg.Onboarded, cfg.Theme)
	}
	if len(cfg.Projects) != 1 || !cfg.Projects[wantWorkDir].Trusted {
		t.Errorf("projects = %v, want exactly %s trusted", cfg.Projects, wantWorkDir)
	}
	if entry.mode != 0o600 {
		t.Errorf(".claude.json mode = %#o, want 0600", entry.mode)
	}
	if strings.Contains(body, town) || strings.Contains(body, "/Users/") {
		t.Errorf(".claude.json names a host path:\n%s", body)
	}
}

// Plain git (and gt done's push) must authenticate inside a sandbox with nothing
// secret in a file: the credential helper reads the token from the environment
// when git asks, and git config prints no token (sling 10, 2026-10-23: the agent
// hit a git auth error and dumped the environment to find out why).
func TestBuildRuntimeArchiveGivesGitACredentialHelperAndNoSecret(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	data, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, openshellPromptRender)
	if err != nil {
		t.Fatalf("buildRuntimeArchive: %v", err)
	}
	got := untarRuntime(t, data)

	cfgEntry, ok := got[".gitconfig"]
	if !ok {
		t.Fatal("the archive has no .gitconfig")
	}
	cfg := cfgEntry.body
	for _, want := range []string{"name = " + openshellTestPolecat, "email = " + openshellTestGitEmail, "helper = /sandbox/.githelper"} {
		if !strings.Contains(cfg, want) {
			t.Errorf(".gitconfig lacks %q:\n%s", want, cfg)
		}
	}
	helperEntry, ok := got[".githelper"]
	if !ok {
		t.Fatal("the archive has no .githelper")
	}
	helper := helperEntry.body
	if helperEntry.mode&0o111 == 0 {
		t.Errorf(".githelper mode = %o, want executable (git runs it as a program)", helperEntry.mode)
	}
	if !strings.HasPrefix(helper, "#!/bin/sh\n") || !strings.Contains(helper, `"password=$FORGEJO_TOKEN"`) {
		t.Errorf(".githelper does not read the token from the environment:\n%s", helper)
	}
	for name, body := range map[string]string{".gitconfig": cfg, ".githelper": helper} {
		if strings.Contains(body, "openshell:resolve") || strings.Contains(strings.ToLower(body), "authorization") {
			t.Errorf("%s holds a credential or an auth header:\n%s", name, body)
		}
	}
}

// The helper answers only for the Forgejo host the clone URL names, so the token
// placeholder never goes to another host's credential prompt. The behaviour (a
// request for github.com prints nothing) is replayed in a sandbox by replay.sh;
// here the generated script is pinned to the host and the early exit.
func TestGitHelperScriptAnswersOnlyForTheForgejoHost(t *testing.T) {
	t.Parallel()
	script := gitHelperScript("host.docker.internal:3000")
	for _, want := range []string{
		`[ "$1" = get ] || exit 0`,
		`case "$line" in host=*) host=${line#host=} ;; esac`,
		`[ "$host" = "host.docker.internal:3000" ] || exit 0`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("the helper lacks %q:\n%s", want, script)
		}
	}
	// The credentials are printed only after the host check and the token check.
	hostCheck := strings.Index(script, `[ "$host" = "host.docker.internal:3000" ] || exit 0`)
	tokenCheck := strings.Index(script, `[ -n "$FORGEJO_TOKEN" ] || exit 0`)
	password := strings.Index(script, `echo "password=$FORGEJO_TOKEN"`)
	if hostCheck < 0 || tokenCheck < 0 || password < 0 || hostCheck > password || tokenCheck > password {
		t.Errorf("credentials must follow the host check and the token check (host at %d, token at %d, password at %d):\n%s", hostCheck, tokenCheck, password, script)
	}
}

func TestRepoHostIsTheCloneURLsHostAndPort(t *testing.T) {
	t.Parallel()
	if got, err := SandboxRepoHost("http://host.docker.internal:3000/sloan/gastown.git"); err != nil || got != "host.docker.internal:3000" {
		t.Errorf("SandboxRepoHost = %q, %v; want host.docker.internal:3000", got, err)
	}
	for _, bad := range []string{"", "not a url", "http:///no-host", `http://evil"; rm -rf /;:3000/x`, "http://a b:3000/x", "http://host:3000:1/x", "http://ho$(id)st:3000/x", "http://host:99999999/x", "http://-host:3000/x", "http://a..b:3000/x", "http://[fd00::1]:3000/x"} {
		if got, err := SandboxRepoHost(bad); err == nil {
			t.Errorf("SandboxRepoHost(%q) = %q, nil; want an error (the host goes into the helper script)", bad, got)
		}
	}
}

// Commits in a sandbox carry the host's global email, as host polecats' do; the
// polecat@… form stands in when the host has none.
func TestSandboxGitConfigUsesTheHostEmailAndFallsBack(t *testing.T) {
	t.Parallel()
	if cfg := sandboxGitConfig("ace", "dev@example.test"); !strings.Contains(cfg, "email = dev@example.test") || !strings.Contains(cfg, "name = ace") {
		t.Errorf("config does not carry the host email and the polecat name:\n%s", cfg)
	}
	if cfg := sandboxGitConfig("ace", ""); !strings.Contains(cfg, "email = ace@gastown.local") {
		t.Errorf("config does not fall back to ace@gastown.local when the host has no email:\n%s", cfg)
	}
}

func TestSandboxGitConfigRefusesAnUnsafeEmail(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"a b@example.test", "x@example.test\n[core]", "x@exa\"mple.test", "<x@example.test>", "no-at-sign"} {
		if cfg := sandboxGitConfig("ace", bad); strings.Contains(cfg, bad) || !strings.Contains(cfg, "email = ace@gastown.local") {
			t.Errorf("email %q reached .gitconfig or the fallback was not used:\n%s", bad, cfg)
		}
	}
}

func TestGitEmailFromTrimsTheAnswerAndTreatsAFailureAsUnset(t *testing.T) {
	t.Parallel()
	var asked []string
	got := gitEmailFrom(func(name string, args ...string) ([]byte, error) {
		asked = append([]string{name}, args...)
		return []byte("dev@example.test\n"), nil
	})
	if got != "dev@example.test" || strings.Join(asked, " ") != "git config --global user.email" {
		t.Errorf("gitEmailFrom = %q after asking %v", got, asked)
	}
	if got := gitEmailFrom(func(string, ...string) ([]byte, error) { return nil, errors.New("exit status 1") }); got != "" {
		t.Errorf("gitEmailFrom after a git failure = %q, want empty (unset)", got)
	}
}

// openshellPromptRender is the renderer the archive tests pass in. It writes the host's prompt file
// to path the way the real renderer does: the host text for a host target, and for a sandbox target
// the host town root rewritten and the sandbox note appended (gt-uvwi6). The host file is the one
// beside the sandbox/ directory path sits in.
func openshellPromptRender(_, townRoot, _, _, path string, target config.SystemPromptTarget) error {
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(filepath.Dir(path)), filepath.Base(path)))
	if err != nil {
		return err
	}
	text := string(raw)
	if target.Sandbox {
		text = RewriteTownRoot(text, townRoot, target.SandboxTownRoot) + target.Note
	}
	return os.WriteFile(path, []byte(text), 0o644)
}

// The tar entry, the file the host rendered and the logged hash are the same bytes: the upload copies
// the render verbatim, so nothing between the render and the archive can change the prompt (gt-uvwi6).
func TestBuildRuntimeArchiveUploadsTheRenderedPromptVerbatim(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	var logged bytes.Buffer
	data, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, &logged, openshellPromptRender)
	if err != nil {
		t.Fatalf("buildRuntimeArchiveLogging: %v", err)
	}
	rendered, err := os.ReadFile(filepath.Join(town, openshellTestRig, "polecats", ".claude", "sandbox", "system-prompt-"+openshellTestPolecat+".md"))
	if err != nil {
		t.Fatalf("rendered sandbox prompt not on disk: %v", err)
	}
	uploaded := untarRuntime(t, data)["gt/"+openshellTestRig+"/polecats/.claude/system-prompt-"+openshellTestPolecat+".md"].body
	if uploaded != string(rendered) {
		t.Fatalf("tar entry is not the rendered file byte for byte:\n%.400s", uploaded)
	}
	sum := sha256.Sum256(rendered)
	line := "system prompt " + openshellTestPolecat + ": " + strconv.Itoa(len(rendered)) + " bytes sha256 " + hex.EncodeToString(sum[:]) + "\n"
	if logged.String() != line {
		t.Errorf("logged hash = %q, want the hash of the rendered file %q", logged.String(), line)
	}
}

// With no renderer installed the prompt cannot be rendered, so the build refuses and names the file:
// the polecat would otherwise start with none of its policy (gt-uvwi6, gt-uoo5j).
func TestBuildRuntimeArchiveRefusesWithoutAPromptRendererNamingTheFile(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	_, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, nil)
	if err == nil {
		t.Fatal("buildRuntimeArchiveLogging with no renderer = nil error; the polecat would start with none of its policy")
	}
	if !strings.Contains(err.Error(), "system-prompt-"+openshellTestPolecat+".md") {
		t.Errorf("error does not name the prompt file: %v", err)
	}
}

// A render that fails refuses the build, names the prompt file and keeps the cause (gt-uvwi6).
func TestBuildRuntimeArchiveFailsWhenThePromptWillNotRenderNamingTheFile(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	cause := errors.New("template unavailable")
	fail := func(_, _, _, _, _ string, _ config.SystemPromptTarget) error { return cause }
	_, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, fail)
	if !errors.Is(err, cause) {
		t.Fatalf("error = %v, want the render failure wrapped", err)
	}
	if !strings.Contains(err.Error(), "system-prompt-"+openshellTestPolecat+".md") {
		t.Errorf("error does not name the prompt file: %v", err)
	}
}

// A renderer that reports success but leaves no file cannot be uploaded: the build refuses and names the
// file, rather than uploading an empty or missing prompt (gt-uvwi6).
func TestBuildRuntimeArchiveRefusesARenderThatWritesNoFileNamingTheFile(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	silent := func(_, _, _, _, _ string, _ config.SystemPromptTarget) error { return nil }
	_, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, silent)
	if !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("error = %v, want the missing rendered file wrapped", err)
	}
	if !strings.Contains(err.Error(), "system-prompt-"+openshellTestPolecat+".md") {
		t.Errorf("error does not name the prompt file: %v", err)
	}
}

// A file where the sandbox render directory must go stops the render before it starts: the build refuses,
// names the prompt file and keeps the cause (gt-uvwi6).
func TestBuildRuntimeArchiveRefusesWhenTheRenderDirectoryCannotBeMade(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	claudeDir := filepath.Join(town, openshellTestRig, "polecats", ".claude")
	if err := os.WriteFile(filepath.Join(claudeDir, "sandbox"), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	rendered := false
	render := func(_, _, _, _, _ string, _ config.SystemPromptTarget) error { rendered = true; return nil }
	_, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, io.Discard, render)
	if err == nil {
		t.Fatal("buildRuntimeArchiveLogging with no render directory = nil error")
	}
	if rendered {
		t.Error("the prompt was rendered although its directory could not be made")
	}
	if !strings.Contains(err.Error(), "system-prompt-"+openshellTestPolecat+".md") {
		t.Errorf("error does not name the prompt file: %v", err)
	}
}

// A store that cannot be written is logged in the spawn log, and the archive is
// still built: the prompt store never fails a spawn (gt-js6di).
func TestBuildRuntimeArchiveSurvivesAStoreFailure(t *testing.T) {
	t.Parallel()
	town, work := openshellTown(t)
	// A file where the store directory should be makes the store fail.
	if err := os.MkdirAll(filepath.Join(town, ".runtime"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(town, polecatPromptDir), []byte("not a directory"), 0o644); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	if _, err := buildRuntimeArchiveLogging(openshellSpawn(town, work), openshellTestGitEmail, &logged, openshellPromptRender); err != nil {
		t.Fatalf("a store failure failed the spawn: %v", err)
	}
	if !strings.Contains(logged.String(), "system prompt store "+openshellTestPolecat+":") {
		t.Errorf("the store failure is not in the spawn log:\n%s", logged.String())
	}
}
