package ci

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/steveyegge/gastown/internal/ci/preflight"
	"github.com/steveyegge/gastown/internal/imagevalues"
	"github.com/steveyegge/gastown/internal/land"
)

// workflowPath is the images workflow text, relative to this package: the file
// the operator copies to .forgejo/workflows/images.yml.
const workflowPath = "../../deploy/ci/images.workflow.yml"

// helperSchemeLoop is the scheme list the credential helper step iterates: a
// scheme the preflight accepts but this list omits gets no credential.
var helperSchemeLoop = regexp.MustCompile(`for scheme in ([a-z ]+)`)

// acceptedSchemes is the schemes the preflight accepts for the mango pin,
// sorted so two spellings compare equal.
func acceptedSchemes() []string {
	out := slices.Clone(preflight.Schemes)
	slices.Sort(out)
	return out
}

// helperSchemes is the scheme list the credential helper step iterates, sorted.
func helperSchemes(script string) []string {
	m := helperSchemeLoop.FindStringSubmatch(script)
	if m == nil {
		return nil
	}
	out := strings.Fields(m[1])
	slices.Sort(out)
	return out
}

// step is one entry of the images job's steps: its name, its id, the run
// script, the env a secret reaches it through, the action it uses, and its if
// condition.
type step struct {
	name string
	id   string
	run  string
	env  map[string]string
	uses string
	cond string
}

// loadWorkflow parses the workflow file. A file that is not valid YAML fails
// here: every rule below reads the parsed document, never the text.
func loadWorkflow(t *testing.T) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(workflowPath))
	if err != nil {
		t.Fatalf("reading %s: %v", workflowPath, err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s is not valid YAML: %v", workflowPath, err)
	}
	return doc
}

// object reads v as a decoded YAML mapping.
func object(t *testing.T, v any, what string) map[string]any {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("%s is not a mapping: %T", what, v)
	}
	return m
}

// sequence reads v as a decoded YAML sequence.
func sequence(t *testing.T, v any, what string) []any {
	t.Helper()
	s, ok := v.([]any)
	if !ok {
		t.Fatalf("%s is not a sequence: %T", what, v)
	}
	return s
}

// text reads v as a decoded YAML string.
func text(t *testing.T, v any, what string) string {
	t.Helper()
	s, ok := v.(string)
	if !ok {
		t.Fatalf("%s is not a string: %T", what, v)
	}
	return s
}

// loadJob reads the images job and its steps. A file without that job fails
// here, so every rule below reads steps that exist.
func loadJob(t *testing.T) (map[string]any, []step) {
	t.Helper()
	wf := loadWorkflow(t)
	jobs := object(t, wf["jobs"], "jobs")
	job := object(t, jobs["images"], "the images job")
	var steps []step
	for i, raw := range sequence(t, job["steps"], "the images job's steps") {
		m := object(t, raw, fmt.Sprintf("step %d", i+1))
		s := step{name: fmt.Sprintf("step %d", i+1), env: map[string]string{}}
		if v, ok := m["name"]; ok {
			s.name = text(t, v, fmt.Sprintf("step %d name", i+1))
		}
		if v, ok := m["id"]; ok {
			s.id = text(t, v, s.name+" id")
		}
		if v, ok := m["run"]; ok {
			s.run = text(t, v, s.name+" run")
		}
		if v, ok := m["uses"]; ok {
			s.uses = text(t, v, s.name+" uses")
		}
		if v, ok := m["if"]; ok {
			s.cond = text(t, v, s.name+" if")
		}
		if v, ok := m["env"]; ok {
			for k, val := range object(t, v, s.name+" env") {
				s.env[k] = text(t, val, s.name+" env "+k)
			}
		}
		steps = append(steps, s)
	}
	return job, steps
}

// named is the index of the step called name, or -1.
func named(steps []step, name string) int {
	for i, s := range steps {
		if s.name == name {
			return i
		}
	}
	return -1
}

// containing is the index of the first step whose run holds substr, or -1.
func containing(steps []step, substr string) int {
	for i, s := range steps {
		if strings.Contains(s.run, substr) {
			return i
		}
	}
	return -1
}

