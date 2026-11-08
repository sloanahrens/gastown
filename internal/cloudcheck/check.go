package cloudcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const (
	// DefaultTimeout bounds one fetch when Options.Timeout is unset.
	DefaultTimeout = 5 * time.Second
	// maxBodyDrain is how much of a response body is read and discarded, so a
	// body that never ends cannot hold the run open.
	maxBodyDrain = 4 << 10
)

// Options configures one Check.
type Options struct {
	// OffDir holds the .off markers; empty checks every app.
	OffDir string
	// Timeout bounds each fetch; zero uses DefaultTimeout.
	Timeout time.Duration
	// Now stamps the report; nil uses time.Now.
	Now func() time.Time
}

// Check fetches every app and returns the run's report. An app whose section
// has an .off marker in OffDir is reported off and not fetched; an app with no
// section reads no marker, because the marker is named by the app= line and a
// check with no app= has none (the devops rig's ops/cloud-check.sh). Redirects
// are not followed: the run fetches only the URLs the apps file names, and a
// 3xx answer is a warning.
func Check(ctx context.Context, apps []App, problems []Problem, o Options) Report {
	now := time.Now
	if o.Now != nil {
		now = o.Now
	}
	timeout := o.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	client := &http.Client{
		Timeout: timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	r := Report{At: now().Unix(), Items: []Item{}, Rows: []Row{}}
	for _, p := range problems {
		r.record(Row{Level: p.Level, Name: "apps file", Detail: p.detail()})
	}
	for _, app := range apps {
		r.record(checkApp(ctx, client, o.OffDir, app))
	}
	return r
}

// checkApp reports one app's level without deciding the run's exit status.
func checkApp(ctx context.Context, client *http.Client, offDir string, app App) Row {
	row := Row{Section: app.Section, Name: app.Name}
	if offDir != "" && app.Section != "" {
		marker := filepath.Join(offDir, app.Section+".off")
		switch _, err := os.Stat(marker); {
		case err == nil:
			row.Level = LevelOff
			row.Detail = fmt.Sprintf("%s is off on purpose (app %s, %s)", app.Name, app.Section, marker)
			return row
		case !errors.Is(err, fs.ErrNotExist):
			// An unreadable marker must not read as "not off".
			row.Level, row.Detail = LevelFail, "cannot read the off marker: "+errText(err)
			return row
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, app.URL, nil)
	if err != nil {
		row.Level, row.Detail = LevelFail, "cannot build the request: "+errText(err)
		return row
	}
	resp, err := client.Do(req)
	if err != nil {
		row.Level, row.Detail = LevelFail, "GET failed: "+errText(err)
		return row
	}
	defer func() { _ = resp.Body.Close() }()
	// The body is not part of the verdict; reading a bounded prefix lets the
	// connection be reused, and a read error there changes no level.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBodyDrain))
	row.Detail = fmt.Sprintf("HTTP %d", resp.StatusCode)
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		row.Level = LevelOK
	case resp.StatusCode >= 500:
		row.Level = LevelFail
	default:
		row.Level = LevelWarn
	}
	return row
}

// errText is err's message without the request URL: a URL from the apps file
// may carry a token, and an error is not the place for it.
func errText(err error) string {
	var ue *url.Error
	if errors.As(err, &ue) && ue.Err != nil {
		err = ue.Err
	}
	return clean(err.Error(), detailMax)
}
