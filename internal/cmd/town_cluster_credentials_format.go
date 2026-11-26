package cmd

import (
	"errors"
	"strings"
	"time"
)

// formatCredential is the one line the credential command prints: the role, the certificate subject and
// the expiry in UTC RFC3339, tab-separated. It takes no key and no path. A zero expiry is refused rather
// than printed as year 1, and a role or subject holding a line break or tab is refused, since either would
// break the one-line, tab-separated line the reader splits.
func formatCredential(role string, subject string, notAfter time.Time) (string, error) {
	switch {
	case role == "":
		return "", errors.New("a missing role: the line needs the role the credential was issued for")
	case subject == "":
		return "", errors.New("a missing subject: the line needs the certificate subject")
	case notAfter.IsZero():
		return "", errors.New("a zero expiry: the certificate has no not-after time to print")
	case strings.ContainsAny(role+subject, "\r\n\t"):
		return "", errors.New("the role or subject holds a line break or tab: the line must be one line")
	}
	return role + "\t" + subject + "\t" + notAfter.UTC().Format(time.RFC3339), nil
}
