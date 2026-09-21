# hobby-server

A small **multi-project** auth + storage backend. One Go binary in
Docker, multiple isolated project namespaces (each with its own DB,
URL prefix, cookie scope, and Liquibase changelog).

**Pattern this mirrors:**
[`slackwing/manuscript-studio`](https://github.com/slackwing/manuscript-studio)
— Go binary in Docker, Liquibase schema, Apache reverse proxy, Cloud
SQL via Auth Proxy. When you don't know how to do something deploy- or
schema-related here, look at how manuscript-studio does it.

## Currently hosted projects

- **rv** — auth for the
  [feathers RV trip site](https://andrewcheong.com/rv). Frontend:
  [`slackwing/feathers`](https://github.com/slackwing/feathers) at
  `foundry/website/html/rv/` (locally:
  `~/src/feathers/foundry/website/html/rv/`).
- **hxh** — the
  [Hunter × Halloween party site](https://andrewcheong.com/hxh).
  Frontend: same feathers repo at `foundry/website/html/hxh/`.
  Own database (`hxh`) — unlike rv it does NOT live in the shared
  `hobby_server` DB, since every project's auth tables share the
  same `user`/`session` names. NOTE: hxh's per-project auth floor is
  unused — the site logs in via the shared **admin** system below.
  The `hxh` DB holds the character roster (`hxh_characters`, mirrored
  from feathers `html/hxh/roster.json`, the master; card fields since
  changeset 004 — see feathers `foundry/website/hxh-roster/CHARACTER.md`)
  and, since changeset 005 (2026-09-18), the **Beetle chat**:
  `hxh_chat_message` (kept forever; `deleted_at` is unused since unsend was dropped) and
  `hxh_chat_profile` (JSON runs). Realtime is a WebSocket hub
  (`internal/hxh/hub.go`, one per process) at `/api/hxh/chat/ws`,
  mounted OUTSIDE the request timeout; Apache proxies that path via
  `mod_proxy_wstunnel`. Any role on hxh may chat. Spec: feathers
  `foundry/website/docs/HXH_CHAT.md`; frontend `html/hxh/apps/chat/`.
- **bap** — the
  [bongo-cat table-bap site](https://andrewcheong.com/bap).
  Frontend: same feathers repo at `foundry/website/html/bap/`.
  Own database (`bap`), one table (`bap_cup_state`). Like hxh it has
  no per-project auth floor — endpoints (`internal/bap/`) are gated
  by the shared **admin** system; any logged-in user reads/writes
  their own cup state.
- **bots** — not a project but a service in the same binary
  (`internal/bots`): bot programs driving `is_bot` users through the
  PUBLIC site API. `hxh_chatbots` is the first (README "Bots"; spec in
  feathers `foundry/website/docs/HXH_BOTS.md`). Tables `shared_*` in
  the shared DB: `shared_random_sentences` (public-domain corpus,
  `cmd/seed-sentences`), `shared_bot_program` (on/off per program,
  console toggle). `shared_` is the prefix for cross-project DATA
  tables; `hobby_server_` stays the auth system's.
- **admin** — the SHARED cross-website auth system
  (`internal/shared/`, console at
  [andrewcheong.com/admin](https://andrewcheong.com/admin)). One
  account per person (`hobby_server_user`), per-website roles
  (`hobby_server_user_roles`), domain-wide SSO cookie
  (`hobby_session`, Path=/), one-time invite/reset links. Tables live
  in the shared `hobby_server` DB (`hobby_server_*` prefix, schema in
  `liquibase/admin/`). This project is special-cased in
  `cmd/server/main.go` — it does NOT get the generic per-project auth
  floor. rv does not use it (yet; may migrate one day).

(Keep this list in sync with the one in `README.md` and
`ARCHITECTURE.md`.)

## What this server does, per project

Three endpoints under each project's `url_prefix`:
- `POST <prefix>/login` — sets `<project>_session` cookie
- `POST <prefix>/logout` — clears it
- `GET <prefix>/me` — returns username when logged in

Plus `GET /healthz` at the server root.

That's the floor for every project. Add more endpoints (per-project
state, etc.) as needed.

## Where things live

- `cmd/server/main.go` — multi-project HTTP server (loops over
  `config.projects[]` at startup)
- `cmd/add-user/main.go` — `--project <name> <user> <pass>` upsert
- `internal/auth/auth.go` — sessions + bcrypt
- `liquibase/<project>/changelog/` — per-project schema (one dir per
  project, never edit landed changesets). Table names carry the
  project prefix (`hxh_characters`, not `characters`) — see
  AGENTS.md N7; rv's unprefixed tables are grandfathered.
- `internal/hxh/rosterdb.go` — the Roster DB (2026-09-19): the curated
  character base built one character at a time by the feathers
  `hxh-character` skill and reviewed in the Roster DB desktop app
  (feathers `html/hxh/apps/roster/`). Tables `hxh_char` (no slug —
  the id is the number, the name the handle; `version` bumps on every
  change to the character or its pictures; `review_status` pending /
  accepted / rejected with `review_reason`), `hxh_char_image`
  (BYTEA + server-made thumb, typed raw = "Random" (found by the
  skill) / uploaded / cropped / pixelated / upscaled / transparent with
  `source_image_id` lineage; sha256 unique per character) and
  `hxh_char_review` (the verdict log, per version) — changesets 006
  to 008 (`card_description`: what the Greed Island card prints)
  and 007 (`owner` on all three tables: the bot user `claude` for what
  the skill finds, Andrew/Abi for uploads, crops and verdicts). API
  `/api/hxh/db/*`; writes need hxh admin OR the site-wide `admin` role
  (how `claude` writes without an hxh role), picture reads any hxh
  role; `POST /chars/{id}/review` passes a verdict (reason optional)
  and never bumps the version; the server does the cropping (exact
  source pixels, PNG). `GET /db/binder` (any hxh role) is the Binder's
  source: the accepted characters that have BOTH an avatar and a card
  picture (Andrew, 2026-09-21), card fields only. Pure parts tested
  in `rosterdb_test.go`.
  Requests (changeset 010, 2026-09-21): a reviewer asks the bot for
  work — `POST /chars/{id}/request {kind, text}` files a row in
  `hxh_char_request` (kind = a slug from `hxh_request_kind`, seeded
  `extend-picture` and `card-description`; a new kind is a changeset)
  and puts the character in `review_status` "requested" (a fourth
  state; the review log gets a "requested" line). `GET /requests?
  status=open` is the queue (a request may name one picture:
  `image_id`, changeset 013; kinds carry a scope character/image/any
  and `needs_text` — seeded `card-description`, `outpaint-white`,
  `other`; deleting a picture withdraws its open requests in the
  deleter's name), `POST /requests/{id}/resolve` marks one
  done and, when it was the character's last open one, sets the
  character back to "pending". A reviewer's verdict on a requested
  character withdraws its open requests. The feathers CLI
  (`hxh-roster/roster.py requests | resolve | kinds | request`) and
  the skill's § 8b are how the bot works the queue.
  Card numbers (changeset 011, 2026-09-21): `card_number` is the
  binder position, separate from the id (the handle, never renumbered)
  and NOT unique on purpose (Andrew: a duplicate is easy to fix, a
  constraint would make swapping hard). Starts as the id; a new
  character takes max+1; patchable. `POST /chars/{id}/move {after}`
  puts a character right after another (0 = the front) in one
  transaction — `moveOrder` in rosterdb.go re-hands the numbers held
  between the old and new position, so gaps and duplicates survive and
  nothing outside that stretch changes — and answers with the whole
  list (the Roster DB app's drag-and-drop calls it; versions do not
  bump). `/chars` and `/db/binder` order by card_number then id.
  Change log (changeset 012, 2026-09-21): `hxh_char_change` — one row
  per field set (old/new) or picture added/removed/edited, under the
  version that change made, with owner and a `bot` flag (the request
  middleware looks up `is_bot` and puts it in the context, `botOf`).
  Every version bump goes through `Store.changed`, which also sends an
  accepted/rejected character back to pending when the BOT changed it
  (a review-log "pending" line says what); a person's change is
  self-approved. A patch that changes nothing does not bump. The
  character payload carries `baseline` (the last accepted/rejected
  verdict) and `changes` above it; the app marks the bot's ones "New".
  The review log holds verdicts only — requests no longer mirror into
  it (their table and window are the record).
  Accepted is a VERSION (changeset 014, 2026-09-21): `accepted_version`
  + `accepted_snapshot` (the card fields as JSON, `snapshotExpr`,
  written by Accept and by a person's edit to an accepted character —
  self-approved — never by a bot's). `GET /db/binder` prints the
  snapshots (both pictures required), so a bot's edit shows nowhere
  until re-accepted; the live row goes pending meanwhile. Requests are
  a count (`open_requests`), not a state: filing, resolving and
  verdicts never touch each other. Guards: a bot cannot pass a
  verdict; a picture on an accepted card cannot be deleted (409).
  The 2026-09-17 `hxh_characters` roster tables stay as a cross-check
  source.
- `internal/hxh/chat.go` + `hub.go` — the chat endpoints, profile-run
  validation and the WebSocket hub (tests: `hub_test.go`, `go test
  ./internal/hxh/`). Presence lives in the SHARED user table:
  `last_seen_at` (touched by `Store.GetSession`, throttled) and
  `activated_at` (set by `ConsumeToken` on the first password) —
  admin changeset 006.
- `internal/bots/` — the bot service: `service.go` (scheduler, program
  registry, hooks), `site.go` (the public API as a client: login,
  contacts, history, WebSocket typing/send), `chatbots.go`
  (`hxh_chatbots` — the maths and the speech act; tests in
  `chatbots_test.go` with a fake site and a scripted die),
  `handlers.go` (console endpoints). The hub calls `Hub.OnMessage` for
  every stored message; main wires it to `Service.Hook`. Bot passwords
  are provisioned from `bots.password` (config) — the one bot-specific
  act; everything else is what a browser does.
- `internal/shared/` — the shared auth system (users with
  initial/colour/email profile fields, roles, sessions, links) and
  `email.go`, which fetches each website's `_email/` templates over
  HTTP and sends them via `internal/mailer/` (SMTP; server-wide
  `email:` block in config.yaml, optional — unconfigured = the console
  says so). Guide: `docs/SHARED_AUTH.md`.
- `install.sh` — Docker build + per-project Liquibase + restart
- `docs/SETUP.md` — first-time VM walkthrough
- `docs/SHARED_AUTH.md` — how ANY website integrates the shared
  login/roles/invite system. Read it before touching auth anywhere.

## Adding a new project (the playbook)

See `ARCHITECTURE.md` §7. Short version:

1. `CREATE DATABASE foo_db;` + user + grants
2. `mkdir -p liquibase/foo/changelog/` + `db.changelog-master.xml` +
   `001-initial-schema.xml` (copy from `liquibase/rv/` template)
3. Append a `projects:` entry to your `~/.config/hobby-server/config.yaml`
4. Push schema dir to GitHub
5. `deploy_latest_hobby_server` on the VM
6. Apache `<Location>` block proxying public URL → backend

## Querying the prod DB directly

Sometimes you need to inspect prod data (verify overrides landed, compare
API output vs. DB truth). The Cloud SQL Postgres instance is on a private
IP; access is through the VM:

```bash
# 1. Grab the DB password from the VM's hobby-server config:
ws_ssh 'grep -A3 "name: rv" ~/.config/hobby-server/config.yaml | grep password'

# 2. Run a query (substitute the password from step 1):
ws_ssh 'PGPASSWORD="<paste-password>" psql -h 10.12.32.3 -U hobby_server -d hobby_server -c "SELECT ... FROM ...;"'
```

- Host `10.12.32.3` is the Cloud SQL Auth Proxy's private IP.
- DB name / user are both `hobby_server`.
- All projects (rv, future) share this one DB; project isolation is by
  table-name prefix (`rv_...`) — though currently rv tables are
  unprefixed (`itinerary_override`, `location_user`, `dunkin_log`, ...).
- `ws_ssh` is a shell alias for `ssh -i ~/.ssh/id_ed25519_gcp_202512
  acheong87@35.243.192.242`.

## Deploy

```bash
deploy_latest_hobby_server() {
  bash <(curl -sSL -H "Cache-Control: no-cache" \
    "https://raw.githubusercontent.com/slackwing/hobby-server/main/install.sh")
}
deploy_latest_hobby_server
```
