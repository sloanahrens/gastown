// Package loadtest holds the tracker's concurrency test: many seats against
// one real Postgres through the real server and client. It has no non-test
// code and builds only with the integration tag.
package loadtest