// preflightStep is the index of the step that runs the Go preflight, or -1.
func preflightStep(steps []step) int {
	return containing(steps, "go run ./internal/ci/cmd/imagepreflight")
}

// countRuns is how many steps' run scripts hold substr.
func countRuns(steps []step, substr string) int {
	n := 0
	for _, s := range steps {
		if strings.Contains(s.run, substr) {
			n++
		}
	}
	return n
}

// runsDocker reports whether script has a line that invokes docker as a
// command, rather than naming it in a comment.
func runsDocker(script string) bool {
	for _, line := range strings.Split(script, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "docker ") {
			return true
		}
	}
	return false
}

// echoes reports whether script has an echo line that prints the named value.
func echoes(script, name string) bool {
	for _, line := range strings.Split(script, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "echo") && strings.Contains(line, name) {
			return true
		}
	}
	return false
}

// TestImagesWorkflowTriggersOnlyOnGreenRefs holds the trigger rule: the only
// event is a push to a green/<sha12> ref, and the file has no concurrency
// block that a canceled or replaced run could turn into a false failure or a
// missing image.
func TestImagesWorkflowTriggersOnlyOnGreenRefs(t *testing.T) {
	t.Parallel()
	wf := loadWorkflow(t)

	t.Run("the only event is a push to green refs", func(t *testing.T) {
		t.Parallel()
		on := object(t, wf["on"], "on")
		if len(on) != 1 {
			t.Fatalf("on has %d events, want only push: %v", len(on), on)
		}
		trigger, ok := on["push"]
		if !ok {
			t.Fatalf("on = %v, want the key push", on)
		}
		push := object(t, trigger, "push")
		if len(push) != 1 {
			t.Fatalf("push has %d filters, want only branches: %v", len(push), push)
		}
		branches, ok := push["branches"]
		if !ok {
			t.Fatalf("push = %v, want the key branches", push)
		}
		got := sequence(t, branches, "push.branches")
		if len(got) != 1 || text(t, got[0], "push.branches[0]") != "green/**" {
			t.Fatalf("push.branches = %v, want exactly [\"green/**\"]", got)
		}
	})

	t.Run("there is no concurrency block", func(t *testing.T) {
		t.Parallel()
		if v, ok := wf["concurrency"]; ok {
			t.Fatalf("the workflow has a concurrency block (%v); a canceled or replaced run would leave a false failure or no image", v)
		}
	})
}

