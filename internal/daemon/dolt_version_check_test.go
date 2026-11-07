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
	var logs doltLogs
	check.run(context.Background(), "/town", doltCheckStart, logs.logf)

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
		return doltTagPage(true, "2.7.1", "2.7.2")
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
		name     string
		running  string
		releases []string
		want     bool
	}{
		{"same release clears", "2.7.2", []string{"2.7.2"}, false},
		{"one release behind raises nothing", "2.7.1", []string{"2.7.1", "2.7.2"}, false},
		{"two releases behind raises", "2.7.0", []string{"2.7.0", "2.7.1", "2.7.2"}, true},
		{"one release across a minor raises nothing", "2.6.9", []string{"2.6.9", "2.7.0"}, false},
		{"two releases across a minor raise", "2.6.9", []string{"2.6.9", "2.7.0", "2.7.1"}, true},
		{"far behind raises", "2.3.2", []string{"2.3.2", "2.4.0", "2.5.1", "2.7.2"}, true},
		{"running ahead of the registry is not behind", "2.8.0", []string{"2.7.2", "2.8.0"}, false},
		{"numeric not lexical: 2.9 is behind 2.10", "2.9.0", []string{"2.9.0", "2.10.0", "2.10.1"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			running, ok := parseDoltVersion(tc.running)
			if !ok {
				t.Fatalf("running %q is not a plain release", tc.running)
			}
			var releases []doltVersion
			for _, tag := range tc.releases {
				v, ok := parseDoltVersion(tag)
				if !ok {
					t.Fatalf("release %q is not a plain release", tag)
				}
				releases = append(releases, v)
			}
			if got := doltImageBehind(running, releases); got != tc.want {
				t.Errorf("doltImageBehind(%s, %v) = %v, want %v", tc.running, tc.releases, got, tc.want)
			}
		})
	}
}

func TestDoltVersionCheckIgnoresNonReleaseTags(t *testing.T) {
	t.Parallel()
	releases, err := doltReleases([]string{"latest", "2.8.0-rc1", "sha-1a2b3c", "2.7.2", "2.7", "v2.9.0", "2.10.0"})
	if err != nil {
		t.Fatalf("doltReleases: %v", err)
	}
	if newest := doltNewest(releases); newest.tag != "2.10.0" {
		t.Errorf("newest = %s, want 2.10.0", newest.tag)
	}
	if len(releases) != 2 {
		t.Errorf("releases = %d, want the two plain ones (2.7.2, 2.10.0)", len(releases))
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
			answer:  func(int) (int, string) { return doltTagPage(true, "2.7.1", "2.7.2") },
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
			answer:  func(int) (int, string) { return doltTagPage(true, "2.7.1", "2.7.2") },
		},
		{
			name:   "registry request is cancelled",
			ctx:    cancelled,
			answer: func(int) (int, string) { return doltTagPage(true, "2.7.1", "2.7.2") },
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
	healthy := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.1", "2.7.2") })
	broken := newDoltFakeRegistry(func(int) (int, string) { return http.StatusBadGateway, "" })
	check, _ := newDoltImageCheckFor("dolthub/dolt-sql-server:2.3.2", healthy)
	var logs doltLogs
	clock := clockwork.NewFakeClockAt(doltCheckStart)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	held := check.observed()

	check.client = broken.client(0)
	clock.Advance(time.Hour)
	check.run(context.Background(), "/town", clock.Now(), logs.logf)
	if got := check.observed(); len(got) != 1 {
		t.Errorf("after a failed read, observed = %d items, want the held one kept", len(got))
	}
	if got := check.observed(); len(got) != 1 || len(held) != 1 || got[0] != held[0] {
		t.Errorf("after a failed read, observed = %+v, want the held item %+v unchanged", got, held)
	}
	if len(logs) != 1 {
		t.Errorf("logs = %v, want the failed hourly run logged once", logs)
	}
}

func TestDoltVersionCheckDockerAndContainerFailuresRaiseNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		container func(string) (string, error)
		image     string
		dockerErr error
	}{
		{
			name:      "docker inspect fails",
			image:     "dolthub/dolt-sql-server:2.3.2",
			dockerErr: errors.New("exit status 1"),
		},
		{
			name:  "container setting does not parse",
			image: "dolthub/dolt-sql-server:2.3.2",
			container: func(string) (string, error) {
				return "", errors.New("dolt.external_container is malformed")
			},
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
			name:  "running image is not a plain release",
			image: "dolthub/dolt-sql-server:latest",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.1", "2.7.2") })
			check, docker := newDoltImageCheckFor(tc.image, reg)
			docker.err = tc.dockerErr
			if tc.container != nil {
				check.container = tc.container
			}
			var logs doltLogs
			check.run(context.Background(), "/town", doltCheckStart, logs.logf)

			if got := check.observed(); len(got) != 0 {
				t.Errorf("observed = %+v, want none", got)
			}
			if len(logs) != 1 {
				t.Errorf("logs = %v, want one line", logs)
			}
			if reg.requests.Load() != 0 {
				t.Errorf("registry was asked after a failed read of the running image")
			}
		})
	}
}

func TestDoltVersionCheckHostProcessTownClearsItem(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.1", "2.7.2") })
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
	reg := newDoltFakeRegistry(func(int) (int, string) { return doltTagPage(true, "2.7.1", "2.7.2") })
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
	if _, err := doltReleases([]string{"latest", "2.8.0-rc1"}); err == nil {
		t.Errorf("doltReleases with no plain tag returned no error")
	}
}

func TestDoltVersionCheckListingTagsAreRead(t *testing.T) {
	t.Parallel()
	reg := newDoltFakeRegistry(func(page int) (int, string) {
		if page == 1 {
			return doltTagPage(false, "2.5.0")
		}
		return doltTagPage(true, "2.7.1", "2.7.2")
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
