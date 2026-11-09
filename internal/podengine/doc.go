// Package podengine holds the guard that keeps the daemon pod's container
// engine question answered: every non-test Go file under internal/ that holds
// the docker program literal is named in docs/design/pod-engine.md, each call
// site among them with a disposition, and that document is the one place the
// pod's answer for it lives (gt-cd5rqa).
//
// The document is the deliverable and its table is read by the overseer; this
// package carries no production code, only the test that holds the two
// together.
package podengine