// TestImagesWorkflowBuildsBothImagesAtOneCommit holds the one-commit rule: the
// preflight reads GITHUB_SHA once and writes the tag, both image builds take
// their revision, tag and commit label from it, and a step after both builds
// reads the label back from each image and fails when either disagrees.
func TestImagesWorkflowBuildsBothImagesAtOneCommit(t *testing.T) {
	t.Parallel()
	_, steps := loadJob(t)

	t.Run("GITHUB_SHA is read once, by the preflight", func(t *testing.T) {
		t.Parallel()
		// The sha and its tag are checked and computed in internal/ci/preflight;
		// TestCheckRefusesAGreenCommitThatIsNotAFullSHA and
		// TestCheckAcceptsTheRunnerAndTheGreenCommit drive both.
		if got := countRuns(steps, "GITHUB_SHA"); got != 1 {
			t.Fatalf("%d run scripts name GITHUB_SHA, want only the preflight", got)
		}
		at := preflightStep(steps)
		if at < 0 {
			t.Fatal("no step runs go run ./internal/ci/cmd/imagepreflight")
		}
		if !strings.Contains(steps[at].run, `-sha "${GITHUB_SHA:-}"`) {
			t.Errorf("the preflight does not pass GITHUB_SHA as the green commit: %s", steps[at].run)
		}
		if got := countRuns(steps, "GITHUB_SHA:0:12"); got != 0 {
			t.Errorf("%d run scripts compute the tag from GITHUB_SHA; the preflight computes it", got)
		}
	})

	t.Run("the preflight runs before anything is fetched or built", func(t *testing.T) {
		t.Parallel()
		at := preflightStep(steps)
		if at < 0 {
			t.Fatal("no step runs the preflight")
		}
		for _, later := range []string{"fetch mango at its pin", "stage the daemon image's build context", "build the daemon image"} {
			if i := named(steps, later); i < 0 || at > i {
				t.Errorf("the preflight is step %d, after %q (step %d)", at, later, i)
			}
		}
	})

	t.Run("the daemon context is built at the one commit", func(t *testing.T) {
		t.Parallel()
		at := containing(steps, "daemon image --apply --context-only")
		if at < 0 {
			t.Fatal("no step runs: go run ./cmd/gt daemon image --apply --context-only")
		}
		for _, want := range []string{
			`--rev "$SHA"`,
			`--gt-src "$GITHUB_WORKSPACE"`,
			`--pins "$GITHUB_WORKSPACE/deploy/daemon/image-pins"`,
		} {
			if !strings.Contains(steps[at].run, want) {
				t.Errorf("the daemon context step does not pass %s", want)
			}
		}
	})

	t.Run("the daemon context carries the known_hosts the Dockerfile copies", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "stage the daemon image's build context")
		if at < 0 {
			t.Fatal("no step is named: stage the daemon image's build context")
		}
		if !strings.Contains(steps[at].run, `cp "$GITHUB_WORKSPACE/deploy/daemon/ssh_known_hosts" "$ctx/ssh_known_hosts"`) {
			t.Error("the daemon context step does not stage ssh_known_hosts, so the Dockerfile's COPY of it fails")
		}
	})

	t.Run("the polecat context is built at the one commit", func(t *testing.T) {
		t.Parallel()
		at := containing(steps, "./internal/polecat/imagectx/cmd/build")
		if at < 0 {
			t.Fatal("no step runs: go run ./internal/polecat/imagectx/cmd/build")
		}
		for _, want := range []string{`-rev "$SHA"`, "-pins deploy/daemon/image-pins"} {
			if !strings.Contains(steps[at].run, want) {
				t.Errorf("the polecat context step does not pass %s", want)
			}
		}
	})

	t.Run("the polecat context labels are held by the tested Go read", func(t *testing.T) {
		t.Parallel()
		// The off-commit, missing and unbuildable label refusals are driven by
		// TestParseRefusesAContextOffTheGreenCommit and
		// TestParseRefusesAnUnusableBDOrGoFloor in internal/ci/ctxlabels.
		at := named(steps, "stage the polecat image's build context")
		if at < 0 {
			t.Fatal("no step is named: stage the polecat image's build context")
		}
		run := steps[at].run
		if !strings.Contains(run, "go run ./internal/ci/cmd/ctxlabels") {
			t.Fatalf("the polecat context step does not run the Go label read: %s", run)
		}
		for _, want := range []string{`-sha "$SHA"`, `-env-out "$GITHUB_ENV"`} {
			if !strings.Contains(run, want) {
				t.Errorf("the label read does not pass %s", want)
			}
		}
		if strings.Contains(run, "sed -n") {
			t.Errorf("the polecat context step parses its labels in shell instead of the tested read: %s", run)
		}
	})

	t.Run("the daemon image is tagged and labeled with the one commit", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "build the daemon image")
		if at < 0 {
			t.Fatal("no step is named: build the daemon image")
		}
		for _, want := range []string{
			`-t "$FORGEJO_HOST/$REGISTRY_OWNER/$DAEMON_IMAGE:$TAG"`,
			`--label org.gastown.gt-commit="$SHA"`,
		} {
			if !strings.Contains(steps[at].run, want) {
				t.Errorf("the daemon image build does not pass %s", want)
			}
		}
	})

	t.Run("the polecat image is tagged and labeled with the one commit", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "build the polecat image")
		if at < 0 {
			t.Fatal("no step is named: build the polecat image")
		}
		for _, want := range []string{
			`-t "$FORGEJO_HOST/$REGISTRY_OWNER/$POLECAT_IMAGE:$TAG"`,
			`--label org.gastown.gt-commit="$SHA"`,
		} {
			if !strings.Contains(steps[at].run, want) {
				t.Errorf("the polecat image build does not pass %s", want)
			}
		}
	})

	t.Run("the label check follows both builds", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "check both images carry the one commit")
		if at < 0 {
			t.Fatal("no step is named: check both images carry the one commit")
		}
		if daemon := named(steps, "build the daemon image"); daemon < 0 || at < daemon {
			t.Errorf("the label check is step %d, before the daemon build (step %d)", at, daemon)
		}
		if polecat := named(steps, "build the polecat image"); polecat < 0 || at < polecat {
			t.Errorf("the label check is step %d, before the polecat build (step %d)", at, polecat)
		}
	})

	t.Run("the label check runs the tested Go check on both images", func(t *testing.T) {
		t.Parallel()
		// The mismatch and unreadable-label refusals are driven against the
		// check itself by TestCheckReportsEveryImageOffTheCommit and
		// TestCheckStopsWhenALabelCannotBeRead in internal/ci/imagepublish.
		at := named(steps, "check both images carry the one commit")
		if at < 0 {
			t.Fatal("no step is named: check both images carry the one commit")
		}
		run := steps[at].run
		if !strings.Contains(run, "go run ./internal/ci/cmd/imagepublish check") {
			t.Fatalf("the label check does not run go run ./internal/ci/cmd/imagepublish check: %s", run)
		}
		for _, want := range []string{
			`-registry "$FORGEJO_HOST"`,
			`-owner "$REGISTRY_OWNER"`,
			`-daemon "$DAEMON_IMAGE"`,
			`-polecat "$POLECAT_IMAGE"`,
			`-tag "$TAG"`,
			`-sha "$SHA"`,
		} {
			if !strings.Contains(run, want) {
				t.Errorf("the label check does not pass %s", want)
			}
		}
		if runsDocker(run) {
			t.Errorf("the label check types its own docker logic instead of the tested check: %s", run)
		}
	})
}

