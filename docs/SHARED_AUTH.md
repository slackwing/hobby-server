# Shared auth — integration guide for new websites

How to give any new andrewcheong.com website login, users, and roles
WITHOUT building auth. One account per person works across all sites.
Read this before wiring a new project; the implementation lives in
`internal/shared/` (backend) and feathers `html/admin/` (console).

## The model

- **One user table for everything**: `hobby_server_user`
  (`username` = immutable id, e.g. `andrew`; `display_name`;
  `password_hash` argon2id, NULL until set via an invite/reset link).
- **Roles are per-website**: `hobby_server_user_roles` holds
  (username, website, role) string triplets. What a role MEANS is up
  to each site. Available roles per site: `hobby_server_website_roles`;
  registered sites: `hobby_server_websites`.
- **One SSO session**: cookie `hobby_session`, `Path=/`, HttpOnly,
  30-day sliding. Log in on any site → logged in on all. A site
  decides access by looking at the user's roles, not by having its own
  login.
- All four tables live in the shared `hobby_server` database, schema
  in `liquibase/admin/changelog/`.

## HTTP API (public path `/admin/api/*` → backend `/api/admin/*`)

Public:
- `POST /admin/api/login` `{username, password}` → sets cookie,
  returns `{username, display_name, roles: [{website, role}]}`
- `POST /admin/api/logout`
- `GET /admin/api/me` → same payload as login; 401 when logged out
- `GET /admin/api/token-info?code=...` → `{username, display_name,
  website, expires_at}` for a valid invite/reset code
- `POST /admin/api/set-password` `{code, password}` → sets password,
  consumes code, logs the user in (returns the me-payload)

Admin-only (requires role `admin` on website `admin`):
- `GET/POST /admin/api/users`, `PATCH /admin/api/users/{username}`
- `GET /admin/api/websites`
- `POST/DELETE /admin/api/roles` `{username, website, role}`
- `POST /admin/api/links` `{username, type: "reset"}` or
  `{username, type: "invite", website: "<site>"}` → `{url, expires_at}`

## Invite / reset links

One-time codes; DB stores only the SHA-256. Reset links
(`/admin/reset.html?code=...`, 1h TTL) and invite links
(`/<website>/invite.html?code=...`, 7d TTL) are functionally identical
— set a password for the username baked into the code — but invites
are skinned per site. Re-inviting an existing user who forgot their
password is the intended recovery path. Generate links in the
`/admin/` console.

## Frontend integration recipe (what hxh does)

1. On page load `fetch("/admin/api/me")`. 401 → show a login form
   that POSTs to `/admin/api/login`. 200 → show the site; gate
   admin-only UI on `roles` containing `{website: "<site>", role: "admin"}`.
   (Client-side gating is UX, not security — protect DATA in APIs.)
2. Copy `html/hxh/invite.html` (feathers repo) and reskin it for the
   new site. Keep: readonly `username` field (autocomplete=username),
   `new-password` field, `token-info` prefill, `set-password` POST.
3. Server-side, gate project endpoints with the shared store — see
   `internal/hxh/hxh.go` `requireHxhAdmin` (resolve `hobby_session`
   cookie via `shared.Store.GetSession`, then `HasRole(user, site, role)`).

## Registering a new website

1. In the `/admin/` console DB (or a liquibase changeset under
   `liquibase/admin/`): add a row to `hobby_server_websites` and its
   roles to `hobby_server_website_roles`. (No console UI for this yet
   — changeset or psql.)
2. Grant roles to users in the console; send invite links.
3. If the site needs its own backend data, add a hobby-server project
   per `ARCHITECTURE.md` §7 — tables prefixed `<project>_` (AGENTS.md
   N7) — and gate handlers via the shared store as above.

## Ops notes

- Argon2id (OWASP params) via `internal/shared/password.go`; min
  password length 8, no composition rules — by design, don't add any.
- Never log codes, passwords, or hashes (AGENTS.md N4).
- Bootstrapping a user who can't log in: insert a token row directly
  (see CLAUDE.md "Querying the prod DB"): token = 32 random bytes
  base64url; store `sha256hex(token)` in `hobby_server_password_token`
  with username/website/expiry; hand the user
  `/admin/reset.html?code=<token>`.
