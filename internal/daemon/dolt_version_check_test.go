package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jonboulle/clockwork"

	"github.com/steveyegge/gastown/internal/attention"
)

// doltRegistryURL is the tag listing the fake registry answers for. Nothing
// dials it: the check's client carries the fake as its transport.
const doltRegistryURL = "https://registry.invalid/v2/dolthub/dolt-sql-server/tags"

// doltFakeDocker is the docker seam: it answers with image, or err, and keeps
// every argv it was asked for.
type doltFakeDocker struct {
	image string
	err   error
	argv  [][]string
}

func (f *doltFakeDocker) inspect(_ context.Context, argv []string) ([]byte, error) {
	f.argv = append(f.argv, argv)
	if f.err != nil {
		return nil, f.err
	}
	return []byte(f.image + "\n"), nil
}

// doltFakeServer is the Dolt server seam: it answers version, or err, and
// counts every read.
type doltFakeServer struct {
	version string
	err     error
	calls   atomic.Int32
}

func (f *doltFakeServer) read(context.Context) (string, error) {
	f.calls.Add(1)
	if f.err != nil {
		return "", f.err
	}
	return f.version, nil
}

// doltFakeRegistry answers the tag listing in process. answer gives the status
// and body of each page, pages numbered from 1. hang makes a request wait for
// its own context to end, which is how a client timeout is seen. It counts
// every request.
type doltFakeRegistry struct {
	answer   func(page int) (int, string)
	hang     bool
	requests atomic.Int32
}

func newDoltFakeRegistry(answer func(page int) (int, string)) *doltFakeRegistry {
	return &doltFakeRegistry{answer: answer}
}

// RoundTrip is the fake transport.
func (f *doltFakeRegistry) RoundTrip(req *http.Request) (*http.Response, error) {
	f.requests.Add(1)
	if f.hang {
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	page, err := strconv.Atoi(req.URL.Query().Get("page"))
	if err != nil {
		page = 1
	}
	status, body := f.answer(page)
	return &http.Response{
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode: status,
		Header:     http.Header{},
		Body:       io.NopCloser(strings.NewReader(body)),
		Request:    req,
	}, nil
}

// client is a registry client whose every request goes to f.
func (f *doltFakeRegistry) client(timeout time.Duration) *http.Client {
	return &http.Client{Transport: f, Timeout: timeout}
}

// doltTagPage is one registry answer: a page of tags, with a next link unless
// last.
func doltTagPage(last bool, tags ...string) (int, string) {
	type result struct {
		Name string `json:"name"`
	}
	page := struct {
		Next    string   `json:"next"`
		Results []result `json:"results"`
	}{}
	if !last {
		page.Next = "more"
	}
	for _, t := range tags {
		page.Results = append(page.Results, result{Name: t})
	}
	body, err := json.Marshal(page)
	if err != nil {
		panic(err)
	}
	return http.StatusOK, string(body)
}

// newDoltImageCheckFor builds a check that reads image from a fake docker and
// the tags from reg, for the container "dolt-town".
func newDoltImageCheckFor(image string, reg *doltFakeRegistry) (*doltImageCheck, *doltFakeDocker) {
	docker := &doltFakeDocker{image: image}
	return &doltImageCheck{
		container: func(string) (string, error) { return "dolt-town", nil },
		inspect:   docker.inspect,
		tagsURL:   doltRegistryURL,
		client:    reg.client(0),
	}, docker
}

// doltLogs collects what a run logs.
type doltLogs []string

func (l *doltLogs) logf(format string, args ...any) {
	*l = append(*l, fmt.Sprintf(format, args...))
}

var doltCheckStart = time.Date(2026, 11, 7, 6, 0, 0, 0, time.UTC)

func TestDoltVersionCheckRaisesOneItemNamingBoth(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) {
		return doltTagPage(true, "2.7.2", "2.7.1", "2.8.0-rc1", "latest", "sha-1a2b3c")
	})
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2@sha256:38d5e900", reg)
	server := &doltFakeServer{version: "2.3.2"}
	check.serverVersion = server.read
	var logs doltLogs
	check.run(context.Background(), "/town", doltCheckStart, logs.logf)

	if server.calls.Load() != 0 {
		t.Errorf("the Dolt server was asked %d times, want 0: docker answered", server.calls.Load())
	}
	got := check.observed()
	if len(got) != 1 {
		t.Fatalf("observed = %d items, want 1: %+v", len(got), got)
	}
	it := got[0]
	if it.Kind != attention.KindDoltImageBehind || it.Severity != attention.SeverityLow {
		t.Errorf("item kind/severity = %s/%s, want %s/%s", it.Kind, it.Severity, attention.KindDoltImageBehind, attention.SeverityLow)
	}
	for _, want := range []string{"2.3.2", "2.7.2"} {
		if !strings.Contains(it.Summary, want) {
			t.Errorf("summary %q does not name %s", it.Summary, want)
		}
	}
	wantArgv := "docker inspect -f {{.Config.Image}} dolt-town"
	if len(docker.argv) != 1 || strings.Join(docker.argv[0], " ") != wantArgv {
		t.Errorf("docker argv = %v, want one call %q", docker.argv, wantArgv)
	}
	if reg.requests.Load() != 1 {
		t.Errorf("registry requests = %d, want 1", reg.requests.Load())
	}
	if len(logs) != 0 {
		t.Errorf("logs = %v, want none for a healthy check", logs)
	}
}

