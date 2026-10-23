package session

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/steveyegge/gastown/internal/tmux"
)

func TestResolveTmuxIsTheDefaultAndSharesTheHostClient(t *testing.T) {
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
	prev := NewOpenShellBackend
	t.Cleanup(func() { NewOpenShellBackend = prev })
	NewOpenShellBackend = nil
	_, err := Resolve("deepseek-flash-sandbox", BackendOpenShell, Profile{Name: "p", Image: "img", Policy: "pol"}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "not linked") {
		t.Fatalf("err = %v, want not linked", err)
	}
	NewOpenShellBackend = func(string, Profile) (Backend, error) { return NewTmuxBackend(nil), nil }
	_, err = Resolve("deepseek-flash-sandbox", BackendOpenShell, Profile{Name: "p"}, t.TempDir(), nil)
	if err == nil || !strings.Contains(err.Error(), "needs image and policy") {
		t.Fatalf("err = %v, want needs image and policy", err)
	}
	if _, err := Resolve("s", "firecracker", Profile{}, t.TempDir(), nil); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestSandboxRegistryRoundTrip(t *testing.T) {
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
