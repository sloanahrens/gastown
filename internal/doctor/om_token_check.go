package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/steveyegge/gastown/internal/config"
	"github.com/steveyegge/gastown/internal/daemon"
)

// omTokenWarnWindow is how long before the subscription token expires the check
// starts to warn.
const omTokenWarnWindow = 30 * 24 * time.Hour

// OMTokenExpiryCheck warns before the subscription token om authenticates with
// in the daemon container expires. The token lasts a year and a dead one rejects
// every landing to needs-human (gt-qpeei.37). It reads the expiry date from the
// expires_at field of the JSON sidecar that daemon.om_token_meta names, and
// never opens the token file: the sidecar is the only file it reads, and it
// holds a date.
//
// States: skipped (the town sets no daemon.om_token_meta, so om is not run on a
// token here), ok (more than 30 days left), warning (30 days or fewer left, or
// the expiry cannot be read), error (already expired).
type OMTokenExpiryCheck struct {
	BaseCheck

	// settings reads the town's daemon settings; nil is daemon.ReadPatrolConfig.
	settings func(townRoot string) (*config.DaemonPatrolConfig, error)
	// readFile reads the sidecar; nil is os.ReadFile.
	readFile func(path string) ([]byte, error)
	// now is the clock; nil is time.Now.
	now func() time.Time
}

// NewOMTokenExpiryCheck creates the om token expiry check.
func NewOMTokenExpiryCheck() *OMTokenExpiryCheck {
	return &OMTokenExpiryCheck{
		BaseCheck: BaseCheck{
			CheckName:        "om-token-expiry",
			CheckDescription: "Warn 30 days before the om subscription token expires",
			CheckCategory:    CategoryInfrastructure,
		},
	}
}

// omTokenMeta is the sidecar's one field.
type omTokenMeta struct {
	ExpiresAt string `json:"expires_at"`
}

// Run reads the town's settings, then the sidecar they name, and judges the
// date against the clock.
func (c *OMTokenExpiryCheck) Run(ctx *CheckContext) *CheckResult {
	settings := c.settings
	if settings == nil {
		settings = daemon.ReadPatrolConfig
	}
	cfg, err := settings(ctx.TownRoot)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "unknown: could not read the town's daemon settings",
			Details: []string{err.Error()},
		}
	}
	path := cfg.OMTokenMeta()
	if path == "" {
		return &CheckResult{Name: c.Name(), Status: StatusSkipped, Message: "daemon.om_token_meta is not set: om runs on no subscription token here"}
	}
	if !filepath.IsAbs(path) {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: "unknown: daemon.om_token_meta is not an absolute path",
			Details: []string{fmt.Sprintf("daemon.om_token_meta %q", path)},
		}
	}

	read := c.readFile
	if read == nil {
		read = os.ReadFile
	}
	data, err := read(path)
	if err != nil {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("unknown: the token's expiry sidecar %s cannot be read", path),
			Details: []string{err.Error()},
		}
	}
	expires, ok := parseOMTokenExpiry(data)
	if !ok {
		return &CheckResult{
			Name:    c.Name(),
			Status:  StatusWarning,
			Message: fmt.Sprintf("unknown: %s has no parseable expires_at (want RFC 3339 or YYYY-MM-DD)", path),
		}
	}

	now := c.now
	if now == nil {
		now = time.Now
	}
	return judgeOMTokenExpiry(c.Name(), expires, now())
}

// parseOMTokenExpiry reads expires_at from the sidecar's JSON. A date with no
// time is that day's start in UTC, so a token that expires on the 28th warns
// as if it were gone from the beginning of the 28th rather than the end.
func parseOMTokenExpiry(data []byte) (time.Time, bool) {
	var meta omTokenMeta
	if err := json.Unmarshal(data, &meta); err != nil || meta.ExpiresAt == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, meta.ExpiresAt); err == nil {
		return t, true
	}
	if t, err := time.Parse(time.DateOnly, meta.ExpiresAt); err == nil {
		return t, true
	}
	return time.Time{}, false
}

// judgeOMTokenExpiry turns the expiry and the clock into the check's answer.
// The message names the date and the days left and nothing else.
func judgeOMTokenExpiry(name string, expires, now time.Time) *CheckResult {
	remaining := expires.Sub(now)
	// Keep the zone the operator wrote: in UTC an early-morning expiry east of it names the day before.
	date := expires.Format(time.DateOnly)
	switch {
	case remaining <= 0:
		ago := int(-remaining / (24 * time.Hour))
		return &CheckResult{
			Name:    name,
			Status:  StatusError,
			Message: fmt.Sprintf("the om token expired on %s (%d days ago): every landing review is rejected until it is replaced", date, ago),
			FixHint: "generate a new token with claude setup-token, replace the token file, and update expires_at in the sidecar",
		}
	case remaining <= omTokenWarnWindow:
		return &CheckResult{
			Name:    name,
			Status:  StatusWarning,
			Message: fmt.Sprintf("the om token expires on %s (%s)", date, daysLeft(int(remaining/(24*time.Hour)))),
			FixHint: "generate a new token with claude setup-token, replace the token file, and update expires_at in the sidecar",
		}
	}
	return &CheckResult{
		Name:    name,
		Status:  StatusOK,
		Message: fmt.Sprintf("the om token expires on %s (%s)", date, daysLeft(int(remaining/(24*time.Hour)))),
	}
}

// daysLeft words a whole number of days left, singular for one.
func daysLeft(days int) string {
	if days == 1 {
		return "1 day left"
	}
	return fmt.Sprintf("%d days left", days)
}