func TestDoltVersionCheckClearsWhenMatched(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) {
		return doltTagPage(true, "2.7.2")
	})
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	var logs doltLogs
	clock := clockwork.NewFakeClockAt(doltCheckStart)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if len(check.observed()) != 1 {
		t.Fatalf("setup: running 2.3.2 against 2.7.2 raised no item")
	}

	docker.image = "dolthub/dolt-sql-server:2.7.2"
	clock.Advance(time.Hour)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if got := check.observed(); len(got) != 0 {
		t.Errorf("after the image matched the newest release, observed = %+v, want none", got)
	}
}

func TestDoltVersionCheckBehindRule(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name            string
		running, newest string
		want            bool
	}{
		{"same release clears", "2.7.2", "2.7.2", false},
		{"one patch behind raises nothing", "2.7.1", "2.7.2", false},
		{"two patches behind raises", "2.7.0", "2.7.2", true},
		{"earlier minor raises", "2.6.9", "2.7.2", true},
		{"far behind raises", "2.3.2", "2.7.2", true},
		{"running ahead of the registry is not behind", "2.8.0", "2.7.2", false},
		{"numeric not lexical: 2.9 is behind 2.10", "2.9.0", "2.10.0", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			running, ok := parseDoltVersion(tc.running)
			if !ok {
				t.Fatalf("running %q is not a plain release", tc.running)
			}
			newest, ok := parseDoltVersion(tc.newest)
			if !ok {
				t.Fatalf("newest %q is not a plain release", tc.newest)
			}
			if got := doltImageBehind(running, newest); got != tc.want {
				t.Errorf("doltImageBehind(%s, %s) = %v, want %v", tc.running, tc.newest, got, tc.want)
			}
		})
	}
}

func TestDoltVersionCheckIgnoresNonReleaseTags(t *testing.T) {
	t.Parallel()
	newest, err := newestDoltRelease([]string{"latest", "2.8.0-rc1", "sha-1a2b3c", "2.7.2", "2.7", "v2.9.0", "2.10.0"})
	if err != nil {
		t.Fatalf("newestDoltRelease: %v", err)
	}
	if newest.tag != "2.10.0" {
		t.Errorf("newest = %s, want 2.10.0", newest.tag)
	}
}

func TestDoltVersionCheckRegistryFailureRaisesNothing(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name    string
		tagsURL string
		timeout time.Duration
		hang    bool
		ctx     context.Context
		answer  func(int) (int, string)
	}{
		{
			name:    "registry address does not parse",
			tagsURL: "http://[::1",
			answer:  func(int) (int, string) { return doltTagPage(true, "2.7.2") },
		},
		{
			name:   "registry answers an error status",
			answer: func(int) (int, string) { return http.StatusInternalServerError, "" },
		},
		{
			name:   "registry body is not JSON",
			answer: func(int) (int, string) { return http.StatusOK, "not json" },
		},
		{
			name:   "registry lists no plain release",
			answer: func(int) (int, string) { return doltTagPage(true, "latest", "2.8.0-rc1", "sha-1a2b3c") },
		},
		{
			name:   "registry listing never ends",
			answer: func(int) (int, string) { return doltTagPage(false, "2.9.9") },
		},
		{
			name:    "registry request times out",
			timeout: time.Nanosecond,
			hang:    true,
			answer:  func(int) (int, string) { return doltTagPage(true, "2.7.2") },
		},
		{
			name:   "registry request is cancelled",
			ctx:    cancelled,
			answer: func(int) (int, string) { return doltTagPage(true, "2.7.2") },
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newDoltFakeRegistry(tc.answer)
			reg.hang = tc.hang
			check, _ := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
			check.client = reg.client(tc.timeout)
			if tc.tagsURL != "" {
				check.tagsURL = tc.tagsURL
			}
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			var logs doltLogs
			check.run(ctx, "/town", doltCheckStart, logs.logf)

			if got := check.observed(); len(got) != 0 {
				t.Errorf("observed = %+v, want none: an unreadable registry is not behind", got)
			}
			if len(logs) != 1 || !strings.Contains(logs[0], "dolt image check") {
				t.Errorf("logs = %v, want one dolt image check line", logs)
			}
		})
	}
}

