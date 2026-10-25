// Package pgtest starts a private PostgreSQL 17 for tests: one cluster per
// test package on a unix socket in a temp directory, one template database
// built once, and a cheap clone of it per test. It never listens on TCP.
//
// Locating the binaries is untagged and unit-tested; the cluster itself needs
// the real binaries and builds only with the integration tag.
package pgtest
