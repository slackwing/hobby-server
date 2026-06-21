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

## Endpoints

For each project P, three endpoints under its `url_prefix`:

- `POST <prefix>/login` — `{username, password}` → sets `<P>_session`
  cookie scoped to `cookie_path`
- `POST <prefix>/logout` — deletes session row + clears cookie
- `GET  <prefix>/me` — returns `{username}` when logged in, 401
  otherwise

Plus `GET /healthz` at the server root (used by Docker healthcheck).

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
