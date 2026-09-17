# Shared auth — integration guide for new websites

How to give any new andrewcheong.com website login, users, roles,
invite links, and templated email WITHOUT building any of it. One
account per person works across all sites. Read this before wiring a
new project; the implementation lives in `internal/shared/` (backend),
`internal/mailer/` (SMTP), and feathers `html/admin/` (console).

## The model

- **One user table for everything**: `hobby_server_user`
  (`username` = immutable id, e.g. `andrew`; `display_name`;
  `initial` 1–3 chars and `color` #rrggbb for the circular initials
  avatar; `email` optional; `password_hash` argon2id, NULL until set
  via an invite/reset link). All profile fields are set by the admin
  in the console — there is no user-facing profile page yet. A new
  user gets a random avatar colour (random hue, fixed
  saturation/lightness — `shared.RandomColor`, mirrored in the console
  JS) which the admin can change with a colour picker.
- **Roles are per-website**: `hobby_server_user_roles` holds
  (username, website, role) string triplets. What a role MEANS is up
  to each site. Available roles per site: `hobby_server_website_roles`;
  registered sites: `hobby_server_websites`.
- **One SSO session**: cookie `hobby_session`, `Path=/`, HttpOnly,
  30-day sliding. Log in on any site → logged in on all. A site
  decides access by looking at the user's roles, not by having its own
  login.
- All tables live in the shared `hobby_server` database, schema in
  `liquibase/admin/changelog/`.

## HTTP API (public path `/admin/api/*` → backend `/api/admin/*`)

Public:
- `POST /admin/api/login` `{username, password}` → sets cookie,
  returns the me-payload `{username, display_name, initial, color,
  email, roles: [{website, role}]}`
- `POST /admin/api/logout`
- `GET /admin/api/me` → me-payload; 401 when logged out
- `GET /admin/api/token-info?code=...` → `{username, display_name,
  website, expires_at}` for a valid invite/reset code
- `POST /admin/api/set-password` `{code, password}` → sets password,
  consumes code, logs the user in (returns the me-payload). If the
  code was an invite and the site has an `"on": "accept"` email
  template, it is sent afterwards (best effort).

Logged-in:
- `POST /admin/api/password` `{password}` → replaces the session
  user's password, no link needed (what `/<site>/_invite/` does when
  opened without a code).

Admin-only (requires role `admin` on website `admin`):
- `GET /admin/api/users` → `[{username, display_name, initial, color,
  email, has_password, created_at, roles}]`
- `POST /admin/api/users` `{username, display_name, initial?, color?,
  email?}` — missing initial = first letter of display name; missing
  colour = random
- `PATCH /admin/api/users/{username}` any of `{display_name, initial,
  color, email}` (`email: ""` clears it)
- `DELETE /admin/api/users/{username}` (cascades roles, sessions, and
  pending links; self-delete refused)
- `GET /admin/api/websites`
- `POST/DELETE /admin/api/roles` `{username, website, role}`
- `POST /admin/api/links` `{username, type: "reset"}` or
  `{username, type: "invite", website: "<site>"}` → `{url, expires_at}`
- `GET /admin/api/email-status` → `{configured, from}`
- `POST /admin/api/email` `{username, website, template}` → renders
  the site's `_email/<template>.html` for that user and sends it;
  returns `{to, subject, template, invite_url?, expires_at?}`. 503 when
  email is not configured, 400 when the user has no email, 404 for an
  unknown template.

Validation: initial 1–3 non-blank chars; colour `#rrggbb`; email a
bare address (`name@host`), max 254.

## Cross-project standard paths (the underscore convention)

Files and directories that every website provides in the SAME place,
because the shared system (console, server) reaches into them by
convention, are named with a leading underscore. Today:

- `/<website>/_invite/` — the site's invite/set-password page.
  Invite links point here (`?code=...`).
- `/<website>/_email/` — the site's email templates (manifest +
  bodies), fetched by the server when the console sends mail, and an
  admin-only preview page.

Add new cross-project files with the same prefix (`_something`), and
document them here.

## Invite / reset links

One-time codes; DB stores only the SHA-256. Reset links
(`/admin/reset.html?code=...`, 1h TTL) and invite links
(`/<website>/_invite/?code=...`, 7d TTL) are functionally identical —
set a password for the username baked into the code — but invites
are skinned per site. Re-inviting an existing user who forgot their
password is the intended recovery path. Generate links (or send them
by email) in the `/admin/` console.

The invite page must make it obvious the invitee CHOOSES a password
right there (nobody sends them one) — say "password", not
"passphrase". Opened WITHOUT a code by a logged-in user, it shows the
same form as a "change password" for that user (POST
`/admin/api/password`). It should honour `?preview=invite|change|void`
(forced state, sample data, no network side effects) so the site can
ship `/<website>/_invite/preview.html`: an admin-only previewer in the
plain `/admin/` console style showing all three states in an inert
frame — the same pattern as `_email/index.html`.

