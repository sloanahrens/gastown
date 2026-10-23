package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/liveness"
	"github.com/steveyegge/gastown/internal/tmux"
)

func TestResolveTmuxIsTheDefaultAndSharesTheHostClient(t *testing.T) {
	t.Parallel()
	host := tmux.NewTmuxWithSocket("gt-test")
	for _, name := range []string{"", BackendTmux} {
		b, err := Resolve("deepseek-flash", name, Profile{}, t.TempDir(), host)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", name, err)
		}
		if b.Name() != BackendTmux || b.Tmux() != host {
			t.Fatalf("Resolve(%q) = %s with client %p, want tmux sharing %p", name, b.Name(), b.Tmux(), host)
		}
		p, err := b.Prepare(context.Background(), SpawnSpec{HostWorkDir: "/work/tree"})
		if err != nil || p.WorkDir != "/work/tree" {
			t.Fatalf("tmux Prepare = %+v, %v", p, err)
		}
	}
}

func TestResolveOpenShellNeedsTheImplementationAndAProfile(t *testing.T) {
	t.Parallel()
	_, err := ResolveWith(nil, "deepseek-flash-sandbox", BackendOpenShell, Profile{Name: "p", Image: "img", Policy: "pol"}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Fatalf("err = %v, want not linked", err)
	}
	linked := func(string, Profile) (Backend, error) { return NewTmuxBackend(nil), nil }
	_, err = ResolveWith(linked, "deepseek-flash-sandbox", BackendOpenShell, Profile{Name: "p"}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "needs image and policy") {
		t.Fatalf("err = %v, want needs image and policy", err)
	}
	if _, err := ResolveWith(linked, "s", "firecracker", Profile{}, t.TempDir(), nil); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestSandboxRegistryRoundTrip(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if _, ok, err := ReadSandboxRecord(town, "gt-rig-box"); ok || err != nil {
		t.Fatalf("empty registry: ok=%v err=%v", ok, err)
	}
	rec := SandboxRecord{Session: "gt-rig-box", Rig: "rig", Polecat: "box", Gateway: "openshell", Profile: "polecat-default", Image: "img:dev", CreatedAt: time.Unix(1700000000, 0).UTC()}
	if err := WriteSandboxRecord(town, rec); err != nil {
		t.Fatal(err)
	}
	got, ok, err := ReadSandboxRecord(town, "gt-rig-box")
	if err != nil || !ok || got != rec {
		t.Fatalf("read = %+v ok=%v err=%v", got, ok, err)
	}
	if err := WriteSandboxRecord(town, SandboxRecord{Session: "gt-rig-ace"}); err != nil {
		t.Fatal(err)
	}
	if s := ListSandboxSessions(town); strings.Join(s, ",") != "gt-rig-ace,gt-rig-box" {
		t.Fatalf("list = %v", s)
	}
	if err := RemoveSandboxRecord(town, "gt-rig-box"); err != nil {
		t.Fatal(err)
	}
	if err := RemoveSandboxRecord(town, "gt-rig-box"); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if err := WriteSandboxRecord(town, SandboxRecord{Session: "../escape"}); err == nil {
		t.Fatal("a session name with a path separator was accepted")
	}
}

//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestSandboxRoutingRoutesRegisteredSessionsOnly(t *testing.T) {
	town := t.TempDir()
	ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { ResetSandboxRoutingForTest("") })
	if err := WriteSandboxRecord(town, SandboxRecord{Session: "gt-rig-box", Gateway: "gw"}); err != nil {
		t.Fatal(err)
	}
	host := tmux.NewTmuxWithSocket("gt-test")
	if c := RoutedTmuxFor("gt-rig-local", host); c != host {
		t.Fatal("an unregistered session was routed")
	}
	c := RoutedTmuxFor("gt-rig-box", host)
	if c == host {
		t.Fatal("a registered session was not routed")
	}
	want := "openshell -g gw sandbox exec -n gt-rig-box --no-tty --no-login-shell --"
	if got := strings.Join(c.ExecPrefix(), " "); got != want {
		t.Fatalf("prefix = %q, want %q", got, want)
	}
	if again := RoutedTmuxFor("gt-rig-box", host); again != c {
		t.Fatal("routed client is not cached")
	}
	if s := routing.sessions(); strings.Join(s, ",") != "gt-rig-box" {
		t.Fatalf("extra sessions = %v", s)
	}
}

