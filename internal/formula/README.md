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
- There is no formula-level `agent` (bd rejects the top-level key): pass
  `gt formula run --agent`, which applies to workflow steps.

The convoy formula type is gone (gt-gzhin.5): bd still decodes
`type = "convoy"`, but no embedded formula uses it and `gt formula run`
dispatches workflow formulas only.

```toml
formula = "shiny"
type = "workflow"
version = 1

[vars.problem]
description = "The problem to work"
required = true

[[steps]]
id = "design"
title = "Design"
metadata.target = "mayor"
description = "..."

[[steps]]
id = "synthesis"
title = "Synthesis"
needs = ["design"]
description = "Combine the design docs"
```

## Testing

Unit tests read the embedded files as text. Real-bd checks live in
`TestIntegrationFormulaCook*` (here and in `internal/cmd`), which cook every
shipped formula.
