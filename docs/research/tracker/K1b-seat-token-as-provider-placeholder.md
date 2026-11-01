> Status: historical (2026-10). Merged in: gt-5dkxp. Not maintained.

# K1b: the tracker seat token as a provider placeholder

Bead gt-5dkxp. Plan row K1 of the hosted-platform plan, second half. K1 (gt-rh8ak, 4bc7e2a2) staged the token file owner-read-only (0400). The goal here is stronger: the real token never enters a sandbox, as FORGEJO_TOKEN does not (`deploy/polecat/profiles/forgejo-polecat.yaml`: the sandbox holds a placeholder, the supervisor swaps in the real token on named paths and nowhere else).

Facts below are from `origin/main` at the time of writing. Anything the code cannot show is marked **not verified** and goes to the live proof in the last section.

## What exists

- The client sends `Authorization: Bearer <token>` (`tracker/client/client.go:317`). It reads the token from `TRACKER_TOKEN_FILE` or `TRACKER_TOKEN` (`client.go:69-72`), trims it, and refuses an empty file (`client.go:101-108`).
- The client accepts plain http only to a literal loopback IP or to `AllowHTTPHost` (`client.go:39`, `client.go:168`). The older note `docs/research/tracker/R7-seat-lifecycle.md` listed this as a blocker; `AllowHTTPHost` now answers it, so a sandbox can name the control-plane host.
- A token is issued by `POST /v1/tokens` and revoked by `POST /v1/tokens:revoke`, both admin-only (`tracker/server/routes_admin.go:26,28`). The actor a request acts as is the token's actor; `GET /v1/whoami` returns it (`tracker/server/server.go:100`).
- A sandbox attaches providers by name at create: `Profile.Providers []string` (`internal/session/backend.go:26`), passed through at `backend_openshell.go:990`. The list is per profile, not per seat.
- The forgejo provider holds one credential for the bot, not one per seat (`forgejo-polecat.yaml`, `credentials: api_token`).

## Question 1: a token per seat, or one provider token

Today the actor on every tracker write is the seat's identity, because each seat has its own token. A single shared provider token would make every seat the same actor: claims, comments and the By column would lose who did what, and revoking one seat would revoke all.

Options:

- **A. A provider per seat.** Create a provider instance at seat start with the seat's token, attach it to that sandbox, delete it at seat end. Keeps per-seat identity and per-seat revoke. Costs one provider create and one delete per seat, and a leak path (a crashed seat leaves a provider) that the reaper must sweep.
- **B. One shared provider token and a seat header.** The sandbox adds a seat id header; the tracker trusts it. A sandbox could then name any seat, so attribution becomes advisory and a hostile sandbox can act as another seat. Not acceptable once workspaces hold customer code.
- **C. Keep the 0400 file.** Done already. The token is still readable by anything running as the sandbox user, which the sandbox-secrets guard (`internal/cmd/tap_guard_sandbox_secrets.go`) tries to stop but cannot make impossible.

Recommendation: A, after the live proof shows the extra cost per seat is small (not verified: the time of `provider create` and `delete`).

## Question 2: does the substitution cover the tracker header

The forgejo profile declares `auth_style: header`, `header_name: authorization`, and git sends `Authorization: token <placeholder>`; the supervisor substitutes inside the header value. The tracker client sends `Bearer <token>`. The same mechanism should work with the placeholder in a `Bearer` value, but **not verified**: the profile docs of the supervisor are outside this repo and the only worked example is the `token` prefix. The live proof decides.

The client also reads the token from a file and trims it, so the file in the sandbox can hold the placeholder string and the client needs no change. The tap guard's tracker-token rules (`tap_guard_sandbox_secrets.go:1393`) judge the file path, so they still apply to the placeholder file.

## Question 3: which tracker paths to allow

Derived from the route registrations in `tracker/server/routes_*.go` and `server.go`. A seat needs the issue read and write routes, comments, dependencies, the board routes, claim and release, batch, `GET /v1/events` and `GET /v1/whoami`:

- reads: `GET /v1/issues/{id}`, `POST /v1/issues:show-many`, `:list`, `:count`, `POST /v1/assigned`, `POST /v1/ready`, `POST /v1/blocked`, `POST /v1/issues:children-of`, `GET /v1/events`, `GET /v1/whoami`, `GET /v1/issues/{id}/comments`, `GET /v1/issues/{id}/dependencies`
- writes: `POST /v1/issues`, `PATCH /v1/issues/{id}`, `POST /v1/issues:close`, `POST /v1/graphs`, `POST /v1/issues/{id}/claim`, `release`, `POST /v1/batch`, `POST /v1/issues/{id}/comments`, `POST /v1/issues/{id}/dependencies`, `DELETE /v1/issues/{id}/dependencies/{dependsOn}`. `:delete` and `transfer` are not seat writes: a bead delete is town-side, and no production seat calls transfer (gt-zbpf0).

Reassignment is still open through the allowed writes. `PATCH /v1/issues/{id}` and the batch `update` op both carry an `assignee` patch, and the store sets it on any bead another actor does not hold `in_progress` (`tracker/store/postgres_update.go`). A profile rule matches method and path, not the body, so removing `transfer` does not stop a seat from reassigning a bead. The server guard is gt-fxwh7.

The batch rule (`POST /v1/batch`, `tracker/api/batch.go`, `tracker/store/postgres_batch.go`) admits four ops: `create`, `update`, `close` and `comment`. There is no delete or transfer op, so the batch rule does not reopen `issues:delete` or `transfer`. Its `update` op runs the same path as PATCH, so it adds no reach beyond the reassignment gap above, and its `close` op runs with the request's `force` flag, the same as `POST /v1/issues:close`.

Refuse in the profile, in two groups.

- Admin-gated, read from the code: `POST /v1/tokens`, `POST /v1/tokens:revoke`, `PUT /v1/config/{key}` and `POST /v1/wisps:purge` are wrapped in `requireAdmin` (`tracker/server/routes_admin.go:25-28`), and `POST /v1/export` is too (`tracker/server/routes_export.go:16`). The profile refuses them as defence in depth: a seat token that was ever issued with admin scope still could not reach them through the sandbox.
- Not seat routes by assumption, **not verified**: `GET /v1/stats`, `GET /v1/config/{key}`, `GET /v1/snapshot/town` and `POST /v1/board/dispatch` are not admin-gated in `routes_admin.go:23-24`, `routes_snapshot.go:12` and `routes_board.go:17`, so the server would answer a seat. I have not checked whether a polecat calls any of them today; a grep of the callers decides before the profile is written, and the first build bead below says so. Until then the allow list above does not include them.

## Build beads (filed held)

1. gt-0f305: the profile file `deploy/polecat/profiles/tracker-polecat.yaml` with the paths above, and the test that lints it (the pattern of `cmd/openshell-providers`): the test refuses any admin path in the allow list.
2. gt-2fj7e: the per-seat provider lifecycle behind an interface with a fake runner: create at seat start, delete at seat end, sweep of orphans by the reaper.
3. gt-874y8: the sandbox stages a placeholder in the token file instead of the real token, and the guard test proves the real token appears nowhere in the sandbox environment or staged files.

## Live proof owed (overseer, one-shot, by hand)

On one sandbox: import the profile, create a provider with a throwaway token, attach it, and call `GET /v1/whoami` through the sandbox. Check that (a) the tracker sees the real token and answers with the seat actor, (b) the sandbox never held it, (c) the time of provider create and delete, (d) a refused path returns the supervisor's refusal.
