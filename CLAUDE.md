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

## Deploy

```bash
deploy_latest_hobby_server() {
  bash <(curl -sSL -H "Cache-Control: no-cache" \
    "https://raw.githubusercontent.com/slackwing/hobby-server/main/install.sh")
}
deploy_latest_hobby_server
```
