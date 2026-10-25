// Package tracker is the root of the town tracker module: a Postgres-backed
// issue service (trackerd), its Go client, and the contract suite both are
// held to. It imports nothing from gastown outside this module
// (boundary_test.go); gt reaches it through internal/beads/trackerclient.
package tracker