## Email templates (`/<website>/_email/`)

Templates are static files in the website's own directory of the
feathers repo, so they ship, version, and re-theme together with the
site. The server never stores them; it fetches them over HTTP at send
time from the public site (or `email.site_base_url` in dev).

- `templates.json` — manifest:
  ```json
  { "templates": [
    { "id": "invite",  "name": "Invite",  "subject": "You are summoned", "invite": true },
    { "id": "welcome", "name": "Welcome", "subject": "Welcome, {{display_name}}", "on": "accept" }
  ] }
  ```
  `invite: true` mints a fresh 7-day invite link per send (exposed as
  `invite_url` / `expires_at`). `on: "accept"` sends the template
  automatically when an invite for this site is accepted.
- `<id>.html` — the body. Email-safe HTML: tables, inline styles, no
  external CSS/fonts (web fonts don't survive Gmail). Match the site's
  theme by hand.
- `index.html` — admin-only preview page: lists the manifest, renders
  each template with the logged-in admin's own values as samples in an
  inert frame, and has a "send test to me" button
  (`POST /admin/api/email`). Style it like the plain `/admin/` console
  (link `/admin/assets/style.css`), NOT like the site, so the themed
  email in the frame is obviously the email.

Variables, substituted as `{{name}}` (HTML-escaped in bodies, raw in
subjects; unknown names become empty): `display_name`, `username`,
`initial`, `color`, `color_text` (ink or cream, whichever reads on
`color`), `email`, `website`, `site_url` (`<base>/<website>/`),
`base_url`, `invite_url`, `expires_at` (e.g. "Thu, Sep 24"; both only
for invite templates), `year`. The server's substitution lives in
`internal/shared/email.go`; the preview page reimplements it in JS —
keep the variable list identical in both when extending it.

Sending needs the server-wide `email:` block in
`~/.config/hobby-server/config.yaml` (see `config.example.yaml`; any
STARTTLS submission host, e.g. Gmail with an app password). Without
it the console shows "email not configured" and sends 503.

## Frontend integration recipe (what hxh does)

1. On page load `fetch("/admin/api/me")`. 401 → show a login form
   that POSTs to `/admin/api/login`. 200 → show the site; gate
   admin-only UI on `roles` containing `{website: "<site>", role: "admin"}`.
   (Client-side gating is UX, not security — protect DATA in APIs.)
   Use `initial`/`color` for the user's avatar if the site shows one.
2. Copy `html/hxh/_invite/index.html` (feathers repo) and reskin it
   for the new site. Keep: readonly `username` field
   (autocomplete=username), `new-password` field, `token-info` prefill,
   `set-password` POST, the no-code "change password" mode, and copy
   that says the user picks the password.
3. Copy `html/hxh/_email/` (manifest, `invite.html`, `welcome.html`,
   `index.html`) and reskin. Keep the ids `invite` and `welcome`.
4. Server-side, gate project endpoints with the shared store — see
   `internal/hxh/hxh.go` `requireHxhAdmin` (resolve `hobby_session`
   cookie via `shared.Store.GetSession`, then `HasRole(user, site, role)`).

## Registering a new website

1. In the `/admin/` console DB (or a liquibase changeset under
   `liquibase/admin/`): add a row to `hobby_server_websites` and its
   roles to `hobby_server_website_roles`. (No console UI for this yet
   — changeset or psql.)
2. Grant roles to users in the console; send invite links or emails.
3. If the site needs its own backend data, add a hobby-server project
   per `ARCHITECTURE.md` §7 — tables prefixed `<project>_` (AGENTS.md
   N7) — and gate handlers via the shared store as above.

## Ops notes

- Argon2id (OWASP params) via `internal/shared/password.go`; min
  password length 8, no composition rules — by design, don't add any.
- Never log codes, passwords, hashes, or the SMTP password (AGENTS.md
  N4). The mailer scrubs its password from SMTP error strings.
- Bootstrapping a user who can't log in: insert a token row directly
  (see CLAUDE.md "Querying the prod DB"): token = 32 random bytes
  base64url; store `sha256hex(token)` in `hobby_server_password_token`
  with username/website/expiry; hand the user
  `/admin/reset.html?code=<token>`.
- Local end-to-end testing: a throwaway Postgres (`docker run
  postgres:16-alpine` on a spare port) + the admin Liquibase changelog
  + a dev config with `email.smtp_host: 127.0.0.1`, `smtp_port: 1025`
  (any SMTP sink) and `site_base_url` pointing at a local static
  server that serves the feathers `html/` tree.