func TestDoltVersionCheckFailedReadKeepsHeldItem(t *testing.T) {
	t.Parallel()
	healthy := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	broken := newDoltFakeRegistry(func(int) (int, string) { return http.StatusBadGateway, "" })
	check, _ := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", healthy)
	var logs doltLogs
	clock := clockwork.NewFakeClockAt(doltCheckStart)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	held := check.observed()
	if len(held) != 1 {
		t.Fatalf("setup: running 2.3.2 against 2.7.2 raised %d items, want 1", len(held))
	}

	check.client = broken.client(0)
	clock.Advance(time.Hour)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if got := check.observed(); len(got) != 1 || got[0] != held[0] {
		t.Errorf("after a failed read, observed = %+v, want the held item %+v unchanged", got, held)
	}
	if len(logs) != 1 {
		t.Errorf("logs = %v, want the failed hourly run logged once", logs)
	}
}

func TestDoltImageCheckUnreadableContainerRaisesNothing(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	check.container = func(string) (string, error) {
		return "", errors.New("dolt.external_container is malformed")
	}
	server := &doltFakeServer{version: "2.7.2"}
	check.serverVersion = server.read
	var logs doltLogs
	check.run(context.Background(), "/town", doltCheckStart, logs.logf)

	if got := check.observed(); len(got) != 0 {
		t.Errorf("observed = %+v, want none", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "dolt.external_container") {
		t.Errorf("logs = %v, want one line naming dolt.external_container", logs)
	}
	if len(docker.argv) != 0 || server.calls.Load() != 0 || reg.requests.Load() != 0 {
		t.Errorf("a container setting that does not parse asked docker %v, the server %d times and the registry %d times, want none",
			docker.argv, server.calls.Load(), reg.requests.Load())
	}
}

func TestDoltImageCheckBothReadsFailRaisesNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		container func(string) (string, error)
		image     string
		dockerErr error
		noServer  bool
	}{
		{
			name:      "docker inspect fails",
			image:     "dolthub/dolt-sql-server:2.3.2",
			dockerErr: errors.New("exit status 1"),
		},
		{
			name:  "container name starts with a dash",
			image: "dolthub/dolt-sql-server:2.3.2",
			container: func(string) (string, error) {
				return "-f", nil
			},
		},
		{
			name:  "running image has no tag",
			image: "dolthub/dolt-sql-server",
		},
		{
			name:  "running image is not a release",
			image: "dolthub/dolt-sql-server:latest",
		},
		{
			name:      "the server read is not wired",
			image:     "dolthub/dolt-sql-server:2.3.2",
			dockerErr: errors.New("exit status 1"),
			noServer:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
			check, docker := newDoltImageCheckFor(tc.image, reg)
			docker.err = tc.dockerErr
			if tc.container != nil {
				check.container = tc.container
			}
			server := &doltFakeServer{err: errors.New("dial tcp: connection refused")}
			if !tc.noServer {
				check.serverVersion = server.read
			}
			var logs doltLogs
			check.run(context.Background(), "/town", doltCheckStart, logs.logf)

			if got := check.observed(); len(got) != 0 {
				t.Errorf("observed = %+v, want none", got)
			}
			if len(logs) != 2 {
				t.Fatalf("logs = %v, want one line per failed read", logs)
			}
			if !strings.Contains(logs[0], "docker") {
				t.Errorf("first log line %q does not name docker", logs[0])
			}
			if !strings.Contains(logs[1], "Dolt server") {
				t.Errorf("second log line %q does not name the Dolt server", logs[1])
			}
			wantServer := int32(1)
			if tc.noServer {
				wantServer = 0
			}
			if got := server.calls.Load(); got != wantServer {
				t.Errorf("the Dolt server was asked %d times, want %d", got, wantServer)
			}
			if reg.requests.Load() != 0 {
				t.Errorf("registry was asked %d times after both reads failed, want 0", reg.requests.Load())
			}
		})
	}
}

