package dashboard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1:8787": true, "localhost:8787": true, "[::1]:8787": true, "localhost": true,
		"evil.example.com": false, "evil.example.com:8787": false, "10.0.0.5:8787": false, "": false,
	} {
		if got := loopbackHost(host); got != want {
			t.Errorf("loopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
	if IsLoopbackAddr("0.0.0.0:80") || !IsLoopbackAddr("127.0.0.1:80") {
		t.Error("IsLoopbackAddr wrong")
	}
}

func TestGuardRefusesRebindingAndWrites(t *testing.T) {
	h := NewHub(Config{}).Handler()
	cases := []struct {
		method, host string
		want         int
	}{
		{"GET", "127.0.0.1:8787", 200},
		{"GET", "evil.example.com:8787", 403},
		{"POST", "127.0.0.1:8787", 405},
		{"DELETE", "localhost:8787", 405},
	}
	for _, c := range cases {
		req := httptest.NewRequest(c.method, "/", nil)
		req.Host = c.host
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Errorf("%s Host=%s: %d, want %d", c.method, c.host, rec.Code, c.want)
		}
	}
	req := httptest.NewRequest("GET", "/", nil)
	req.Host = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") {
		t.Errorf("missing CSP: %q", csp)
	}
}

// With no page open the hub must call no reader at all.
func TestIdleHubPollsNothing(t *testing.T) {
	var calls atomic.Int32
	h := NewHub(Config{
		Feed:         func() []Entry { calls.Add(1); return nil },
		Health:       func() Health { calls.Add(1); return Health{} },
		FeedEvery:    5 * time.Millisecond,
		HealthEvery:  5 * time.Millisecond,
		SummaryEvery: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)
	time.Sleep(80 * time.Millisecond)
	if n := calls.Load(); n != 0 {
		t.Fatalf("idle hub called its readers %d times", n)
	}
}

func TestHubPushesFeedAndStateToAPage(t *testing.T) {
	var fed atomic.Bool
	now := time.Date(2026, 10, 3, 16, 0, 0, 0, time.UTC)
	h := NewHub(Config{
		Feed: func() []Entry {
			if fed.Swap(true) {
				return nil
			}
			return []Entry{
				{At: now, Rig: "gastown", Kind: "daemon", Text: "gt-abc slung to agate (gastown)", Class: "dispatch"},
				{At: now, Rig: "gastown", Kind: "daemon", Text: "stages: lint 18s, gate 5m1s, om 2m", Class: "plain"},
			}
		},
		Health:      func() Health { return Health{Line: "GREEN ok", Verdict: "green"} },
		Machine:     func() (Machine, error) { return Machine{Load1: 7.5, At: now}, nil },
		Summary:     func() Summary { return Summary{Seats: []SeatRef{{Rig: "gastown", Polecat: "agate", Bead: "gt-abc"}}} },
		Title:       func(id string) string { return "the title of " + id },
		Now:         func() time.Time { return now.Add(10 * time.Minute) },
		FeedEvery:   10 * time.Millisecond,
		HealthEvery: 10 * time.Millisecond, MachineEvery: 10 * time.Millisecond, SummaryEvery: 10 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.Run(ctx)

	sub, _ := h.Subscribe()
	defer h.Unsubscribe(sub)

	deadline := time.After(3 * time.Second)
	for {
		st := h.State()
		if st.Health.Verdict == "green" && len(st.Seats) == 1 && len(st.Gates) == 1 && st.Machine.Load1 == 7.5 {
			seat := st.Seats[0]
			if seat.Title != "the title of gt-abc" {
				t.Errorf("seat title = %q", seat.Title)
			}
			if seat.Elapsed != 600 {
				t.Errorf("seat elapsed = %d, want 600 (dispatch line to now)", seat.Elapsed)
			}
			if g := st.Gates[0]; g.Secs != 301 || (g.Load != nil && *g.Load != 7.5) {
				t.Errorf("gate = %+v", g)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatalf("state never filled: %+v", st)
		case <-time.After(10 * time.Millisecond):
		}
	}
	// A page that connects late gets the backlog.
	late, initial := h.Subscribe()
	defer h.Unsubscribe(late)
	var sawBacklog bool
	for _, f := range initial {
		if strings.HasPrefix(string(f), "event: backlog") {
			sawBacklog = true
		}
	}
	if !sawBacklog {
		t.Error("late page got no backlog")
	}
}

// A page that stops reading is dropped, not waited on.
func TestSlowPageIsDropped(t *testing.T) {
	h := NewHub(Config{})
	sub, _ := h.Subscribe()
	h.mu.Lock()
	for i := 0; i < 100; i++ {
		h.broadcastLocked(frame("entry", Entry{Seq: int64(i)}))
	}
	_, still := h.subs[sub]
	h.mu.Unlock()
	if still {
		t.Fatal("slow page still subscribed")
	}
	if _, ok := <-sub.C; ok {
		// drain the buffered frames; the channel must end closed
		for range sub.C {
		}
	}
}

func TestParseLoadavgAndTop(t *testing.T) {
	var m Machine
	if err := parseLoadavg("{ 6.71 16.49 28.11 }\n", &m); err != nil || m.Load1 != 6.71 || m.Load15 != 28.11 {
		t.Fatalf("parseLoadavg: %v %+v", err, m)
	}
	if err := parseLoadavg("garbage", &m); err == nil {
		t.Error("garbage load accepted")
	}
	ps := " 95.0 /usr/local/go/bin/go\n 40.2 /usr/bin/go\n 30.0 /usr/bin/dolt\n  0.1 launchd\n"
	top := parseTop(ps, 5)
	if len(top) != 2 || top[0].Name != "go" || top[0].CPU < 135 || top[1].Name != "dolt" {
		t.Fatalf("parseTop = %+v", top)
	}
}

func TestStreamServesInitialState(t *testing.T) {
	h := NewHub(Config{Health: func() Health { return Health{Verdict: "red"} }})
	srv := httptest.NewServer(h.Handler())
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/stream", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	buf := make([]byte, 256)
	n, _ := resp.Body.Read(buf)
	if !strings.HasPrefix(string(buf[:n]), "event: state") {
		t.Fatalf("first frame %q", buf[:n])
	}
}

func TestSlungRegexReadsBothDispatchLineShapes(t *testing.T) {
	for text, want := range map[string]string{
		"gt-m36as slung to agate (gastown)":                                   "gt-m36as",
		"spec_dispatch: dispatched: gt-4k3fj.14: slung to gastown/agate on x": "gt-4k3fj.14",
	} {
		m := slungRe.FindStringSubmatch(text)
		if m == nil || m[1] != want {
			t.Errorf("%q -> %v, want %q", text, m, want)
		}
	}
}
