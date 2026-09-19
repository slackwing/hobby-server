# Shared auth — integration guide for new websites

How to give any new andrewcheong.com website login, users, roles,
invite / password-reset links, and templated email WITHOUT building
any of it. One account per person works across all sites. Read this
before wiring a new project; the implementation lives in
`internal/shared/` (backend), `internal/mailer/` (SMTP), and feathers
`html/admin/` (console, default pages, shared page machinery).

## The model

- **One user table for everything**: `hobby_server_user`
  (`username` = immutable id, e.g. `andrew`; `display_name`;
  `initial` 1–2 chars and `color` #rrggbb for the circular initials
  avatar; `email` optional; `active_site` — the website the console
  acts on for this person (invite/reset links and emails go to that
  site's pages and templates); `password_hash` argon2id, NULL until
  set via an invite/reset link). All profile fields are set by the
  admin in the console — there is no user-facing profile page yet.
  A new user gets a random avatar colour (random hue, fixed
  saturation/lightness — `shared.RandomColor`, mirrored in the console
  JS) and initials from the first two words of the display name.
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
  email, active_site, roles: [{website, role}]}`
- `POST /admin/api/logout`
- `GET /admin/api/me` → me-payload; 401 when logged out
- `GET /admin/api/token-info?code=...` → `{username, display_name,
  website, kind, expires_at}` for a valid code; `kind` is `invite` or
  `reset`
- `POST /admin/api/set-password` `{code, password}` → sets password,
  consumes code, logs the user in (returns the me-payload). For an
  invite code, the site's `"on": "accept"` email (account created) is
  sent afterwards; a reset code sends nothing.
- `POST /admin/api/forgot` `{username}` → always 204. If the account
  has an email and its active site has a reset template, the reset
  email goes out. One request per username per minute.

Admin-only (requires role `admin` on website `admin`):
- `GET /admin/api/users` → `[{username, display_name, initial, color,
  email, active_site, has_password, created_at, roles}]`
- `POST /admin/api/users` `{username, display_name, initial?, color?,
  email?, active_site?}` — missing initial = first letters of the first
  two words; missing colour = random
- `PATCH /admin/api/users/{username}` any of `{display_name, initial,
  color, email, active_site}` (`""` clears email / active_site)
- `DELETE /admin/api/users/{username}` (cascades roles, sessions, and
  pending links; self-delete refused)
- `GET /admin/api/websites`
- `POST/DELETE /admin/api/roles` `{username, website, role}`
- `POST /admin/api/links` `{username, type: "invite" | "reset",
  website?}` → `{url, expires_at}`. `website` defaults to the user's
  active site (invite needs one; reset falls back to the default
  page). Invite 7 days, reset 1 hour.
- `GET /admin/api/email-status` → `{configured, from}`
- `POST /admin/api/email` `{username, template, website?}` → renders
  the site's `_email/<template>.html` for that user and sends it;
  `website` defaults to the active site. Returns `{to, subject,
  template, url?, expires_at?}` (the minted link for invite/reset
  templates). 503 when email is not configured, 400 when the user has
  no email, 404 for an unknown template.

Validation: initial 1–2 non-blank chars; colour `#rrggbb`; email a
bare address (`name@host`), max 254; active_site must be a registered
website.

## Cross-project standard paths (the underscore convention)

Files and directories that every website provides in the SAME place,
because the shared system (console, server) reaches into them by
convention, are named with a leading underscore:

- `/<website>/_invite/` — the site's invite page (choose a password).
- `/<website>/_reset/` — the site's password-reset page.
- `/<website>/_email/` — the site's email templates (manifest, layout,
  bodies) and an admin-only preview page.

**Skins over shared machinery.** The set-password pages are two thin
skins over one script, `/admin/assets/setpw.js`: the skin supplies the
markup and styling and tags its elements with `data-pw` hooks (`form`,
`username`, `password`, `submit`, `msg`, `nocode`, `invalid`, `done`,
`name`, `enter`); the script reads `?code=`, checks it with
`token-info` (the code's kind must match the page — an invite code is
refused on `/_reset/`), fills the username, submits `set-password`, and
switches the states. Opened without a code the page renders with the
form disabled — a code is the only way in.

