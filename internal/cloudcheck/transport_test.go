package cloudcheck

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// fakeTransport answers each URL with its reply and records every URL asked.
type fakeTransport struct {
	replies map[string]fakeReply
	asked   []string
}

// fakeReply is one URL's answer: a status with an optional Location header, or
// an error.
type fakeReply struct {
	status   int
	location string
	err      error
}

func (f *fakeTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	url := req.URL.String()
	f.asked = append(f.asked, url)
	reply, ok := f.replies[url]
	if !ok {
		return nil, errors.New("fake transport has no reply for the URL")
	}
	if reply.err != nil {
		return nil, reply.err
	}
	resp := &http.Response{
		StatusCode: reply.status,
		Header:     http.Header{},
		Body:       http.NoBody,
	}
	if reply.location != "" {
		resp.Header.Set("Location", reply.location)
	}
	return resp, nil
}

func TestTransportCarriesTheVerdict(t *testing.T) {
	t.Parallel()
	appURL := "http://app.invalid/health"
	f := &fakeTransport{replies: map[string]fakeReply{appURL: {status: http.StatusOK}}}
	r := Check(context.Background(), []App{{Name: "web", URL: appURL}}, nil, Options{Transport: f})
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelOK || len(f.asked) != 1 || f.asked[0] != appURL {
		t.Fatalf("rows = %+v, asked = %v; want one ok row from the fake's answer for %s", r.Rows, f.asked, appURL)
	}
}

func TestTransportRedirectIsAWarningAndNotFollowed(t *testing.T) {
	t.Parallel()
	appURL := "http://app.invalid/health"
	location := "http://elsewhere.invalid/"
	f := &fakeTransport{replies: map[string]fakeReply{
		appURL: {status: http.StatusFound, location: location},
	}}
	r := Check(context.Background(), []App{{Name: "web", URL: appURL}}, nil, Options{Transport: f})
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelWarn {
		t.Fatalf("rows = %+v, want one warn row for the 302", r.Rows)
	}
	if len(f.asked) != 1 || f.asked[0] != appURL {
		t.Errorf("fake was asked %v, want only the app URL, never its Location %s", f.asked, location)
	}
}

// TestTransportErrorFailsTheRow pins that a transport error the fake returns is
// the row's failure and that the URL's credential stays out of the detail.
func TestTransportErrorFailsTheRow(t *testing.T) {
	t.Parallel()
	appURL := "http://app.invalid/health?token=verysecret"
	f := &fakeTransport{replies: map[string]fakeReply{appURL: {err: errors.New("connection refused")}}}
	r := Check(context.Background(), []App{{Name: "web", URL: appURL}}, nil, Options{Transport: f})
	if len(r.Rows) != 1 || r.Rows[0].Level != LevelFail || r.Failed != 1 {
		t.Fatalf("rows = %+v, want one fail row", r.Rows)
	}
	if got := r.Rows[0].Detail; !strings.Contains(got, "connection refused") || strings.Contains(got, "verysecret") {
		t.Errorf("detail = %q, want the fake's error and no URL", got)
	}
}