// TestImagesWorkflowKeepsSecretsOffArgv holds the secrets rule: neither token is
// ever an argument or an echo; the registry login reads its token from standard
// input, and the source token reaches git only through a credential helper that
// reads the environment.
func TestImagesWorkflowKeepsSecretsOffArgv(t *testing.T) {
	t.Parallel()
	_, steps := loadJob(t)

	t.Run("no secret expression is inside a run script", func(t *testing.T) {
		t.Parallel()
		for _, s := range steps {
			if strings.Contains(s.run, "secrets.") {
				t.Errorf("step %q holds a secret expression in its run script: %s", s.name, s.run)
			}
		}
	})

	t.Run("the registry token reaches the login on standard input", func(t *testing.T) {
		t.Parallel()
		at := containing(steps, "docker login")
		if at < 0 {
			t.Fatal("no step runs docker login")
		}
		run := steps[at].run
		// The whole login command, so a flag that stops being an argument is
		// not still found as a substring.
		want := `printf '%s' "$REGISTRY_TOKEN" | docker login "$FORGEJO_HOST" --username "$REGISTRY_USER" --password-stdin`
		if !strings.Contains(run, want) {
			t.Errorf("the registry login is not %q", want)
		}
		for _, s := range steps {
			if strings.Contains(s.run, "--password ") || strings.Contains(s.run, "--password=") {
				t.Errorf("step %q passes the registry password on the argument list", s.name)
			}
		}
	})

	t.Run("the registry token appears in no echo and no other script", func(t *testing.T) {
		t.Parallel()
		if got := countRuns(steps, "$REGISTRY_TOKEN"); got != 1 {
			t.Errorf("%d run scripts name $REGISTRY_TOKEN, want only the login", got)
		}
		for _, s := range steps {
			if echoes(s.run, "REGISTRY_TOKEN") {
				t.Errorf("step %q echoes the registry token", s.name)
			}
		}
	})

	t.Run("the source token reaches git only through the credential helper", func(t *testing.T) {
		t.Parallel()
		helper := ""
		for _, s := range steps {
			if !strings.Contains(s.run, "SOURCES_TOKEN") {
				continue
			}
			if !strings.Contains(s.run, "credential.") {
				t.Errorf("step %q names SOURCES_TOKEN outside the credential helper", s.name)
			}
			if helper == "" {
				helper = s.name
			}
		}
		if helper == "" {
			t.Fatal("no step configures a credential helper that reads SOURCES_TOKEN")
		}
		at := named(steps, helper)
		run := steps[at].run
		if !strings.Contains(run, `credential."$scheme://$FORGEJO_HOST".helper`) {
			t.Error("the helper is not scoped to the Forgejo host the pins name")
		}
		if !strings.Contains(run, `"$SOURCES_TOKEN"`) {
			t.Error("the helper does not read the token from the environment")
		}
	})

	t.Run("the credential helper covers every scheme the preflight accepts", func(t *testing.T) {
		t.Parallel()
		// The preflight holds the accepted schemes in internal/ci/preflight,
		// where each is driven; a scheme it accepts and this helper omits
		// sends that fetch with no credential.
		want := acceptedSchemes()
		at := named(steps, "let git read the pinned sources with the read-only token")
		if at < 0 {
			t.Fatal("no step configures the source credential helper")
		}
		got := helperSchemes(steps[at].run)
		if !slices.Equal(got, want) {
			t.Errorf("the helper is configured for %v, but the preflight accepts %v", got, want)
		}
	})

	t.Run("the helper stays out of the runner's global git config", func(t *testing.T) {
		t.Parallel()
		// A credential helper written to the runner's own ~/.gitconfig would
		// outlive the job; GIT_CONFIG_GLOBAL points the write at the job's
		// temporary file instead.
		at := named(steps, "let git read the pinned sources with the read-only token")
		if at < 0 {
			t.Fatal("no step configures the source credential helper")
		}
		run := steps[at].run
		exported := strings.Index(run, `export GIT_CONFIG_GLOBAL=`)
		written := strings.Index(run, `git config --global credential.`)
		if exported < 0 || !strings.Contains(run, `$RUNNER_TEMP/gitconfig`) {
			t.Fatal("the helper step does not point GIT_CONFIG_GLOBAL at a file under $RUNNER_TEMP")
		}
		if written < exported {
			t.Error("the helper is written to the global config before GIT_CONFIG_GLOBAL is set")
		}
	})

	t.Run("only the steps that fetch get the helper's config", func(t *testing.T) {
		t.Parallel()
		// GIT_CONFIG_GLOBAL reaches each fetching step through its own env, not
		// through $GITHUB_ENV, so no step that runs no fetch inherits a
		// credential helper.
		const want = "${{ runner.temp }}/gitconfig"
		fetchers := []string{
			"fetch mango at its pin",
			"stage the daemon image's build context",
			"stage the polecat image's build context",
		}
		for _, name := range fetchers {
			at := named(steps, name)
			if at < 0 {
				t.Fatalf("no step is named: %s", name)
			}
			if got := steps[at].env["GIT_CONFIG_GLOBAL"]; got != want {
				t.Errorf("the step %q gets GIT_CONFIG_GLOBAL=%q, want %q", name, got, want)
			}
		}
		for _, s := range steps {
			if _, ok := s.env["GIT_CONFIG_GLOBAL"]; ok && !slices.Contains(fetchers, s.name) {
				t.Errorf("the step %q gets the credential helper's config but runs no fetch", s.name)
			}
			if strings.Contains(s.run, "GITHUB_ENV") && strings.Contains(s.run, "GIT_CONFIG_GLOBAL") {
				t.Errorf("the step %q hands GIT_CONFIG_GLOBAL to every later step", s.name)
			}
		}
	})

	t.Run("both tokens are passed through env blocks", func(t *testing.T) {
		t.Parallel()
		registry, sources := 0, 0
		for _, s := range steps {
			if _, ok := s.env["REGISTRY_TOKEN"]; ok {
				registry++
			}
			if _, ok := s.env["SOURCES_TOKEN"]; ok {
				sources++
			}
		}
		if registry != 1 {
			t.Errorf("%d steps get REGISTRY_TOKEN through env, want 1", registry)
		}
		if sources < 1 {
			t.Error("no step gets SOURCES_TOKEN through env")
		}
	})
}

