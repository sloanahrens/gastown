package openshellprov

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestProfileIDTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{"plain", "id: forgejo-polecat\n", "forgejo-polecat"},
		{"double quoted", "id: \"forgejo-polecat\"\n", "forgejo-polecat"},
		{"single quoted", "id: 'forgejo-polecat'\n", "forgejo-polecat"},
		{"trailing comment", "id: forgejo-polecat # the polecat profile\n", "forgejo-polecat"},
		{"trailing space", "id: forgejo-polecat   \n", "forgejo-polecat"},
		{"nested id ignored", "credentials:\n  - id: nested\n", ""},
		{"nested id before the top-level one", "credentials:\n  - id: nested\nid: forgejo-polecat\n", "forgejo-polecat"},
		{"no id", "name: forgejo-polecat\n", ""},
		{"CRLF plain", "id: forgejo-polecat\r\n", "forgejo-polecat"},
		{"CRLF double quoted", "id: \"forgejo-polecat\"\r\n", "forgejo-polecat"},
		{"tab after the key", "id:\tforgejo-polecat\n", "forgejo-polecat"},
		{"tab before the comment", "id: forgejo-polecat\t# the polecat profile\n", "forgejo-polecat"},
		{"unterminated double quote", "id: \"forgejo-polecat\n", ""},
		{"unterminated single quote", "id: 'forgejo-polecat\n", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ProfileID([]byte(tc.yaml)); got != tc.want {
				t.Errorf("ProfileID(%q) = %q, want %q", tc.yaml, got, tc.want)
			}
		})
	}
}

func TestGtDefaultGatewayIsNamedOpenshell(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{}
	if err := (Gt{Run: fake, Root: "/src/gt"}).CheckEndpoint(); err != nil {
		t.Fatalf("CheckEndpoint: %v", err)
	}
	want := "gt sandbox endpoint --gateway openshell --root /src/gt"
	if len(fake.calls) != 1 || fake.calls[0] != want {
		t.Errorf("calls = %q, want [%q]", fake.calls, want)
	}
}

func TestGtArgs(t *testing.T) {
	t.Parallel()
	const base = "gt sandbox endpoint --gateway k8s --root /src/gt"
	tests := []struct {
		name string
		run  func(g Gt) error
		want string
	}{
		{"check", func(g Gt) error { return g.CheckEndpoint() }, base},
		{"render without export", func(g Gt) error { return g.Render("p.yaml", &bytes.Buffer{}, "") }, base + " --render p.yaml"},
		{"render with export", func(g Gt) error { return g.Render("p.yaml", &bytes.Buffer{}, "e.yaml") }, base + " --render p.yaml --from-export e.yaml"},
		{"stale check", func(g Gt) error { _, err := g.StaleReason("forgejo-polecat", "e.yaml"); return err }, base + " --check e.yaml"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{stdout: []byte("bound\n")}
			if err := tc.run(Gt{Run: fake, Gateway: "k8s", Root: "/src/gt"}); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if len(fake.calls) != 1 || fake.calls[0] != tc.want {
				t.Errorf("calls = %q, want [%q]", fake.calls, tc.want)
			}
		})
	}
}

func TestCheckEndpointRefusalNamesTheGateway(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{err: errors.New("exit status 1")}
	err := (Gt{Run: fake, Gateway: "k8s"}).CheckEndpoint()
	if err == nil {
		t.Fatal("CheckEndpoint after a refused call = nil error, want the refusal")
	}
	if want := "gt sandbox endpoint --gateway k8s failed, so the forgejo-polecat profile cannot be rendered"; !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestRenderRefusalWritesNothing(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	fake := &fakeRunner{stdout: []byte("partial"), err: errors.New("exit status 1")}
	err := (Gt{Run: fake}).Render("p.yaml", &out, "")
	if err == nil {
		t.Fatal("Render after a refused call = nil error, want the refusal")
	}
	if !strings.Contains(err.Error(), "gt sandbox endpoint --render p.yaml failed") {
		t.Errorf("error = %q, want it to name the --render call", err)
	}
	if out.Len() != 0 {
		t.Errorf("out = %q after a refused render, want nothing written", out.String())
	}
}

func TestRenderWriteFailureIsAnError(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{stdout: []byte("rendered")}
	if err := (Gt{Run: fake}).Render("p.yaml", failingWriter{}, ""); err == nil {
		t.Fatal("Render into a failing writer = nil error, want the write failure")
	}
}

func TestStaleReasonUnrecognisedAnswerNamesTheVerdict(t *testing.T) {
	t.Parallel()
	fake := &fakeRunner{stdout: []byte("maybe\n")}
	_, err := (Gt{Run: fake}).StaleReason("forgejo-polecat", "e.yaml")
	if err == nil {
		t.Fatal("StaleReason(maybe) = nil error, want an error")
	}
	want := "neither bound nor stale for forgejo-polecat: maybe"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error = %q, want it to contain %q", err, want)
	}
}

func TestStaleReasonVerdicts(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		stdout     string
		err        error
		wantReason string
		wantErr    bool
	}{
		{"bound", "bound\n", nil, "", false},
		{"stale names its reason", "stale: host differs\n", nil, "host differs", false},
		{"stale with no reason is unrecognised", "stale:\n", nil, "", true},
		{"unrecognised answer", "maybe\n", nil, "", true},
		{"failing command", "", errors.New("exit status 1"), "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fake := &fakeRunner{stdout: []byte(tc.stdout), err: tc.err}
			reason, err := (Gt{Run: fake}).StaleReason("forgejo-polecat", "e.yaml")
			if tc.wantErr {
				if err == nil {
					t.Fatalf("StaleReason(%q) = %q, nil error; want an error", tc.stdout, reason)
				}
				if !strings.Contains(err.Error(), "forgejo-polecat") {
					t.Errorf("error = %q, want it to name the profile", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("StaleReason(%q): %v", tc.stdout, err)
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}
