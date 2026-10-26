> Status: historical (2026-10). Merged in: gt-p85pw.60 (R6). Not maintained.

# R6: bd read verbs gt calls, for the shim

This lists the bd read verbs the gt code calls, with the flags it passes and the
source line on `origin/main`. The table is read from gt's source; it is not
checked against bd's help. Anything else is in the last section and is not
verified.

## Verified read verbs

| Verb | Flags gt passes | Source (origin/main) |
|---|---|---|
| `show <id>` | `--json` | `internal/beads/beads.go:3331` |
| `list` | `--json`, `--flat` | `internal/beads/beads.go:2267` |
| `list --id <id>` | `--include-infra --all --json --flat --no-pager` | `internal/beads/beads_agent.go:453` |
| `list --label=gt:agent` | `--include-infra --limit=0 --json --flat --no-pager` | `internal/beads/beads_agent.go:520` |
| `ready` | `--json` | `internal/beads/beads.go:3181`, `:3226`, `:3235` |
| `query <expr>` | `--json --all --limit=0` | `internal/beads/beads.go:2370`, `:2411` |
| `query <expr>` (no `--all`) | `--json` | `internal/beads/beads.go:2463` |
| `search <words>` | `--json --limit --status` | `internal/cmd/bead_search.go:116` |
| `blocked` | `--json` | `internal/beads/beads.go:3609` |
| `comments <id>` | `--json` | `internal/beads/beads.go:3906` |

## Not verified

- `show <id> --children` (source `internal/beads/beads.go:3416`, `:3437`).
- `dep list <id>` (source `internal/beads/beads.go:4171`, with `--direction=down --json`).
- Which verbs agent prompts and templates name, and how often.
- The flags agents pass, as opposed to the flags gt passes.
- Whether `count`, `stats`, `history`, `version` or `remember` are called by any
  agent or by gt; this document does not show it.
- `bd count` flags named in the identity docs.
- Any call count, from logs or transcripts; there is no bd usage ledger here.
- The behaviour of `query` expressions beyond the forms in the table.
- Whether the forms above behave the same on the tracker backend.