// TestImagesWorkflowPrintsTheDigestLines holds the output and preflight rules:
// each push is followed by a registry digest read and the IMAGE line the deploy
// reads, printed only once the digest check passed, and the job starts with a
// bootstrap that asks for go and then runs the Go preflight, which holds the
// runner's tools, jq's version, the mango pin and the green commit.
func TestImagesWorkflowPrintsTheDigestLines(t *testing.T) {
	t.Parallel()
	_, steps := loadJob(t)

	t.Run("the first step is the preflight", func(t *testing.T) {
		t.Parallel()
		if len(steps) == 0 {
			t.Fatal("the images job runs no step")
		}
		if steps[0].name != "preflight" {
			t.Fatalf("the first step is %q, want preflight", steps[0].name)
		}
		if steps[0].uses != "" {
			t.Fatalf("the preflight is an action (%s), want a run step", steps[0].uses)
		}
	})

	t.Run("the bootstrap asks for go and stops", func(t *testing.T) {
		t.Parallel()
		// This is the one shell check the job makes: nothing else can run
		// before the checkout brings the Go source the preflight is written in.
		run := steps[named(steps, "preflight")].run
		for _, want := range []string{"command -v go", "exit 1"} {
			if !strings.Contains(run, want) {
				t.Errorf("the bootstrap does not contain %q", want)
			}
		}
	})

	t.Run("the preflight proper runs the Go check on the runner, the pin and the commit", func(t *testing.T) {
		t.Parallel()
		// The tools, jq's version, the mango pin and the commit are held by
		// internal/ci/preflight, whose refusals
		// TestCheckRefusesAMissingTool, TestCheckRefusesAnotherJQ,
		// TestCheckRefusesAMangoURLTheTokenMayNotReach and
		// TestCheckRefusesAGreenCommitThatIsNotAFullSHA drive.
		at := preflightStep(steps)
		if at < 0 {
			t.Fatal("no step runs go run ./internal/ci/cmd/imagepreflight")
		}
		for _, want := range []string{
			`-mango-url "$MANGO_URL"`,
			`-mango-commit "$MANGO_COMMIT"`,
			`-host "$FORGEJO_HOST"`,
			`-env-out "$GITHUB_ENV"`,
		} {
			if !strings.Contains(steps[at].run, want) {
				t.Errorf("the preflight does not pass %s", want)
			}
		}
		if runsDocker(steps[at].run) || strings.Contains(steps[at].run, "go run ./cmd/gt") {
			t.Errorf("the preflight builds something instead of checking: %s", steps[at].run)
		}
	})

	t.Run("the push step follows both builds and runs the tested Go publish", func(t *testing.T) {
		t.Parallel()
		// The digest read, its refusals and the IMAGE line's order are driven
		// against the publish itself by TestPushPrintsTheRegistryDigestForEachImage,
		// TestPushRefusesWhenTheRegistryListsNoSingleDigest and
		// TestPushStopsAtTheFirstFailedCommand in internal/ci/imagepublish.
		at := named(steps, "push both images and publish their digests")
		if at < 0 {
			t.Fatal("no step is named: push both images and publish their digests")
		}
		if daemon := named(steps, "build the daemon image"); daemon < 0 || at < daemon {
			t.Errorf("the push is step %d, before the daemon build (step %d)", at, daemon)
		}
		if polecat := named(steps, "build the polecat image"); polecat < 0 || at < polecat {
			t.Errorf("the push is step %d, before the polecat build (step %d)", at, polecat)
		}
		run := steps[at].run
		if !strings.Contains(run, "go run ./internal/ci/cmd/imagepublish push") {
			t.Fatalf("the push step does not run go run ./internal/ci/cmd/imagepublish push: %s", run)
		}
		for _, want := range []string{
			`-registry "$FORGEJO_HOST"`,
			`-owner "$REGISTRY_OWNER"`,
			`-daemon "$DAEMON_IMAGE"`,
			`-polecat "$POLECAT_IMAGE"`,
			`-tag "$TAG"`,
			`-sha "$SHA"`,
		} {
			if !strings.Contains(run, want) {
				t.Errorf("the push step does not pass %s", want)
			}
		}
		if runsDocker(run) {
			t.Errorf("the push step types its own docker logic instead of the tested publish: %s", run)
		}
	})
}