func TestDoltImageCheckServerAnswersBehind(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	docker.err = errors.New("exit status 1")
	server := &doltFakeServer{version: "2.6.0"}
	check.serverVersion = server.read
	var logs doltLogs
	check.run(context.Background(), "/town", doltCheckStart, logs.logf)

	got := check.observed()
	if len(got) != 1 {
		t.Fatalf("observed = %d items, want 1: %+v", len(got), got)
	}
	for _, want := range []string{"2.6.0", "2.7.2"} {
		if !strings.Contains(got[0].Summary, want) {
			t.Errorf("summary %q does not name %s", got[0].Summary, want)
		}
	}
	if got := server.calls.Load(); got != 1 {
		t.Errorf("the Dolt server was asked %d times, want 1", got)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "docker") {
		t.Errorf("logs = %v, want one line naming the failed docker read", logs)
	}
}

func TestDoltImageCheckServerAnswerClears(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	clock := clockwork.NewFakeClockAt(doltCheckStart)
	var logs doltLogs
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if len(check.observed()) != 1 {
		t.Fatalf("setup: docker 2.3.2 against 2.7.2 raised no item")
	}

	// Docker can no longer see the container; the server says it runs the
	// newest release.
	docker.err = errors.New("exit status 1")
	server := &doltFakeServer{version: "2.7.2"}
	check.serverVersion = server.read
	clock.Advance(time.Hour)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)

	if got := check.observed(); len(got) != 0 {
		t.Errorf("after the server answered the newest release, observed = %+v, want none", got)
	}
	if got := server.calls.Load(); got != 1 {
		t.Errorf("the Dolt server was asked %d times, want 1", got)
	}
}

func TestDoltImageCheckBothReadsFailKeepsHeldItem(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	clock := clockwork.NewFakeClockAt(doltCheckStart)
	var logs doltLogs
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	held := check.observed()
	if len(held) != 1 {
		t.Fatalf("setup: docker 2.3.2 against 2.7.2 raised %d items, want 1", len(held))
	}

	docker.err = errors.New("exit status 1")
	check.serverVersion = (&doltFakeServer{err: errors.New("dial tcp: connection refused")}).read
	clock.Advance(time.Hour)
	logs = nil
	check.run(context.Background(), "/town", clock.Now(), logs.logf)

	if got := check.observed(); len(got) != 1 || got[0] != held[0] {
		t.Errorf("after both reads failed, observed = %+v, want the held item %+v unchanged", got, held)
	}
	if len(logs) != 2 {
		t.Fatalf("logs = %v, want one line per failed read", logs)
	}
	if !strings.Contains(logs[0], "docker") || !strings.Contains(logs[1], "Dolt server") {
		t.Errorf("logs = %v, want one line naming docker and one the Dolt server", logs)
	}
}

func TestDoltImageCheckServerReportsNonVersionKeepsItem(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	clock := clockwork.NewFakeClockAt(doltCheckStart)
	var logs doltLogs
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	held := check.observed()
	if len(held) != 1 {
		t.Fatalf("setup: docker 2.3.2 against 2.7.2 raised %d items, want 1", len(held))
	}

	docker.err = errors.New("exit status 1")
	check.serverVersion = (&doltFakeServer{version: "3.0"}).read
	clock.Advance(time.Hour)
	logs = nil
	check.run(context.Background(), "/town", clock.Now(), logs.logf)

	if got := check.observed(); len(got) != 1 || got[0] != held[0] {
		t.Errorf("after the server answered a non-release, observed = %+v, want the held item %+v unchanged", got, held)
	}
	if len(logs) != 2 || !strings.Contains(logs[1], "not a release") {
		t.Errorf("logs = %v, want a docker failure and a server line naming the non-release", logs)
	}
}

func TestDoltImageCheckServerNotAskedWhenDockerAnswers(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, _ := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	server := &doltFakeServer{err: errors.New("must not be asked")}
	check.serverVersion = server.read
	var logs doltLogs
	check.run(context.Background(), "/town", doltCheckStart, logs.logf)

	if got := server.calls.Load(); got != 0 {
		t.Errorf("the Dolt server was asked %d times, want 0: docker answered", got)
	}
	if len(logs) != 0 {
		t.Errorf("logs = %v, want none", logs)
	}
}

func TestDoltImageCheckReadsPrefixedAndSuffixedTags(t *testing.T) {
	t.Parallel()
	for _, image := range []string{
		"dolthub/dolt-sql-server:v2.3.2",
		"dolthub/dolt-sql-server:2.3.2-slim",
	} {
		t.Run(image, func(t *testing.T) {
			t.Parallel()
			reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
			check, _ := newDoltImageCheckFor(image, reg)
			var logs doltLogs
			check.run(context.Background(), "/town", doltCheckStart, logs.logf)

			got := check.observed()
			if len(got) != 1 {
				t.Fatalf("observed = %d items, want 1: %+v", len(got), got)
			}
			if !strings.Contains(got[0].Summary, "2.3.2") || !strings.Contains(got[0].Summary, "2.7.2") {
				t.Errorf("summary %q does not name 2.3.2 and 2.7.2", got[0].Summary)
			}
		})
	}
}