**Default pages.** `/admin/_invite/` and `/admin/_reset/` are plain
versions built on the same script. The server checks (HEAD, cached
10 min) whether a site has its own `/<site>/_invite/` or `/_reset/`
page and points links at the default page when it doesn't, so a new
site needs no skin to work. The admin console itself uses
`/admin/_reset/`.

Add new cross-project files with the same prefix (`_something`), and
document them here.

## Invite / reset links

One-time codes carrying a kind (`invite` | `reset`) and a website; DB
stores only the SHA-256. Invite links (`/<site>/_invite/?code=...`,
7d) and reset links (`/<site>/_reset/?code=...`, 1h) both set a
password for the username baked into the code; only an invite fires
the account-created email. Generate links (copied to the clipboard) or
send them by email from the `/admin/` console; users can request a
reset themselves via "Forgot password?" on a site's logon (which posts
to `/admin/api/forgot`).

The invite page must make it obvious the invitee CHOOSES a password
right there — say "password", not "passphrase".

## Email templates (`/<website>/_email/`)

Templates are static files in the website's own directory of the
feathers repo, so they ship, version, and re-theme together with the
site. The server never stores them; it fetches them over HTTP at send
time from the public site (or `email.site_base_url` in dev).

- `templates.json` — manifest:
  ```json
  { "layout": "_layout.html",
    "templates": [
      { "id": "invite", "name": "Invite", "subject": "You are summoned",
        "title": "Summons", "kicker": "HUNTER ASSOCIATION · OFFICIAL SUMMONS",
        "heading": "HUNTER×HALLOWEEN", "invite": true },
      { "id": "account-created", "name": "Account Created", "subject": "…",
        "title": "Account", "kicker": "…", "heading": "ACCOUNT CREATED", "on": "accept" },
      { "id": "reset", "name": "Password Reset", "subject": "…",
        "title": "Account", "kicker": "…", "heading": "PASSWORD RESET", "reset": true }
    ] }
  ```
  `invite: true` mints a fresh 7-day invite link per send
  (`invite_url`); `reset: true` a 1-hour reset link (`reset_url`);
  `on: "accept"` sends the template automatically when an invite for
  this site is accepted. `subject`, `title`, `kicker`, `heading` are
  raw (heading may carry HTML).
- `_layout.html` — the ONE shared frame (title bar, kicker, heading,
  footer, container) with slots `{{title}}`, `{{kicker}}`,
  `{{heading}}`, `{{body}}` (plus `{{subject}}`, `{{year}}`, any
  variable). Change the frame here, once, for every email. Set
  `"layout": ""` in the manifest to opt out.
