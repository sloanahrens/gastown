> Status: implementing on crew/sloan/gt-tail (gt-s3rec.6). Historical once merged; not maintained.

# gt tail: the one operator stream (gt-s3rec.6) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans with superpowers:test-driven-development. Steps use checkbox (`- [ ]`) syntax. Every task starts with a failing test.

**Goal:** `gt tail` prints one time-ordered plain-text stream merging the bd events journal of every rig, the per-rig landings files and daemon.log, optionally following all three.

**Architecture:** Three read-only sources behind one small interface; each `Poll()` returns the lines that appeared since its last poll (the first poll honors `--since`). A merger sorts each batch by time (stable, ties in source order) and prints `<local RFC3339 time> <rig> <kind> <text>`. Read failures and the journal-off notice are lines in the stream, never an exit status: gt tail computes no verdict.

**Tech Stack:** Go, cobra, `beads.Admin.EventsTail`/`ConfigGet` (bd events tail under BD_MACHINE=1), `compress/gzip` for lumberjack backups.

**Spec:** bead gt-s3rec.6; epic gt-s3rec; D8/D7 decisions (claude-1ey.8 comments). Cursor semantics: `docs/plans/2026-09-29-w2-client-plan.md` (Task 4, Findings 1-2). Landings contract: crew/sloan/d2-land `internal/land/landings.go` and `note.go` (`LandingRecord`), plan `docs/plans/2026-09-30-d2-land-plan.md` Global Constraints.

## Global Constraints

- Reads only: no bd mutation, no verdict, no health computation (that is `gt status`, gt-s3rec.2). Exit 0 unless flags are invalid or the town cannot be found.
- Plain text: no curses, no colors.
- Every line: `<local time, RFC3339 with numeric zone> <rig> <kind> <text>`, one line per record, no embedded newlines or control characters.
- Do not delete the feed/TUI (gt-638go.2). No townhealth source (gt-s3rec.2 adds it).
- Unit tests never reach the bd on PATH, the live daemon.log or the town.
- No AI attribution in commits.

## Findings that shape the plan

1. **The events cursor is a seq, not a time.** `--since <dur|ts>` for events reads the journal from seq 0 in pages and keeps records whose `ts` is at or after the cutoff; follow then polls from `NextSince`. A pruned-past read (`*EventsTruncatedError`) resumes at `Floor-1` and prints one line naming the gap.
2. **The journal is off in production.** `bd config get events-journal` answers `false` for every store today. gt's own bd calls journal anyway (`BD_EVENTS_JOURNAL=1`, w2-client), so the journal is partial, not empty. Decision: print one line per rig (`journal off in config (events-journal=false): only mutations made through gt are journaled`) and keep reading the journal; a rig whose read fails prints one line and the other sources continue.
3. **The installed bd predates machine mode** (empty output, JSON lines). `EventsTail` already reads both.
4. **Landings contract (d2-land code, ca8f8046):** path `<town>/.runtime/landings/<rig>.jsonl`, one JSON object per line: `bead, rig, branch, head, target, base, landed_commit, patch_id, gate_result, om_verdict, om_score, route, landed_at` (RFC3339). No actor field. The plan's promised "Landings file contract" section had not appeared when this plan was written; the reader is coded against the struct tags on d2-land and a test pins them.
5. **daemon.log** is written by `log.LstdFlags` (`2006/01/02 15:04:05`, local, no zone) through lumberjack (100MB, 3 backups, `daemon-<UTC rotation time>.log[.gz]`). A backup holds lines older than its rotation time, so `--since` reads the backups rotated at or after the cutoff, oldest first, then daemon.log. A line with no timestamp prefix inherits the previous line's time.

## File map

- `internal/landings/landings.go` (new): `Record`, `Path(townRoot, rig)`, `Reader` (offset-tailing, partial-line safe, truncation resets).
- `internal/landings/landings_test.go` (new).
- `internal/cmd/tail.go` (new): cobra verb, flags, `tailLine`, `tailSource`, merge + render.
- `internal/cmd/tail_sources.go` (new): events, landings, daemon sources.
- `internal/cmd/tail_test.go` (new): flag parsing, per-source tests, golden merged stream (`internal/cmd/testdata/tail_golden.txt`).

---

### Task 1: landings reader package

