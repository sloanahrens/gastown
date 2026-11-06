// Package ci holds the tests that hold the text of the repository's CI
// workflows in deploy/ci. Those files are operator-protected where they land
// (.forgejo/workflows/ refuses an agent push that changes one), so the
// repository keeps the text here and the operator copies it in; a test here is
// the only place a rule of that text can go red before the copy.
package ci