// TestImagesWorkflowNamesTheSharedImageConstants holds the one-spelling rule:
// the workflow's image names are internal/imagevalues' constants, and its
// Forgejo host and registry owner are the registry gt update images reads from
// the town's own rig settings. The workflow's env must equal that source, so a
// build that pushes to one registry and a command that reads another fails
// here instead of in production.
func TestImagesWorkflowNamesTheSharedImageConstants(t *testing.T) {
	t.Parallel()
	wf := loadWorkflow(t)
	env := object(t, wf["env"], "env")
	reg := townImagesRegistry(t)
	for _, want := range []struct{ key, value string }{
		{"DAEMON_IMAGE", imagevalues.DaemonImage},
		{"POLECAT_IMAGE", imagevalues.PolecatImage},
		{"FORGEJO_HOST", reg.Host},
		{"REGISTRY_OWNER", reg.Owner},
	} {
		if got := text(t, env[want.key], "env "+want.key); got != want.value {
			t.Errorf("the workflow's %s is %q, but the town's rig settings name %q", want.key, got, want.value)
		}
	}
}

// townImagesRegistry is the registry the fixture town's rig settings name: the
// source gt update images reads. The fixture is the same one
// TestUpdateImagesWorkflowNamesTheTownsRegistry resolves through the command in
// internal/cmd, so the two tests hold the workflow to the registry the command
// actually reads.
func townImagesRegistry(t *testing.T) imagevalues.Registry {
	t.Helper()
	const path = "../../deploy/ci/testdata/images-town/gastown/config.json"
	raw, err := os.ReadFile(filepath.FromSlash(path))
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	var rigConfig struct {
		MergeQueue struct {
			Forgejo struct {
				RemoteURL string `json:"remote_url"`
			} `json:"forgejo"`
		} `json:"merge_queue"`
	}
	if err := json.Unmarshal(raw, &rigConfig); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	remote := rigConfig.MergeQueue.Forgejo.RemoteURL
	if remote == "" {
		t.Fatalf("%s names no merge_queue.forgejo.remote_url", path)
	}
	owner, _, err := land.RepoFromRemoteURL(remote)
	if err != nil {
		t.Fatalf("reading the owner from %q: %v", remote, err)
	}
	u, err := url.Parse(remote)
	if err != nil || u.Host == "" {
		t.Fatalf("the town's rig settings name no registry host: %q", remote)
	}
	return imagevalues.Registry{Scheme: u.Scheme, Host: u.Host, Owner: owner}
}