**Produces:**
```go
type Record struct { Bead, Rig, Branch, Head, Target, Base, LandedCommit, PatchID, GateResult, OMVerdict, Route string; OMScore float64; LandedAt time.Time } // json tags as d2-land
func Path(townRoot, rig string) (string, error)
type Reader struct { Path string /* unexported offset, partial */ }
func (r *Reader) ReadNew() (recs []Record, bad []string, err error) // missing file = nothing, not an error
```
- [ ] Failing tests: JSON tags round-trip a d2-land line byte-for-byte field set; two appends read as two records, a third after an append reads only the new one; a partial trailing line is held until its newline; a truncated/replaced file (size < offset) rereads from 0; a malformed line is returned in `bad` and reading continues; missing file returns nothing; invalid rig names refused like `RigLandingsFile`.
- [ ] Implement; commit `feat(landings): reader for the per-rig landings file`.

### Task 2: line model, merge and render

**Produces:**
```go
type tailLine struct { At time.Time; Rig, Kind, Text string }
type tailSource interface { Poll() []tailLine }
func mergeTail(batches ...[]tailLine) []tailLine // stable by At, ties keep source order
func renderTailLine(l tailLine, loc *time.Location) string
func parseTailSince(s string, now time.Time, loc *time.Location) (time.Time, error)
func parseTailKinds(s string) (map[string]bool, error)
```
- [ ] Failing tests: merge ordering and stability; render sanitizes newlines/control chars and empty rig to `-`; `--since` accepts `15m`, `2h`, `1d`, RFC3339, `2006-01-02T15:04:05` and `2006-01-02 15:04` local, `2006-01-02`; rejects junk; `--kind` accepts subsets, rejects unknown kinds and empty.
- [ ] Commit `feat(tail): line model, merge and render`.

### Task 3: sources

- **events** (`eventsSource{rig, journal tailJournal, cutoff, since int64, started bool, lastErr string}` with `tailJournal interface{ EventsTail(int64,int) (*beads.EventsPage, error); ConfigGet(string) (string, error) }`): first poll reads config once (off/unreadable = one line), pages from 0 with limit 500 until `!More`, filters by ts; later polls from cursor. Truncation resumes at `Floor-1` (or head) with one line. Read errors print once per distinct message. Text: `<op> <issue> status=<s> actor=<a> seq=<n>`. Unparseable ts uses the poll time and appends `ts=<raw>`.
- **landings** (`landingsSource{rig, reader, cutoff}`): text `landed <bead> <branch> -> <target> commit=<12> patch=<12> gate=<g> om=<verdict>/<score> route=<r>`; bad lines as `unreadable landings line: <line>`.
- **daemon** (`daemonSource{dir, cutoff, offset, lastAt, rigFilter}`): first poll reads qualifying backups (gz or plain) then daemon.log to EOF; later polls from offset; size < offset resets to 0 with one line `daemon.log rotated`. Rig column `town`; with `--rig`, only lines naming the rig as a word.
- [ ] Failing tests with a fake journal (off config, paging, truncation, error dedupe, cutoff), temp landings files, temp daemon dir with a gz backup, continuation lines and rotation.
- [ ] Commit `feat(tail): events, landings and daemon sources`.

### Task 4: the verb

`gt tail [--rig <name>] [--since <dur|ts>] [--follow] [--kind events,landings,daemon] [--interval 3s]`, GroupDiag. Rigs = `hq` + the rig registry; `--rig` must be one of them. `--since` default `15m`. Without `--follow`: one poll of every source, merged, printed, exit 0. With `--follow`: that, then poll every interval until SIGINT/SIGTERM. Events client: `beads.NewWithBeadsDir(townRoot, doltserver.FindRigBeadsDir(townRoot, rig))`; a rig with no beads dir prints one line.
- [ ] Failing tests: golden merged stream from fake sources (`testdata/tail_golden.txt`) via `runTailWith`; follow loop prints the second batch after the first and stops on context cancel; unknown `--rig` errors.
- [ ] Commit `feat(tail): gt tail, the one operator stream`.

### Task 5: gates and review

- [ ] `make lint`, `go build ./...`, `go test ./internal/landings/ ./internal/cmd/ -run 'Tail|CommandTokens'`, then `gt slot run -- make test`, all by exit code, wall time recorded.
- [ ] `om review -base origin/main` at the midpoint (after Task 3) and the end; fix blockers/majors.
- [ ] Attribution grep empty; `git push origin crew/sloan/gt-tail`.

## Execution notes
