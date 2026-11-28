package cloudcheck

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHealthLevels(t *testing.T) {
	t.Parallel()
	okURL, failedURL, warnedURL, goneURL :=
		"http://web.invalid/", "http://api.invalid/", "http://docs.invalid/", "http://dead.invalid/"
	f := &fakeTransport{replies: map[string]fakeReply{
		okURL:     {status: http.StatusOK},
		failedURL: {status: http.StatusBadGateway},
		warnedURL: {status: http.StatusNotFound},
		// A refused connection: the fake answers with an error, so no socket
		// and no deadline decides the verdict.
		goneURL: {err: errors.New("connection refused")},
	}}

	offDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(offDir, "legacy.off"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	apps := []App{
		{Name: "web", URL: okURL},
		{Name: "api", URL: failedURL},
		{Name: "docs", URL: warnedURL},
		{Name: "dead", URL: goneURL},
		{Section: "legacy", Name: "site", URL: failedURL},
	}
	at := time.Unix(1700000000, 0)
	r := Check(context.Background(), apps, nil, Options{OffDir: offDir, Timeout: time.Second, Transport: f, Now: func() time.Time { return at }})

	if r.At != at.Unix() {
		t.Errorf("At = %d, want %d", r.At, at.Unix())
	}
	want := []struct {
		level   Level
		section string
		name    string
	}{
		{LevelOK, "", "web"},
		{LevelFail, "", "api"},
		{LevelWarn, "", "docs"},
		{LevelFail, "", "dead"},
		{LevelOff, "legacy", "site"},
	}
	if len(r.Rows) != len(want) {
		t.Fatalf("rows = %+v, want %d rows", r.Rows, len(want))
	}
	for i, w := range want {
		if r.Rows[i].Level != w.level || r.Rows[i].Section != w.section || r.Rows[i].Name != w.name {
			t.Errorf("row %d = %s %q %q, want %s %q %q", i, r.Rows[i].Level, r.Rows[i].Section, r.Rows[i].Name, w.level, w.section, w.name)
		}
	}
	if r.OK != 1 || r.Warnings != 1 || r.Failed != 2 || r.Off != 1 {
		t.Errorf("counts = %d ok, %d warnings, %d failed, %d off; want 1, 1, 2, 1", r.OK, r.Warnings, r.Failed, r.Off)
	}
	// The status file carries the failures and warnings only, in file order.
	if len(r.Items) != 3 {
		t.Fatalf("items = %+v, want 3", r.Items)
	}
	for i, wantLevel := range []Level{LevelFail, LevelWarn, LevelFail} {
		if r.Items[i].Level != wantLevel {
			t.Errorf("item %d level = %s, want %s", i, r.Items[i].Level, wantLevel)
		}
	}
}

// TestOffMarkerSkipsTheFetch pins that an off app is not fetched: the marker
// stands the check down, it does not merely relabel a result.
func TestOffMarkerSkipsTheFetch(t *testing.T) {
	t.Parallel()
	appURL := "http://web.invalid/"
	f := &fakeTransport{replies: map[string]fakeReply{appURL: {status: http.StatusOK}}}

	offDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(offDir, "gastown.off"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	apps := []App{{Section: "gastown", Name: "web", URL: appURL}}
	r := Check(context.Background(), apps, nil, Options{OffDir: offDir, Timeout: time.Second, Transport: f})

	if r.Failed != 0 {
		t.Errorf("Failed = %d, want 0 for an app that is off on purpose", r.Failed)
	}
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelOff {
		t.Errorf("rows = %+v, want one off row", r.Rows)
	}
	want := "web is off on purpose (app gastown, " + filepath.Join(offDir, "gastown.off") + ")"
	if got := r.Rows[0].Detail; got != want {
		t.Errorf("detail = %q, want %q", got, want)
	}
	if got := len(f.asked); got != 0 {
		t.Errorf("the off app was fetched %d time(s), want 0", got)
	}
}

// TestNoAppMarkerIsNotRead pins that a check with no app= line reads no marker:
// the marker is named by the app= line, so a <check>.off must not stand the
// check down (the devops rig's ops/cloud-check.sh).
func TestNoAppMarkerIsNotRead(t *testing.T) {
	t.Parallel()
	appURL := "http://web.invalid/"
	f := &fakeTransport{replies: map[string]fakeReply{appURL: {status: http.StatusOK}}}
	offDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(offDir, "web.off"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake answers 200, so a reader that consults <check>.off reports off
	// instead of the checked answer.
	r := Check(context.Background(), []App{{Name: "web", URL: appURL}}, nil, Options{OffDir: offDir, Timeout: time.Second, Transport: f})
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelOK {
		t.Fatalf("rows = %+v, want one ok row: a check with no app= reads no marker", r.Rows)
	}
	if r.Off != 0 {
		t.Errorf("Off = %d, want 0", r.Off)
	}
}

func TestOffMarkerUnreadableFails(t *testing.T) {
	t.Parallel()
	// A regular file where the off directory should be makes every marker
	// stat fail with ENOTDIR, which is not "no marker".
	notADir := filepath.Join(t.TempDir(), "off")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// The fake answers 200, so an implementation that reads the unreadable
	// marker as "not off" reports ok instead of the failure it is.
	appURL := "http://web.invalid/"
	f := &fakeTransport{replies: map[string]fakeReply{appURL: {status: http.StatusOK}}}

	r := Check(context.Background(), []App{{Section: "gastown", Name: "web", URL: appURL}}, nil, Options{OffDir: notADir, Timeout: time.Second, Transport: f})
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelFail {
		t.Fatalf("rows = %+v, want one fail row", r.Rows)
	}
	if r.Failed != 1 {
		t.Errorf("Failed = %d, want 1", r.Failed)
	}
	if got := r.Rows[0].Detail; !strings.Contains(got, "off marker") {
		t.Errorf("detail = %q, want it to name the unreadable off marker", got)
	}
}

// TestRedirectIsNotFollowed pins that the run fetches only the URLs the apps
// file names: a 3xx is reported, not chased to another host.
func TestRedirectIsNotFollowed(t *testing.T) {
	t.Parallel()
	appURL := "http://app.invalid/health"
	targetURL := "http://elsewhere.invalid/"
	// The fake answers the app URL with a redirect and has no reply for the
	// Location, so following it would fail the row rather than pass.
	f := &fakeTransport{replies: map[string]fakeReply{
		appURL: {status: http.StatusFound, location: targetURL},
	}}

	r := Check(context.Background(), []App{{Name: "web", URL: appURL}}, nil, Options{Timeout: time.Second, Transport: f})
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelWarn {
		t.Fatalf("rows = %+v, want one warn row", r.Rows)
	}
	if len(f.asked) != 1 || f.asked[0] != appURL {
		t.Errorf("fake was asked %v, want only the app URL %s, never its Location %s", f.asked, appURL, targetURL)
	}
}

// TestUnbuildableRequestFails pins that a URL Check is handed that cannot be
// fetched is a failure, never a skipped check.
func TestUnbuildableRequestFails(t *testing.T) {
	t.Parallel()
	r := Check(context.Background(), []App{{Name: "web", URL: "http://[::1"}}, nil, Options{Timeout: time.Second})
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelFail || r.Failed != 1 {
		t.Fatalf("rows = %+v, want one fail row", r.Rows)
	}
}

// TestFetchErrorNamesNoURL pins that a URL's credential does not reach the
// report, which is printed and written to the status file. The fake refuses the
// URL carrying the token, so the error the client builds from it carries that
// URL too; the row's detail must not.
func TestFetchErrorNamesNoURL(t *testing.T) {
	t.Parallel()
	appURL := "http://dead.invalid/health?token=verysecret"
	f := &fakeTransport{replies: map[string]fakeReply{appURL: {err: errors.New("connection refused")}}}

	r := Check(context.Background(), []App{{Name: "web", URL: appURL}}, nil, Options{Timeout: time.Second, Transport: f})
	if len(f.asked) != 1 || f.asked[0] != appURL {
		t.Fatalf("fake was asked %v, want the URL carrying the token %s", f.asked, appURL)
	}
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelFail {
		t.Fatalf("rows = %+v, want one fail row", r.Rows)
	}
	if got := r.Rows[0].Detail; !strings.Contains(got, "connection refused") {
		t.Errorf("detail = %q, want the fake's error", got)
	}
	if strings.Contains(r.Rows[0].Detail, "verysecret") {
		t.Errorf("the failure detail carries the URL's token: %q", r.Rows[0].Detail)
	}
}

// TestReportTextIsCapped pins that a reported value from an apps-file defect
// is stripped of control characters and cut to the status file's cap.
func TestReportTextIsCapped(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", 500) + "\n\ta second line"
	r := Check(context.Background(), nil, []Problem{{Level: LevelWarn, Line: 3, Reason: long}}, Options{})
	if len(r.Rows) != 1 || len(r.Items) != 1 {
		t.Fatalf("rows = %+v, items = %+v, want one of each", r.Rows, r.Items)
	}
	for _, text := range []string{r.Rows[0].Detail, r.Items[0].Text} {
		if n := len([]rune(text)); n > detailMax {
			t.Errorf("text is %d runes long, want at most %d", n, detailMax)
		}
		if strings.ContainsAny(text, "\n\t") {
			t.Errorf("text carries a control character: %q", text)
		}
	}
}

// TestUnknownLevelCountsAsAWarning pins that a level the report does not know
// is not counted as a pass.
func TestUnknownLevelCountsAsAWarning(t *testing.T) {
	t.Parallel()
	r := Check(context.Background(), nil, []Problem{{Level: Level("green"), Reason: "an app nobody checked"}}, Options{})
	if r.OK != 0 || r.Warnings != 1 {
		t.Errorf("counts = %d ok, %d warnings, want 0 ok, 1 warning", r.OK, r.Warnings)
	}
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelWarn {
		t.Errorf("rows = %+v, want one warn row", r.Rows)
	}
	if len(r.Items) != 1 {
		t.Errorf("items = %+v, want the row kept as a warning", r.Items)
	}
	if got := r.Rows[0].Detail; !strings.Contains(got, "unknown level 'green'") {
		t.Errorf("detail = %q, want it to keep the unknown level's name", got)
	}
}

func TestNoAppsProblemFails(t *testing.T) {
	t.Parallel()
	r := Check(context.Background(), nil, []Problem{{Level: LevelFail, Line: 0, Reason: "names no apps"}}, Options{})
	if r.Failed != 1 {
		t.Errorf("Failed = %d, want 1", r.Failed)
	}
	if len(r.Items) != 1 || r.Items[0].Level != LevelFail {
		t.Errorf("items = %+v, want one fail item", r.Items)
	}
}
