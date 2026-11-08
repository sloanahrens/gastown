package cloudledger

import "time"

// WithReadTimeout returns p reading under d rather than PostgresReadTimeout, so
// a test can drive the deadline without waiting out the production one.
func WithReadTimeout(p *PostgresSource, d time.Duration) *PostgresSource {
	p.timeout = d
	return p
}

// WithDriver returns p opening the database/sql driver name, so a test can name
// a driver this binary does not link.
func WithDriver(p *PostgresSource, name string) *PostgresSource {
	p.driver = name
	return p
}
