> Status: current research (2026-10-31), gt-5dkxp. Build beads filed held; live proof owed by the overseer.

# K1b: the tracker seat token as a provider placeholder

Bead gt-5dkxp. Plan row K1 of the hosted-platform plan, second half. K1 (gt-rh8ak, 4bc7e2a2) staged the token file owner-read-only (0400). The goal here is stronger: the real token never enters a sandbox, as FORGEJO_TOKEN does not (`deploy/polecat/profiles/forgejo-polecat.yaml`: the sandbox holds a placeholder, the supervisor swaps in the real token on named paths and nowhere else).

Facts below are from `origin/main` at the time of writing. Anything the code cannot show is marked **not verified** and goes to the live proof in the last section.

## What exists

- The client sends `Authorization: Bearer <token>` (`tracker/client/client.go:317`). It reads the token from `TRACKER_TOKEN_FILE` or `TRACKER_TOKEN` (`client.go:69-72`), trims it, and refuses an empty file (`client.go:101-108`).
- The client accepts plain http only to a literal loopback IP or to `AllowHTTPHost` (`client.go:39`, `client.go:168`). The older note `R7-seat-lifecycle.md` listed this as a blocker; `AllowHTTPHost` now answers it, so a sandbox can name the control-plane host.
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
- writes: `POST /v1/issues`, `PATCH /v1/issues/{id}`, `POST /v1/issues:close`, `POST /v1/issues:delete`, `POST /v1/graphs`, `POST /v1/issues/{id}/claim`, `release`, `transfer`, `POST /v1/batch`, `POST /v1/issues/{id}/comments`, `POST /v1/issues/{id}/dependencies`, `DELETE /v1/issues/{id}/dependencies/{dependsOn}`

Refuse in the profile (defence in depth; `requireAdmin` already refuses them for a non-admin token): `/v1/tokens`, `/v1/tokens:revoke`, `/v1/config/*`, `/v1/stats`, `/v1/wisps:purge`, `/v1/export`, `/v1/snapshot/town`, `/v1/board/dispatch`.

Not verified: whether a polecat calls `POST /v1/board/dispatch` or `/v1/snapshot/town` today; a grep of the callers decides before the profile is written.

## Build beads (filed held)

1. The profile file `deploy/polecat/profiles/tracker-polecat.yaml` with the paths above, and the test that lints it (the pattern of `cmd/openshell-providers`): the test refuses any admin path in the allow list.
2. The per-seat provider lifecycle behind an interface with a fake runner: create at seat start, delete at seat end, sweep of orphans by the reaper.
3. The sandbox stages a placeholder in the token file instead of the real token, and the guard test proves the real token appears nowhere in the sandbox environment or staged files.

## Live proof owed (overseer, one-shot, by hand)

On one sandbox: import the profile, create a provider with a throwaway token, attach it, and call `GET /v1/whoami` through the sandbox. Check that (a) the tracker sees the real token and answers with the seat actor, (b) the sandbox never held it, (c) the time of provider create and delete, (d) a refused path returns the supervisor's refusal.
