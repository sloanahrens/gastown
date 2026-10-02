# Formula Package

Gastown's shipped formulas and the town overlay dir. bd is the one formula
engine (D6, gt-fd2cu): it parses, resolves and cooks every formula, and
gastown renders what `bd cook` returns. Nothing in gastown parses a formula.

## What lives here

- `formulas/*.formula.toml`: the formulas embedded in the gt binary. The
  binary is canonical: `gt formula sync` (and `gt doctor fix formulas`) writes them
  to `<town>/.beads/formulas`, checked by content hash (`embed.go`).
- `overlay.go`: the one overlay dir, `<town>/formula-overlays/<formula>.toml`,
  whose step overrides bd applies when it cooks.

Check a formula against bd's strict decoder with `bd formula lint
internal/formula/formulas`.

## Gastown fields

bd's strict decode rejects keys it does not know, so gastown's own fields
use the places it accepts:

- Step metadata: a workflow step's `metadata.target` (where `gt formula run`
  slings it) and `metadata.interactive` (keep it in the current session).
- Convoy formulas (`type = "convoy"`): the step with
  `metadata.convoy = "synthesis"` is the synthesis and every other step is a
  leg (`metadata.focus`, `metadata.agent`, `metadata.review_only`). The vars
  `base_prompt`, `output_directory`, `output_leg_pattern`, `output_synthesis`
  and `review_only` are the run settings; `--set` overrides any of them.
- There is no formula-level `agent` (bd rejects the top-level key): set
  `metadata.agent` on a leg, or pass `gt formula run --agent`, which also
  applies to workflow steps.

```toml
formula = "design"
type = "convoy"
version = 1

[vars.problem]
description = "The design problem"
required = true

[vars.base_prompt]
default = """Analyze {{.problem}} for {{.leg.focus}}."""

[vars.output_directory]
default = ".designs/{{.review_id}}"

[[steps]]
id = "api"
title = "API Design"
metadata.focus = "Interface design"
description = "..."

[[steps]]
id = "synthesis"
metadata.convoy = "synthesis"
title = "Design Synthesis"
description = "Combine {{.output.directory}}/*.md"
depends_on = ["api"]
```

## Testing

Unit tests read the embedded files as text. Real-bd checks live in
`TestIntegrationFormulaCook*` (here and in `internal/cmd`), which cook every
shipped formula.
