package cmd

import (
	"strings"
	"testing"
	"time"
)

// TestFormatCredential drives the line formatCredential prints for each role, and each refusal. The expiry
// is given in a +02:00 zone so the printed line proves the conversion to UTC.
func TestFormatCredential(t *testing.T) {
	t.Parallel()
	cest := time.FixedZone("CEST", 2*60*60)
	expiry := time.Date(2026, 11, 27, 12, 0, 0, 0, cest)
	for _, tc := range []struct {
		name, role, subject string
		notAfter            time.Time
		want                string
		wantErr             string
	}{
		{"role read", "read", "CN=gt-deployer-read", expiry, "read\tCN=gt-deployer-read\t2026-11-27T10:00:00Z", ""},
		{"role policy", "policy", "CN=gt-deployer-policy", expiry, "policy\tCN=gt-deployer-policy\t2026-11-27T10:00:00Z", ""},
		{"role apply", "apply", "CN=gt-deployer-apply", expiry, "apply\tCN=gt-deployer-apply\t2026-11-27T10:00:00Z", ""},
		{"a subject with a space", "apply", "CN=gt-deployer-apply, O=system:masters", expiry,
			"apply\tCN=gt-deployer-apply, O=system:masters\t2026-11-27T10:00:00Z", ""},
		{"a zero expiry", "apply", "CN=gt-deployer-apply", time.Time{}, "", "expiry"},
		{"an empty role", "", "CN=gt-deployer-apply", expiry, "", "role"},
		{"an empty subject", "apply", "", expiry, "", "subject"},
		{"a newline in the subject", "apply", "CN=a\nb", expiry, "", "one line"},
		{"a tab in the subject", "apply", "CN=a\tb", expiry, "", "one line"},
		{"a newline in the role", "app\nly", "CN=gt-deployer-apply", expiry, "", "one line"},
		{"a tab in the role", "app\tly", "CN=gt-deployer-apply", expiry, "", "one line"},
	} {
		got, err := formatCredential(tc.role, tc.subject, tc.notAfter)
		if tc.wantErr != "" {
			if err == nil {
				t.Errorf("%s: formatCredential() = %q, want a refusal", tc.name, got)
				continue
			}
			if got != "" {
				t.Errorf("%s: line %q returned beside error %v, want none", tc.name, got, err)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("%s: error %q does not name %q", tc.name, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: formatCredential(): %v", tc.name, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s: formatCredential() = %q, want %q", tc.name, got, tc.want)
		}
	}
}
