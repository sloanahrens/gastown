# Fork-Based Rig Setup

When you run a rig against a repository you **don't own**, the rig has to
fetch canonical history from upstream but push all work to your fork.
`gt rig add` supports this directly through `--push-url` and
`--upstream-url`. Without them, the default `gt rig add <name> <fork-url>`
makes `origin` push to the upstream you cannot write to.

## When you need fork mode

Use fork mode whenever:

- You have read-only access to the canonical repo (e.g. you're an external
  contributor), and
- You push your work to a personal/organisation fork and open PRs from there.

If you own the canonical repo and push directly to it, you do **not** need
these flags — the plain `gt rig add <name> <git-url>` is correct.

## Setup

```bash
gt rig add <name> <upstream-url> \
  --push-url     <your-fork-url> \
  --upstream-url <upstream-url>
```

Concretely, for a Gas Town contributor:

```bash
gt rig add gastown https://github.com/gastownhall/gastown \
  --push-url     https://github.com/<you>/gastown \
  --upstream-url https://github.com/gastownhall/gastown
```

What each flag does:

| Flag | Effect |
|---|---|
| positional `<git-url>` | `origin`'s **fetch** URL — where canonical history is pulled from |
| `--push-url` | `origin`'s **push** URL — where all pushes go (your fork) |
| `--upstream-url` | Adds a separate named `upstream` remote for rebases against `upstream/main` |

The flags configure these remotes on the rig's bare clone, `<rig>/.repo.git`.

## Verifying the setup

Check the remotes in the rig's bare clone:

```bash
git -C <town>/<rig>/.repo.git remote -v
```

Expect (substituting your fork and the canonical repo):

```
origin    https://github.com/gastownhall/gastown (fetch)
origin    https://github.com/<you>/gastown       (push)
upstream  https://github.com/gastownhall/gastown (fetch)
upstream  https://github.com/gastownhall/gastown (push)
```

The key invariant: **`origin`'s fetch URL is upstream, `origin`'s push URL
is your fork.** If `origin (push)` points at the canonical repo, the flags
did not take effect — re-add the rig.

## Landing

A rig added with `--upstream-url` is a fork rig. `gt prime` tells its
polecats to push branches to the fork and open a PR against upstream by hand
instead of using the landing worker for upstream changes.

The daemon landing worker lands a rig's work through Forgejo (CI, then om
review) and fails closed for a rig without a `merge_queue.forgejo` block in
`<rig>/settings/config.json`. Before you point a fork rig at that path, read
[the Forgejo runbook](../forgejo-runbook.md): it provisions the bots, cuts a
rig over and names the block's keys. Fork rigs follow the Forgejo-primary
landing path; this guide does not cover that setup.

## See also

- [Local Rig Bootstrap](local-rig-bootstrap.md) — read it to set up a rig from
  a local or private repo
