//go:build integration

package testsel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/steveyegge/gastown/internal/jev"
	"github.com/steveyegge/gastown/internal/land"
)

// git runs git in dir with a fixed identity and returns trimmed stdout.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "commit.gpgsign=false", "-c", "user.name=test", "-c", "user.email=test@example.test"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// commitFile writes body to path under dir and commits it, returning the
// commit.
func commitFile(t *testing.T, dir, path, body string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "change "+path)
	return git(t, dir, "rev-parse", "HEAD")
}

// fixtureRepo is the fixture module committed as base, then one change to
// lib's body committed as head.
func fixtureRepo(t *testing.T) (dir, base, head string) {
	t.Helper()
	dir = t.TempDir()
	git(t, dir, "init", "-q", "-b", "main")
	commitFile(t, dir, "go.mod", "module example.test/fixture\n\ngo 1.22\n")
	commitFile(t, dir, "lib/lib.go", "package lib\n\nfunc Value() int { return 1 }\n")
	commitFile(t, dir, "app/app.go", "package app\n\nimport \"example.test/fixture/lib\"\n\nfunc Run() int { return lib.Value() }\n")
	commitFile(t, dir, "deep/deep.go", "package deep\n\nimport \"example.test/fixture/app\"\n\nfunc Go() int { return app.Run() }\n")
	base = commitFile(t, dir, "util/util.go", "package util\n\nfunc Name() string { return \"util\" }\n")
	head = commitFile(t, dir, "lib/lib.go", "package lib\n\nfunc Value() int { return 2 }\n")
	return dir, base, head
}

// TestIntegrationJevTestselPassWritesOneLineForAFixtureDiff runs the real git
// and go list over a fixture module, asks a real Jev client at an httptest
// server, and checks the one line the landing writes. The state Jev reads
// carries the stat and the package list, never the diff text.
func TestIntegrationJevTestselPassWritesOneLineForAFixtureDiff(t *testing.T) {
	dir, base, head := fixtureRepo(t)

	var mu sync.Mutex
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		mu.Unlock()
		var req struct {
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.Unmarshal(body, &req)
		noul := map[string]float64{"deep": 0.4, "util": 0.9}
		answers := map[string]map[string]any{}
		for id := range req.Questions {
			answers[id] = map[string]any{"type": "noul", "noul": noul[id]}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "jev-test", "answers": answers,
			"usage": map[string]int{"input_tokens": 10, "output_tokens": 2},
		})
	}))
	t.Cleanup(srv.Close)
	client, err := jev.NewClient(
		jev.WithBaseURL(srv.URL), jev.WithKey("test-key"),
		jev.WithLedger(filepath.Join(t.TempDir(), "usage.jsonl")), jev.WithCaller(Caller),
	)
	if err != nil {
		t.Fatal(err)
	}
	pass := newPass(t, client, treeState{})
	pass.tree = nil // the production reader: git and go list in dir

	run := pass.Start(context.Background(), Landing{Bead: "gt-fixture", Rig: "gastown", Base: base, Head: head, Dir: dir})
	err = run.Finish([]land.PackageResult{
		{Package: "example.test/fixture/lib", Passed: true},
		{Package: "example.test/fixture/deep", Passed: false},
		{Package: "example.test/fixture/util", Passed: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	mu.Lock()
	calls := len(bodies)
	mu.Unlock()
	if calls != 1 {
		t.Fatalf("Jev was called %d times, want one call per landing", calls)
	}
	state := bodies[0]
	if !strings.Contains(state, "1 files changed, 1 insertions(+), 1 deletions(-)") {
		t.Errorf("state lacks the diff stat: %s", state)
	}
	if strings.Contains(state, "return 2") {
		t.Errorf("state carries diff text: %s", state)
	}

	lines := readLines(t, pass.LogPath)
	if len(lines) != 1 {
		t.Fatalf("log has %d lines, want one", len(lines))
	}
	got := lines[0]
	if want := []string{"lib"}; !reflect.DeepEqual(got.Changed, want) {
		t.Errorf("changed = %v, want %v", got.Changed, want)
	}
	if want := []string{"app", "lib"}; !reflect.DeepEqual(got.AlwaysRun, want) {
		t.Errorf("always_run = %v, want %v", got.AlwaysRun, want)
	}
	if want := []string{"deep", "util"}; !reflect.DeepEqual(got.Asked, want) {
		t.Errorf("asked = %v, want %v", got.Asked, want)
	}
	if got.Noul["deep"] != 0.4 || got.Noul["util"] != 0.9 || got.Model != "jev-test" {
		t.Errorf("noul = %v, model = %q", got.Noul, got.Model)
	}
	wantSkip := map[string][]string{"0.2": {}, "0.3": {}, "0.5": {"deep"}}
	if !reflect.DeepEqual(got.WouldSkip, wantSkip) {
		t.Errorf("would_skip = %v, want %v", got.WouldSkip, wantSkip)
	}
	wantMiss := map[string][]string{"0.2": {}, "0.3": {}, "0.5": {"deep"}}
	if !reflect.DeepEqual(got.Miss, wantMiss) {
		t.Errorf("miss = %v, want %v", got.Miss, wantMiss)
	}
	if got.GatePackages != 3 || got.Gate["deep"] || !got.Gate["lib"] {
		t.Errorf("gate = %v over %d packages", got.Gate, got.GatePackages)
	}
}

// TestIntegrationJevTestselListPackagesCountsATestOnlyImport runs the real go
// list over a module whose app package imports lib only from its test file. The
// battery must read that import, or a change to lib would leave app unrun.
func TestIntegrationJevTestselListPackagesCountsATestOnlyImport(t *testing.T) {
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":          "module example.test/fixture\n\ngo 1.22\n",
		"lib/lib.go":      "package lib\n\nfunc Value() int { return 1 }\n",
		"app/app.go":      "package app\n",
		"app/app_test.go": "package app\n\nimport (\n\t\"testing\"\n\n\t\"example.test/fixture/lib\"\n)\n\nfunc TestValue(t *testing.T) { _ = lib.Value() }\n",
	}
	for name, body := range files {
		path := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	pkgs, err := listPackages(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	plan := Decide(pkgs, []string{"lib"}, false)
	if want := []string{"app", "lib"}; !reflect.DeepEqual(plan.AlwaysRun, want) {
		t.Errorf("always_run = %v, want %v: app imports lib from its test file", plan.AlwaysRun, want)
	}
}
