package pgdsn

import (
	"strings"
	"testing"
)

// Each case is one connection string form, and the settings that must survive
// the edit.
func TestWithoutPassword(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dsn  string
		// want are settings the result must keep; gone are strings it must not
		// hold.
		want []string
		gone []string
	}{
		{
			name: "keyword form",
			dsn:  "host=db port=5433 user=tracker password='s3cr t' dbname=tracker sslmode=require",
			want: []string{"host=db", "port=5433", "dbname=tracker", "sslmode=require"},
			gone: []string{"password", "s3cr"},
		},
		{
			name: "keyword form with an escaped quote",
			dsn:  `host=db password='pa\'ss word' sslmode=require`,
			want: []string{"host=db", "sslmode=require"},
			gone: []string{"word", `\'`},
		},
		{
			name: "keyword form with no password",
			dsn:  "host=db dbname=tracker sslmode=disable",
			want: []string{"host=db", "dbname=tracker", "sslmode=disable"},
		},
		{
			name: "URI form",
			dsn:  "postgres://tracker:uripw@db.internal/tracker?sslmode=verify-full",
			want: []string{"postgres://tracker@db.internal/tracker", "sslmode=verify-full"},
			gone: []string{"uripw"},
		},
		{
			name: "URI form with a password in the query",
			dsn:  "postgres://tracker@db.internal/tracker?sslmode=require&password=qpw",
			want: []string{"sslmode=require"},
			gone: []string{"qpw"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := WithoutPassword(c.dsn)
			if err != nil {
				t.Fatalf("WithoutPassword(%q): %v", c.dsn, err)
			}
			for _, s := range c.want {
				if !strings.Contains(got, s) {
					t.Errorf("WithoutPassword(%q) = %q, want it to keep %q", c.dsn, got, s)
				}
			}
			for _, s := range c.gone {
				if strings.Contains(got, s) {
					t.Errorf("WithoutPassword(%q) = %q, want %q gone", c.dsn, got, s)
				}
			}
		})
	}
}

// The password reads back from the same forms WithoutPassword strips.
func TestPassword(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dsn  string
		want string
	}{
		{name: "keyword form", dsn: "host=db password='s3cr t' dbname=tracker", want: "s3cr t"},
		{name: "keyword form with an escaped quote", dsn: `host=db password='pa\'ss'`, want: "pa'ss"},
		{name: "keyword form with no password", dsn: "host=db dbname=tracker"},
		{name: "URI form", dsn: "postgres://tracker:uripw@db.internal/tracker?sslmode=require", want: "uripw"},
		{name: "URI form with a password in the query", dsn: "postgres://tracker@db.internal/tracker?password=qpw", want: "qpw"},
		{name: "URI form with no password", dsn: "postgres://tracker@db.internal/tracker"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := Password(c.dsn)
			if err != nil {
				t.Fatalf("Password(%q): %v", c.dsn, err)
			}
			if got != c.want {
				t.Errorf("Password(%q) = %q, want %q", c.dsn, got, c.want)
			}
		})
	}
}

// WithDatabase replaces the database and keeps every other setting, in both
// forms (gt-p85pw.72).
func TestWithDatabase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		dsn  string
		db   string
		want string
	}{
		{
			name: "keyword form",
			dsn:  "host=db port=5433 user=tracker dbname=tracker sslmode=require",
			db:   "restorecheck_probe",
			want: "host=db port=5433 user=tracker sslmode=require dbname=restorecheck_probe",
		},
		{
			name: "keyword form with no database",
			dsn:  "host=db sslmode=disable",
			db:   "probe",
			want: "host=db sslmode=disable dbname=probe",
		},
		{
			name: "URI form",
			dsn:  "postgres://tracker@db.internal/tracker?sslmode=verify-full",
			db:   "restorecheck_probe",
			want: "postgres://tracker@db.internal/restorecheck_probe?sslmode=verify-full",
		},
		{
			name: "URI form with a dbname in the query",
			dsn:  "postgres://tracker@db.internal/tracker?dbname=tracker&sslmode=require",
			db:   "probe",
			want: "postgres://tracker@db.internal/probe?sslmode=require",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, err := WithDatabase(c.dsn, c.db)
			if err != nil {
				t.Fatalf("WithDatabase(%q, %q): %v", c.dsn, c.db, err)
			}
			if got != c.want {
				t.Errorf("WithDatabase(%q, %q) = %q, want %q", c.dsn, c.db, got, c.want)
			}
		})
	}
}

// A parse failure never echoes the string it could not read: a password can
// ride in it (gt-p85pw.72).
func TestErrorsDoNotQuoteTheDSN(t *testing.T) {
	t.Parallel()
	const dsn = "postgres://tracker:s3cr3t@db.internal:%zz/tracker"
	for name, f := range map[string]func() (string, error){
		"WithoutPassword": func() (string, error) { return WithoutPassword(dsn) },
		"Password":        func() (string, error) { return Password(dsn) },
		"WithDatabase":    func() (string, error) { return WithDatabase(dsn, "probe") },
	} {
		_, err := f()
		if err == nil {
			t.Errorf("%s(%q) = nil error, want a refusal", name, dsn)
			continue
		}
		if strings.Contains(err.Error(), "s3cr3t") || strings.Contains(err.Error(), dsn) {
			t.Errorf("%s error quotes the connection string: %v", name, err)
		}
	}
}

// A name that is not a plain identifier is refused, so it cannot splice a
// setting into a keyword string (gt-p85pw.72).
func TestWithDatabaseRefusesAnUnsafeName(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"", "Bad-Name", "probe sslmode=disable", "probe;drop"} {
		if _, err := WithDatabase("host=db dbname=tracker", name); err == nil {
			t.Errorf("WithDatabase(_, %q) = nil error, want a refusal", name)
		}
	}
}
