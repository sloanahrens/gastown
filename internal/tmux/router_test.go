package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// recordExec records every call and answers with reply.
func recordExec(calls *[][]string, reply string) execFunc {
	return func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		*calls = append(*calls, append([]string{name}, args...))
		return []byte(reply), nil, nil
	}
}

func TestSessionOfArgs(t *testing.T) {
	cases := map[string][]string{
		"gt-r-n":  {"has-session", "-t", "gt-r-n"},
		"gt-r-n2": {"send-keys", "-t", "gt-r-n2:0.0", "hi", "Enter"},
		"s3":      {"new-session", "-d", "-s", "s3", "-c", "/x"},
		"":        {"list-sessions", "-F", "#{session_name}"},
		"%":       {"capture-pane", "-p", "-t", "%12"},
		"gt-r-n4": {"has-session", "-t", "=gt-r-n4"},
		"gt-r-n5": {"set-hook", "-t", "=gt-r-n5:", "-u", "pane-died"},
	}
	for want, args := range cases {
		got := sessionOfArgs(args)
		if want == "%" {
			want = ""
		}
		if got != want {
			t.Errorf("sessionOfArgs(%v) = %q, want %q", args, got, want)
		}
	}
}

func TestRouterSendsSessionCommandsToTheRoutedClient(t *testing.T) {
	var hostCalls, routedCalls [][]string
	host := newTmuxForTest("gt-test", recordExec(&hostCalls, ""), nil)
	routed := newTmuxForTest("gt", recordExec(&routedCalls, ""), nil)
	SetSessionRouter(func(session string) *Tmux {
		if session == "gt-rig-box" {
			return routed
		}
		return nil
	})
	t.Cleanup(func() { SetSessionRouter(nil) })

	if _, err := host.HasSession("gt-rig-box"); err != nil {
		t.Fatal(err)
	}
	if _, err := host.HasSession("gt-rig-local"); err != nil {
		t.Fatal(err)
	}
	if len(routedCalls) != 1 || !strings.Contains(strings.Join(routedCalls[0], " "), "-L gt has-session -t =gt-rig-box") {
		t.Fatalf("routed calls = %v", routedCalls)
	}
	if len(hostCalls) != 1 || !strings.Contains(strings.Join(hostCalls[0], " "), "-L gt-test has-session -t =gt-rig-local") {
		t.Fatalf("host calls = %v", hostCalls)
	}
	if !routed.noRoute {
		t.Fatal("a routed client must be marked noRoute so it never routes again")
	}
}

func TestRouterKeepsProcessProbesInsideTheRoutedClient(t *testing.T) {
	var hostCalls, routedCalls [][]string
	host := newTmuxForTest("gt-test", recordExec(&hostCalls, ""), nil)
	// The routed exec answers tmux queries with a pid and a process name, so
	// IsAgentAliveChecked reaches its ps probe; that ps must run here too.
	routed := newTmuxForTest("gt", func(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
		routedCalls = append(routedCalls, append([]string{name}, args...))
		switch name {
		case "tmux":
			return []byte("4242"), nil, nil
		case "ps":
			return []byte("4242 node\n"), nil, nil
		}
		return nil, nil, errors.New("unexpected " + name)
	}, nil)
	SetSessionRouter(func(string) *Tmux { return routed })
	t.Cleanup(func() { SetSessionRouter(nil) })

	_, _ = host.IsAgentAliveChecked("gt-rig-box")
	if len(hostCalls) != 0 {
		t.Fatalf("the host client ran %v; every ps for a routed session must run on the routed client", hostCalls)
	}
	var sawPs bool
	for _, c := range routedCalls {
		if c[0] == "ps" {
			sawPs = true
		}
	}
	if !sawPs {
		t.Fatalf("routed client ran no ps: %v", routedCalls)
	}
}

func TestListSessionsIncludesExtraSessions(t *testing.T) {
	var calls [][]string
	host := newTmuxForTest("gt-test", recordExec(&calls, "gt-rig-local\n"), nil)
	SetExtraSessions(func() []string { return []string{"gt-rig-box", "gt-rig-local"} })
	t.Cleanup(func() { SetExtraSessions(nil) })
	got, err := host.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "gt-rig-local,gt-rig-box" {
		t.Fatalf("ListSessions = %v", got)
	}
}