// TestImagesWorkflowRunsOneJobOnCIBase holds the runner rule: one job named
// images on the ci-base label.
func TestImagesWorkflowRunsOneJobOnCIBase(t *testing.T) {
	t.Parallel()
	wf := loadWorkflow(t)
	jobs := object(t, wf["jobs"], "jobs")
	if len(jobs) != 1 {
		t.Fatalf("the workflow has %d jobs, want only images: %v", len(jobs), jobs)
	}
	job := object(t, jobs["images"], "the images job")
	if got := text(t, job["runs-on"], "the images job's runs-on"); got != "ci-base" {
		t.Fatalf("the images job runs on %q, want ci-base", got)
	}
}

// liveWorkflowPath is the live copy the operator writes from workflowPath. It is
// absent until the operator copies the text, so a checkout may lack it.
const liveWorkflowPath = "../../.forgejo/workflows/images.yml"

// checkLiveCopy holds the live copy equal to the reviewed text. An absent live
// copy passes, since the text may land before the operator's copy. Any other
// read failure, and any byte difference, is an error that names the paths.
func checkLiveCopy(livePath, reviewedPath string) error {
	live, err := os.ReadFile(livePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("reading the live copy %s: %w", livePath, err)
	}
	reviewed, err := os.ReadFile(reviewedPath)
	if err != nil {
		return fmt.Errorf("reading the reviewed text %s: %w", reviewedPath, err)
	}
	if !bytes.Equal(live, reviewed) {
		return fmt.Errorf("%s differs from the reviewed text %s: copy the reviewed text over it byte for byte", livePath, reviewedPath)
	}
	return nil
}

// TestImagesWorkflowLiveCopyMatchesReviewedText holds the two copies equal in
// the checkout: once the operator has written the live copy, it is the reviewed
// text byte for byte.
func TestImagesWorkflowLiveCopyMatchesReviewedText(t *testing.T) {
	t.Parallel()
	if err := checkLiveCopy(filepath.FromSlash(liveWorkflowPath), filepath.FromSlash(workflowPath)); err != nil {
		t.Fatal(err)
	}
}

