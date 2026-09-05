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
  unused — the site logs in via the shared **admin** system below;
  the `hxh` DB is reserved for future party data tables.
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
  project, never edit landed changesets)
- `install.sh` — Docker build + per-project Liquibase + restart
- `docs/SETUP.md` — first-time VM walkthrough

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