- `<id>.html` — the body fragment that goes into `{{body}}`.
  Email-safe HTML: tables, inline styles, no external CSS/fonts (web
  fonts don't survive Gmail).
- `index.html` — admin-only preview page: tabs for the manifest, the
  rendered email in an inert frame, "send test to me". It links the
  administrative base stylesheet `/admin/assets/style.css` (the same
  one the console and the default pages use) rather than the site's
  theme, so everything an admin operates looks like one tool and the
  themed email in the frame is obviously the email. Honour `?template=<id>` and `?user=<username>` (the console's
  Preview button opens `/<site>/_email/?template=…&user=…`; previewing
  as another user needs the console role and falls back to yourself).

Variables, substituted as `{{name}}` (HTML-escaped in bodies and the
layout except the four slots, raw in the manifest strings; unknown
names become empty): `display_name`, `username`, `initial`, `color`,
`color_text` (ink or cream, whichever reads on `color`), `email`,
`website`, `site_url` (`<base>/<website>/`), `base_url`, `invite_url`,
`reset_url`, `expires_at` (invite: "Fri, Sep 25"; reset: "Fri, Sep 18
at 5:20 PM EDT" — America/New_York), `year`. The server's substitution
lives in `internal/shared/email.go`; the preview page reimplements it
in JS — keep the variable list identical in both when extending it.

Sending needs the server-wide `email:` block in
`~/.config/hobby-server/config.yaml` (see `config.example.yaml`; any
STARTTLS submission host, e.g. Gmail with an app password). Without
it the console says "email not configured" and sends 503.

## Frontend integration recipe (what hxh does)

1. On page load `fetch("/admin/api/me")`. 401 → show a login form
   that POSTs to `/admin/api/login` (with a "Forgot password?" that
   POSTs `/admin/api/forgot`). 200 → show the site; gate admin-only UI
   on `roles` containing `{website: "<site>", role: "admin"}`.
   (Client-side gating is UX, not security — protect DATA in APIs.)
   Use `initial`/`color` for the user's avatar if the site shows one.
2. Optionally skin `_invite/` and `_reset/`: copy `html/hxh/_invite/`,
   `html/hxh/_reset/` (feathers) and restyle — hxh's markup lives in
   its `SetPassword` app (`html/hxh/apps/setpw.js`), a plain page can
   inline the same `data-pw`-tagged form; keep the `data-pw` hooks and
   load `/admin/assets/setpw.js`. Without a skin, links use the default
   pages.
3. Copy `html/hxh/_email/` (manifest, `_layout.html`, `invite.html`,
   `account-created.html`, `reset.html`, `index.html`) and reskin. Keep
   the ids and the `invite` / `reset` / `on: accept` flags.
4. Register the website (below) and set users' active site to it in
   the console.
5. Server-side, gate project endpoints with the shared store — see
   `internal/hxh/hxh.go` `requireHxhAdmin` (resolve `hobby_session`
   cookie via `shared.Store.GetSession`, then `HasRole(user, site, role)`).

## Registering a new website

1. In the `/admin/` console DB (or a liquibase changeset under
   `liquibase/admin/`): add a row to `hobby_server_websites` and its
   roles to `hobby_server_website_roles`. (No console UI for this yet
   — changeset or psql.)
2. Grant roles to users in the console; set their active site; send
   invites.
3. If the site needs its own backend data, add a hobby-server project
   per `ARCHITECTURE.md` §7 — tables prefixed `<project>_` (AGENTS.md
   N7) — and gate handlers via the shared store as above.

## Ops notes

- Argon2id (OWASP params: 19 MiB, t=2, p=1, 16-byte salt) via
  `internal/shared/password.go`; min password length 8, no composition
  rules — by design, don't add any. The invite email's footnote
  describes exactly this; keep them in sync.
- Never log codes, passwords, hashes, or the SMTP password (AGENTS.md
  N4). The mailer scrubs its password from SMTP error strings.
- Bootstrapping a user who can't log in: insert a token row directly
  (see CLAUDE.md "Querying the prod DB"): token = 32 random bytes
  base64url; store `sha256hex(token)` in `hobby_server_password_token`
  with username/website/kind='reset'/expiry; hand the user
  `/admin/_reset/?code=<token>`.
- Local end-to-end testing: a throwaway Postgres (`docker run
  postgres:16-alpine` on a spare port) + the admin Liquibase changelog
  + a dev config with `email.smtp_host: 127.0.0.1`, `smtp_port: 1025`
  (any SMTP sink) and `site_base_url` pointing at a local static
  server that serves the feathers `html/` tree and proxies
  `/admin/api/*` to the Go server.

### Presence and activation (2026-09-18)

`hobby_server_user` also carries `last_seen_at` — bumped by every
authenticated request through `Store.GetSession`, on any website, at
most once per 20 s per user — and `activated_at`, set the first time
an account gets a password (`ConsumeToken`). hxh's chat derives
presence tiers from the former and message visibility from the latter
(`Store.ListMembers(website)`, `IsMember`, `TouchLastSeen`). Other
sites may read them the same way; nothing else writes them.
