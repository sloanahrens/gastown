package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// workflowPath is the images workflow text, relative to this package: the file
// the operator copies to .forgejo/workflows/images.yml.
const workflowPath = "../../deploy/ci/images.workflow.yml"

// step is one entry of the images job's steps: its name, the run script, the
// env a secret reaches it through, and the action it uses.
type step struct {
	name string
	run  string
	env  map[string]string
	uses string
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

// loadJob reads the images job and its steps. A file without the single job the
// workflow runs fails here, so every rule below reads steps that exist.
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
		if v, ok := m["run"]; ok {
			s.run = text(t, v, s.name+" run")
		}
		if v, ok := m["uses"]; ok {
			s.uses = text(t, v, s.name+" uses")
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
// block that a cancelled or replaced run could turn into a false failure or a
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
			t.Fatalf("the workflow has a concurrency block (%v); a cancelled or replaced run would leave a false failure or no image", v)
		}
	})
}

// TestImagesWorkflowBuildsBothImagesAtOneCommit holds the one-commit rule: the
// job reads GITHUB_SHA once, both image builds take their revision, tag and
// commit label from it, and a step after both builds reads the label back from
// each image and fails when either disagrees.
func TestImagesWorkflowBuildsBothImagesAtOneCommit(t *testing.T) {
	t.Parallel()
	_, steps := loadJob(t)

	t.Run("GITHUB_SHA is read once and the sha12 tag computed once", func(t *testing.T) {
		t.Parallel()
		if got := countRuns(steps, `SHA=$GITHUB_SHA`); got != 1 {
			t.Fatalf("%d run scripts compute SHA=$GITHUB_SHA, want 1", got)
		}
		if got := countRuns(steps, `TAG=${GITHUB_SHA:0:12}`); got != 1 {
			t.Fatalf("%d run scripts compute TAG=${GITHUB_SHA:0:12}, want 1", got)
		}
	})

	t.Run("the commit is refused unless it is a full sha", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "resolve the one commit")
		if at < 0 {
			t.Fatal("no step is named: resolve the one commit")
		}
		for _, want := range []string{`[ "${#GITHUB_SHA}" -ne 40 ]`, `case "$GITHUB_SHA"`, "*[!0-9a-f]*"} {
			if !strings.Contains(steps[at].run, want) {
				t.Errorf("the resolve step does not contain %q", want)
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

	t.Run("the daemon image is tagged and labelled with the one commit", func(t *testing.T) {
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

	t.Run("the polecat image is tagged and labelled with the one commit", func(t *testing.T) {
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

	t.Run("the label check reads both images' commit labels", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "check both images carry the one commit")
		if at < 0 {
			t.Fatal("no step is named: check both images carry the one commit")
		}
		for _, want := range []string{"$DAEMON_IMAGE", "$POLECAT_IMAGE", `org.gastown.gt-commit`} {
			if !strings.Contains(steps[at].run, want) {
				t.Errorf("the label check does not name %s", want)
			}
		}
	})

	t.Run("the label check fails on a difference from the one commit", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "check both images carry the one commit")
		if at < 0 {
			t.Fatal("no step is named: check both images carry the one commit")
		}
		run := steps[at].run
		if !strings.Contains(run, `"$got" != "$SHA"`) {
			t.Error("the label check does not compare the read label with $SHA")
		}
		if !strings.Contains(run, `exit "$status"`) || !strings.Contains(run, "status=1") {
			t.Error("the label check does not fail the job when a label differs")
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
		if !strings.Contains(run, "--password-stdin") {
			t.Error("the registry login does not read the token from standard input")
		}
		if !strings.Contains(run, `printf '%s' "$REGISTRY_TOKEN" | docker login`) {
			t.Error("the registry token is not piped into the login")
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
		if !strings.Contains(run, `credential."http://$FORGEJO_HOST".helper`) {
			t.Error("the helper is not scoped to the Forgejo host the pins name")
		}
		if !strings.Contains(run, `"$SOURCES_TOKEN"`) {
			t.Error("the helper does not read the token from the environment")
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
// reads, printed only once the digest check passed, and the first step is a
// preflight that names what the runner is missing and refuses another jq.
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

	t.Run("the preflight names what is missing and stops", func(t *testing.T) {
		t.Parallel()
		run := steps[named(steps, "preflight")].run
		for _, want := range []string{"for tool in jq go docker git", "command -v", "missing:", "exit 1"} {
			if !strings.Contains(run, want) {
				t.Errorf("the preflight does not contain %q", want)
			}
		}
	})

	t.Run("the preflight refuses another jq", func(t *testing.T) {
		t.Parallel()
		run := steps[named(steps, "preflight")].run
		for _, want := range []string{"jq --version", `[ "$version" != "jq-1.7.1" ]`, "exit 1"} {
			if !strings.Contains(run, want) {
				t.Errorf("the preflight does not contain %q", want)
			}
		}
	})

	t.Run("the preflight refuses a malformed mango pin", func(t *testing.T) {
		t.Parallel()
		run := steps[named(steps, "preflight")].run
		for _, want := range []string{
			`case "$MANGO_URL"`,
			"http://*|https://*)",
			`[ "${#MANGO_COMMIT}" -ne 40 ]`,
			`case "$MANGO_COMMIT"`,
		} {
			if !strings.Contains(run, want) {
				t.Errorf("the preflight does not contain %q", want)
			}
		}
	})

	t.Run("each push is followed by a registry digest read and its IMAGE line", func(t *testing.T) {
		t.Parallel()
		at := named(steps, "push both images and publish their digests")
		if at < 0 {
			t.Fatal("no step is named: push both images and publish their digests")
		}
		run := steps[at].run
		push := strings.Index(run, "docker push")
		pull := strings.Index(run, "docker pull")
		digest := strings.Index(run, "RepoDigests")
		if push < 0 || pull < 0 || digest < 0 {
			t.Fatalf("the push step does not push and read the registry digest back: %s", run)
		}
		if !(push < pull && pull < digest) {
			t.Errorf("the digest read is not after the push (push %d, pull %d, digest %d)", push, pull, digest)
		}
		if !strings.Contains(run, "for kind in daemon polecat") {
			t.Fatalf("the push step does not visit both images: %s", run)
		}
		printed := strings.Index(run, `echo "IMAGE $kind $TAG $digests"`)
		if printed < 0 {
			t.Fatalf("the push step does not print the IMAGE line: %s", run)
		}
		if printed < digest {
			t.Error("the IMAGE line is printed before the registry digest was read")
		}
	})

	t.Run("the registry read must answer exactly one sha256 digest", func(t *testing.T) {
		t.Parallel()
		run := steps[named(steps, "push both images and publish their digests")].run
		for _, want := range []string{
			`grep -Ec '^sha256:[0-9a-f]{64}$'`,
			`[ "$count" -ne 1 ] || [ "$lines" -ne 1 ]`,
			`wc -l`,
		} {
			if !strings.Contains(run, want) {
				t.Errorf("the push step does not contain %q", want)
			}
		}
	})

	t.Run("the IMAGE line is printed only after the digest check", func(t *testing.T) {
		t.Parallel()
		run := steps[named(steps, "push both images and publish their digests")].run
		check := strings.Index(run, `'^sha256:[0-9a-f]{64}$'`)
		printed := strings.Index(run, `echo "IMAGE`)
		if check < 0 {
			t.Fatal("the push step does not check the digest's shape")
		}
		if printed < check {
			t.Error("the IMAGE line is printed before the digest's shape is checked")
		}
	})
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