func TestTmuxBackendWithoutAClientIsInert(t *testing.T) {
	t.Parallel()
	b := NewTmuxBackend(nil)
	if ok, err := b.SessionExists("gt-rig-local"); err != nil || ok {
		t.Fatalf("SessionExists = %v, %v", ok, err)
	}
	if err := b.Stop("gt-rig-local"); err != nil {
		t.Fatal(err)
	}
	if err := b.Delete("gt-rig-local"); err != nil {
		t.Fatal(err)
	}
}

//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestCleanupSandboxDeletesThroughTheHookAndForgetsTheRecord(t *testing.T) {
	town := t.TempDir()
	ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { ResetSandboxRoutingForTest("") })
	var deleted []string
	deleter := func(_ string, rec SandboxRecord) error { deleted = append(deleted, rec.Session); return nil }

	if err := CleanupSandboxWith(deleter, town, "gt-rig-host"); err != nil || len(deleted) != 0 {
		t.Fatalf("host session: err=%v deleted=%v", err, deleted)
	}
	if err := WriteSandboxRecord(town, SandboxRecord{Session: "gt-rig-box"}); err != nil {
		t.Fatal(err)
	}
	_ = RoutedTmuxFor("gt-rig-box", nil) // populate the client cache
	if err := CleanupSandboxWith(deleter, town, "gt-rig-box"); err != nil {
		t.Fatal(err)
	}
	still, err := IsSandboxed(town, "gt-rig-box")
	if err != nil || strings.Join(deleted, ",") != "gt-rig-box" || still {
		t.Fatalf("deleted=%v still registered=%v err=%v", deleted, still, err)
	}
	if c := RoutedTmuxFor("gt-rig-box", nil); c != nil {
		t.Fatal("routed client survived cleanup")
	}
}

//testpolicy:allow parallel — points the process-wide sandbox routing at a temp town, which another test must not see mid-run
func TestRoutingFailsClosedWhenTheRegistryIsUnreadable(t *testing.T) {
	town := t.TempDir()
	ResetSandboxRoutingForTest(town)
	t.Cleanup(func() { ResetSandboxRoutingForTest("") })
	// A registry path that is a file, not a directory: listing it fails.
	if err := os.MkdirAll(filepath.Dir(SandboxRegistryDir(town)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SandboxRegistryDir(town), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	host := tmux.NewTmuxWithSocket("gt-test")
	c := RoutedTmuxFor("gt-rig-box", host)
	if c == host {
		t.Fatal("an unreadable registry fell back to the host server")
	}
	if _, err := c.HasSession("gt-rig-box"); err == nil || !strings.Contains(err.Error(), "sandbox registry") {
		t.Fatalf("the unavailable client must fail with the registry error, got %v", err)
	}
}

func TestAssessWhereItLivesFailsClosed(t *testing.T) {
	t.Parallel()
	town := t.TempDir()
	if err := os.MkdirAll(filepath.Dir(SandboxRegistryDir(town)), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(SandboxRegistryDir(town), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	// With the registry unreadable the verdict is Unknown, never a host
	// verdict (ReadSandboxRecord fails with ENOTDIR on the record path).
	res := assessWhereItLivesWith(nil, town, nil, liveness.Input{Session: "gt-rig-box"})
	if res.Verdict != liveness.Unknown || res.Err == nil {
		t.Fatalf("unreadable registry: %+v", res)
	}
	town2 := t.TempDir()
	if err := WriteSandboxRecord(town2, SandboxRecord{Session: "gt-rig-box"}); err != nil {
		t.Fatal(err)
	}
	res = assessWhereItLivesWith(nil, town2, nil, liveness.Input{Session: "gt-rig-box"})
	if res.Verdict != liveness.Unknown || !strings.Contains(res.Reason, "no sandbox liveness") {
		t.Fatalf("sandboxed session without the hook: %+v", res)
	}
	var called bool
	res = assessWhereItLivesWith(func(string, SandboxRecord, liveness.Input) liveness.Result {
		called = true
		return liveness.Result{Verdict: liveness.Alive}
	}, town2, nil, liveness.Input{Session: "gt-rig-box"})
	if !called || res.Verdict != liveness.Alive {
		t.Fatalf("sandboxed session with the hook: called=%v %+v", called, res)
	}
}

func TestSandboxRecordNamesAreStrict(t *testing.T) {
	t.Parallel()
	for _, bad := range []string{"", ".", "..", ".hidden", "a/b", "a\\b", "-leading", "x y"} {
		if validSessionName(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{"gt-gastown-box", "gt-rig-a.b_c"} {
		if !validSessionName(good) {
			t.Errorf("%q rejected", good)
		}
	}
}
