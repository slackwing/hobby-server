# hobby-server

A small multi-project auth + storage backend for slackwing's hobby
projects. One Go binary in Docker, running multiple **isolated**
project namespaces:

- Each project has its **own Postgres database** (no shared tables).
- Each project has its **own Liquibase changelog** under
  `liquibase/<project>/`.
- Each project has its **own URL prefix** + **own cookie scope**, so
  logins don't bleed between projects.

Add a new project: drop a `liquibase/<new_project>/` dir, add an entry
to `config.yaml`'s `projects:` list, re-run `install.sh`. Done.

Mirrors [`manuscript-studio`](https://github.com/slackwing/manuscript-studio)'s
deploy patterns: Docker, Liquibase, install.sh with `SCRIPT_VERSION`
discipline, Cloud SQL via the Cloud SQL Auth Proxy.

## Currently configured projects

- **rv** — auth for the
  [RV trip site](https://github.com/slackwing/feathers/tree/master/foundry/website/html/rv).
  Database: `rv_trip`. URL prefix: `/api/rv` (Apache rewrites public
  `/rv/api/*` to this). Cookie path: `/rv/`.
- **hxh** — auth for the
  [Hunter × Halloween party site](https://github.com/slackwing/feathers/tree/master/foundry/website/html/hxh).
  Database: `hxh`. URL prefix: `/api/hxh` (Apache rewrites public
  `/hxh/api/*` to this). Cookie path: `/hxh/`. Also hosts the site's
  **Beetle** chat: a WebSocket hub (`internal/hxh/hub.go`) at
  `/api/hxh/chat/ws` — Apache must proxy that one path with
  `mod_proxy_wstunnel` (see Endpoints) — plus `hxh_chat_message` /
  `hxh_chat_profile`.
- **bap** — data for the
  [bongo-cat table-bap site](https://github.com/slackwing/feathers/tree/master/foundry/website/html/bap).
  Database: `bap`. URL prefix: `/api/bap` (Apache rewrites public
  `/bap/api/*` to this). Auth via the shared system (no per-project
  floor); see `internal/bap/`.
- **admin** — shared cross-website auth system (one account, roles per
  website, SSO cookie `hobby_session` at Path=/, invite/reset links).
  Database: shared `hobby_server` (tables prefixed `hobby_server_*`).
  URL prefix: `/api/admin` (public `/admin/api/*`). See
  `internal/shared/`.

## Endpoints

For each project P, three endpoints under its `url_prefix`:

- `POST <prefix>/login` — `{username, password}` → sets `<P>_session`
  cookie scoped to `cookie_path`
- `POST <prefix>/logout` — deletes session row + clears cookie
- `GET  <prefix>/me` — returns `{username}` when logged in, 401
  otherwise

Plus `GET /healthz` at the server root (used by Docker healthcheck).

### hxh chat (shared-auth session with any role on `hxh`)

- `GET  /api/hxh/chat/contacts` — every hxh member with presence
  (`online` < 1 min of activity or a live socket, `away` < 1 h,
  `offline`, `nopass` = no password yet)
- `GET  /api/hxh/chat/history?room=global|dm:<a>:<b>` — the last 100
  live messages written after the viewer's `activated_at`
- `GET  /api/hxh/chat/profile/{username}` / `PUT /api/hxh/chat/profile`
  `{runs:[…]}` — AIM-style profiles in a JSON "runs" format (never
  HTML), ≤ 1024 characters
- `GET  /api/hxh/chat/ws` — the WebSocket: `msg`, `typing`, `read`,
  `ping` in; `hello` (contacts + `unread` per room), `msg`, `typing`,
  `presence`, `read` (to the user's other tabs), `pong`, `error` out
  (frame shapes at the top of `internal/hxh/hub.go`). Read markers
  (`hxh_chat_read`, changeset 008) are per user and room: Andrew's
  rule is that only the focused tab's active window reads. A DM to
  someone neither online nor away is refused (`error offline`) —
  "you can message online and away people, but not offline".
  Mounted outside the 15 s request timeout. Apache needs
  `a2enmod proxy_wstunnel` and, after the `/hxh/api` block:

  ```
  <Location /hxh/api/chat/ws>
      ProxyPass ws://127.0.0.1:5002/api/hxh/chat/ws
      ProxyPreserveHost On
  </Location>
  ```

Presence comes from `hobby_server_user.last_seen_at`, bumped (at most
every 20 s) by every authenticated request on any website;
`activated_at` is set when an account first gets a password.

## Bots

`internal/bots` runs **bot programs** — fake users (`is_bot`, listed
after real users in the console) that play the site through its PUBLIC
API exactly like a browser would, so they double as end-to-end
testers. Enable with the `bots:` block in config.yaml; each program is
a row in `shared_bot_program` and can be switched on/off from the
console (`GET/PUT /api/admin/bots[/{name}]`).

- **hxh_chatbots** (Andrew's spec, 2026-09-19): thirteen bots named
  after The Brothers Karamazov and The Idiot, each with
  `metadata.talkativity` (0..1). Every tick (5 min) every bot rolls;
  weight 0.3 + 0.7·talkativity over the sum, so ≈ 1 conversation
  starts per tick. A speech waits 5 s + 55 s·u² (mostly near 5 s),
  sends "typing" once a second for the last 5 s, and targets any
  REACHABLE member (online or away — never the offline or the
  password-less, real or bot; same rule as the hub) or the global
  room — which weighs as much as three members — with a random line
  from `shared_random_sentences`. A reply owed to someone who went
  offline is dropped. A message reaching a bot wakes
  it: reply chance 0.5 + 0.4·talkativity, same delay — unless
  md5(body) mod 10 < 2 (nobody answers that one), re-checked against
  the room's latest message right before sending. Safety valve: one
  pending speech per bot, and 10 min of silence in global after
  speaking there. After a message the bot lingers 30–120 s (online),
  then hangs up (away after a minute, offline after an hour).
- **Corpus**: `seed-sentences` fills `shared_random_sentences` with
  10,000 lines from public-domain children's books on Project
  Gutenberg (Carroll, Lear, Potter, Grahame, Milne, Baum, Barrie,
  Kipling, Collodi, Grimm, Andersen, Aesop, Montgomery, Burnett…):

  ```bash
  docker run --rm --network host \
    -v "$HOME/.config/hobby-server/config.yaml:/config/config.yaml:ro" \
    hobby-server:latest seed-sentences --config /config/config.yaml
  ```

## Install (production)

```bash
deploy_latest_hobby_server() {
  bash <(curl -sSL -H "Cache-Control: no-cache" \
    "https://raw.githubusercontent.com/slackwing/hobby-server/main/install.sh")
}
deploy_latest_hobby_server
```

First run writes `~/.config/hobby-server/config.yaml`. Edit it with
your project list + DB passwords, then re-run.

See `docs/SETUP.md` for the full first-time walkthrough (Cloud SQL
setup, Apache proxy config, etc.).

## Add a user

```bash
docker run --rm --network host \
  -v "$HOME/.config/hobby-server/config.yaml:/config/config.yaml:ro" \
  hobby-server:latest \
  add-user --config /config/config.yaml --project rv abi beeboweebo
```

## Adding a new project

1. **Database**: create it in Postgres + create a dedicated user with
   grants on that DB only.
2. **Schema**: add `liquibase/<name>/changelog/db.changelog-master.xml`
   + at least one changeset.
3. **Config**: append a new entry to `projects:` in your `config.yaml`.
4. **Re-run** `deploy_latest_hobby_server`. Liquibase will migrate the
   new DB, the server picks up the new project from config, the new
   `<name>_session` cookie + endpoints are mounted.
5. **Apache**: add a `<Location>` block proxying the new project's
   public URL to `127.0.0.1:5002/<url_prefix>/`.

## Local dev

```bash
docker run -d --name hobby-pg -p 5433:5432 \
  -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=devpass \
  postgres:16-alpine
# psql in and CREATE DATABASE rv_trip_dev;

go run ./cmd/server --config ./config.dev.yaml
```

See `ARCHITECTURE.md` for the deeper tour, `AGENTS.md` for AI-agent
rules.