func TestDoltImageCheckVersionNormalisation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"2.7.2", "2.7.2", true},
		{"v2.7.2", "2.7.2", true},
		{"V2.7.2", "2.7.2", true},
		{"2.7.2-slim", "2.7.2", true},
		{"2.7.2+build.7", "2.7.2", true},
		{"latest", "", false},
		{"sha-1a2b3c", "", false},
		{"2.7", "", false},
		{"2.7.2.1", "", false},
		{"2.7.2-", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := normalizeDoltVersion(tc.in)
		if ok != tc.ok {
			t.Errorf("normalizeDoltVersion(%q) ok = %v, want %v", tc.in, ok, tc.ok)
			continue
		}
		if !ok {
			continue
		}
		core := fmt.Sprintf("%d.%d.%d", got.major, got.minor, got.patch)
		if core != tc.want {
			t.Errorf("normalizeDoltVersion(%q) = %s, want %s", tc.in, core, tc.want)
		}
		if got.tag != tc.in {
			t.Errorf("normalizeDoltVersion(%q).tag = %q, want the input", tc.in, got.tag)
		}
	}
}

func TestDoltVersionCheckHostProcessTownClearsItem(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, docker := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	var logs doltLogs
	clock := clockwork.NewFakeClockAt(doltCheckStart)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if len(check.observed()) != 1 {
		t.Fatalf("setup: no item raised")
	}

	check.container = func(string) (string, error) { return "", nil }
	clock.Advance(time.Hour)
	before := len(docker.argv)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if got := check.observed(); len(got) != 0 {
		t.Errorf("a town with no Dolt container observed %+v, want none", got)
	}
	if len(docker.argv) != before {
		t.Errorf("a host-process town asked docker: %v", docker.argv[before:])
	}
}

func TestDoltVersionCheckHourlySpacing(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.2") })
	check, _ := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	var logs doltLogs
	clock := clockwork.NewFakeClockAt(doltCheckStart)

	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if got := reg.requests.Load(); got != 1 {
		t.Fatalf("first run: registry requests = %d, want 1", got)
	}
	clock.Advance(59 * time.Minute)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if got := reg.requests.Load(); got != 1 {
		t.Errorf("a run inside the hour: registry requests = %d, want still 1", got)
	}
	clock.Advance(time.Minute)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if got := reg.requests.Load(); got != 2 {
		t.Errorf("a run an hour after the first: registry requests = %d, want 2", got)
	}
}

func TestDoltVersionCheckTagParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref  string
		want string
		ok   bool
	}{
		{"dolthub/dolt-sql-server:2.3.2@sha256:38d5e900", "2.3.2", true},
		{"dolthub/dolt-sql-server:2.3.2", "2.3.2", true},
		{"localhost:5000/dolt-sql-server:2.3.2", "2.3.2", true},
		{"localhost:5000/dolt-sql-server", "", false},
		{"dolthub/dolt-sql-server", "", false},
		{"", "", false},
	}
	for _, tc := range tests {
		got, ok := doltImageTag(tc.ref)
		if got != tc.want || ok != tc.ok {
			t.Errorf("doltImageTag(%q) = (%q, %v), want (%q, %v)", tc.ref, got, ok, tc.want, tc.ok)
		}
	}
}

func TestDoltVersionCheckNoPlainReleaseIsAnError(t *testing.T) {
	t.Parallel()
	if _, err := newestDoltRelease([]string{"latest", "2.8.0-rc1"}); err == nil {
		t.Errorf("newestDoltRelease with no plain tag returned no error")
	}
}

func TestDoltVersionCheckListingTagsAreRead(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(page int) (int, string) {
		if page == 1 {
			return doltTagPage(false, "2.5.0")
		}
		return doltTagPage(true, "2.7.2")
	})
	check, _ := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", reg)
	var logs doltLogs
	check.run(context.Background(), "/town", doltCheckStart, logs.logf)
	if reg.requests.Load() != 2 {
		t.Errorf("registry requests = %d, want 2 (both pages)", reg.requests.Load())
	}
	if got := check.observed(); len(got) != 1 || !strings.Contains(got[0].Summary, "2.7.2") {
		t.Errorf("observed = %+v, want one item naming 2.7.2 from the second page", got)
	}
}