// TestImagesWorkflowLiveCopyBranches drives each branch of the live-copy check
// against files under t.TempDir(), never the checkout.
func TestImagesWorkflowLiveCopyBranches(t *testing.T) {
	t.Parallel()
	reviewed := []byte("name: images\n")
	cases := []struct {
		name    string
		live    []byte // nil: no live file is written
		liveDir bool   // the live path is a directory, which cannot be read as a file
		wantErr bool
	}{
		{name: "an absent live copy passes", live: nil},
		{name: "an identical live copy passes", live: reviewed},
		{name: "a differing live copy is refused", live: []byte("name: other\n"), wantErr: true},
		{name: "an unreadable live copy is refused, not read as absent", liveDir: true, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			reviewedPath := filepath.Join(dir, "images.workflow.yml")
			livePath := filepath.Join(dir, "images.yml")
			if err := os.WriteFile(reviewedPath, reviewed, 0o644); err != nil {
				t.Fatal(err)
			}
			switch {
			case tc.liveDir:
				if err := os.Mkdir(livePath, 0o755); err != nil {
					t.Fatal(err)
				}
			case tc.live != nil:
				if err := os.WriteFile(livePath, tc.live, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			err := checkLiveCopy(livePath, reviewedPath)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkLiveCopy = %v, want an error: %v", err, tc.wantErr)
			}
			if err != nil && !strings.Contains(err.Error(), livePath) {
				t.Errorf("the error %q does not name the live copy %s", err, livePath)
			}
		})
	}

	t.Run("a differing live copy names both paths", func(t *testing.T) {
		t.Parallel()
		dir := t.TempDir()
		reviewedPath := filepath.Join(dir, "images.workflow.yml")
		livePath := filepath.Join(dir, "images.yml")
		if err := os.WriteFile(reviewedPath, reviewed, 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(livePath, []byte("name: other\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		err := checkLiveCopy(livePath, reviewedPath)
		if err == nil {
			t.Fatal("checkLiveCopy passed a live copy that differs from the reviewed text")
		}
		for _, p := range []string{livePath, reviewedPath} {
			if !strings.Contains(err.Error(), p) {
				t.Errorf("the error %q does not name %s", err, p)
			}
		}
	})
}

// TestImagesWorkflowLogsOutOfTheRegistryAlways holds the logout rule: the last
// step logs out of the registry host with if: always(), after the login, so the
// login's token never outlives a job that failed. The logout runs only when the
// login succeeded, and a logout that fails fails the step.
func TestImagesWorkflowLogsOutOfTheRegistryAlways(t *testing.T) {
	t.Parallel()
	_, steps := loadJob(t)
	at := containing(steps, `docker logout "$FORGEJO_HOST"`)
	if at < 0 {
		t.Fatal("no step runs docker logout on the registry host")
	}

	t.Run("the logout runs after the login, as the last step", func(t *testing.T) {
		t.Parallel()
		login := containing(steps, "docker login")
		if login < 0 {
			t.Fatal("no step runs docker login")
		}
		if at <= login {
			t.Errorf("the logout is step %d, before or with the login (step %d)", at, login)
		}
		if at != len(steps)-1 {
			t.Errorf("the logout is step %d of %d, want the final step", at, len(steps))
		}
	})

	t.Run("the logout runs always, but only after a successful login", func(t *testing.T) {
		t.Parallel()
		const want = "always() && steps.login.outcome == 'success'"
		if got := steps[at].cond; got != want {
			t.Errorf("the logout step's if is %q, want %q", got, want)
		}
	})

	t.Run("the condition reads the login step by the id the login step has", func(t *testing.T) {
		t.Parallel()
		login := named(steps, "log in to the registry")
		if login < 0 {
			t.Fatal("no step is named log in to the registry")
		}
		if got := steps[login].id; got != "login" {
			t.Errorf("the login step's id is %q, want login: the logout condition reads steps.login", got)
		}
	})

	t.Run("a failed logout fails the step, it is not ignored", func(t *testing.T) {
		t.Parallel()
		if got := strings.TrimSpace(steps[at].run); got != `docker logout "$FORGEJO_HOST"` {
			t.Errorf("the logout script is %q, want the bare logout with no fallback", got)
		}
	})
}
