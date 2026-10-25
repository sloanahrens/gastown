// Package modsync holds the test that keeps the tracker module's dependencies
// resolvable from the root module's. A gate that lints tracker/ in a sandbox
// with a read-only shared module cache has the versions the root module pins
// and nothing else; a tracker requirement at another version cannot resolve
// there and fails the gate with "cannot find module" (the mysql driver at
// v1.10.1 did, 2026-10-25).
package modsync
