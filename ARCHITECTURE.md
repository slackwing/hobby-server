# hobby-server — Architecture

A high-level guide to how the pieces fit together. The code wins when in
doubt.

---

## 1. Purpose

A small **multi-project** auth + storage backend for slackwing's hobby
projects. One Go binary, one Docker container, multiple isolated
project namespaces. Each project has:

- its own Postgres **database** (full table isolation)
- its own **Liquibase changelog** (`liquibase/<name>/changelog/`)
- its own **URL prefix** (mounted as a chi sub-router)
- its own **cookie scope** (`<name>_session`, Path=`<cookie_path>`)

Deliberately small per project — three HTTP endpoints (`/login`,
`/logout`, `/me`), two DB tables (`user`, `session`). Each new project
plugs in by adding a config entry + a changelog dir.

---

## 2. Currently hosted projects

- **rv** — auth for the
  [feathers RV trip site](https://andrewcheong.com/rv).
  Frontend in `slackwing/feathers`:
  `foundry/website/html/rv/`.

When adding a new project, update the "Currently hosted projects" list
in this file and in `README.md`.

---

## 3. Request topology

```
┌───────────────┐    ┌────────────────────┐    ┌──────────────┐
│   Browser     │───►│ Apache (TLS, proxy)│───►│ hobby-server │
└───────────────┘    │  - serves static   │    │ (Go, :5002)  │
                     │  - per-project     │    └──────┬───────┘
                     │    proxy blocks:   │           │ per-project pgx pool
                     │    /rv/api/*   →   │           │
                     │    127.0.0.1:5002/ │           ▼
                     │    api/rv/*        │    ┌──────────────┐
                     │    /next/api/* →   │    │ PostgreSQL   │
                     │    127.0.0.1:5002/ │    │ (one DB per  │
                     │    api/next/*      │    │  project,    │
                     └────────────────────┘    │  via Cloud   │
                                               │  SQL Auth    │
                                               │  Proxy)      │
                                               └──────────────┘
```

- **Static sites** are rsynced into `/var/www/html/<project>/` and
  served by Apache directly. No backend involvement for static files.
- **`hobby-server`** runs in a single Docker container with
  `--restart unless-stopped`. Docker handles restarts; there's no
  separate systemd unit.
- **Postgres**: one database per project, all on the same Cloud SQL
  instance via the Auth Proxy at `127.0.0.1:5432`. Same instance as
  `manuscript-studio`.

---

## 4. Repository layout

```
cmd/
  server/             cmd/server/main.go — long-running multi-project HTTP server.
                      Loops over config.projects[] at startup; one chi
                      sub-router + pgx pool + session store per project.
  add-user/           Upsert a user. Required: --project <name>.
                      Routes to that project's database.
internal/
  auth/               Password hashing (bcrypt) + DB-backed cookie sessions.
                      Cookie name is project-scoped (passed in at call site).
  config/             YAML loader (~/.config/hobby-server/config.yaml).
                      Loads `projects: []` (each: name, database{}, url_prefix, cookie_path).
  database/           pgx pool wrapper.
liquibase/
  rv/changelog/       Per-project Liquibase changesets. ALL schema changes
                      go here — never via raw SQL or Go. Add new projects
                      as new sibling directories: liquibase/<name>/changelog/.
config.example.yaml   Template copied on first install.sh run.
config.dev.yaml       Local-dev config; points at a Docker Postgres on :5433.
Dockerfile            Multi-stage build → /usr/local/bin/{hobby-server, add-user}.
Dockerfile.liquibase  Liquibase image with all changelog subdirs baked in.
                      install.sh mounts liquibase/<name> per project at /liquibase/project.
install.sh            One-liner deploy: pulls source, builds images, runs
                      Liquibase per project, restarts the container.
docs/
  SETUP.md            First-time walkthrough on a fresh VM.
```

---

## 5. Auth model (per project)

- **Passwords** are bcrypt-hashed and stored in the project's `user`
  table. The `password_hash` column is the only user-facing secret.
- **Sessions** are random 32-byte tokens stored in the project's
  `session` table. The cookie value IS the row's primary key. No JWT —
  DB lookup on every request (sliding-window update on every read;
  refresh only when the remaining lifetime drops under 7 days).
- **Cookie**: `<project>_session` (e.g. `rv_session`), `HttpOnly`,
  `Secure` (in production), `SameSite=Lax`, `Path=<cookie_path>`.
  Path-scoping prevents the cookie from leaking to other projects on
  the same domain.
- **30-day TTL**, sliding. Configurable in `internal/auth/auth.go`.
- **Failed-login timing**: always do a bcrypt comparison even when the
  username doesn't exist, to avoid leaking user existence via timing.

## 6. Schema (per project)

Two tables (see `liquibase/<name>/changelog/001-initial-schema.xml`):

- `"user"` — `username` (PK), `password_hash`, `created_at`.
- `"session"` — `id` (PK = cookie token), `username` (FK), timestamps.
  Cascades on user delete.

Quote `"user"` in every SQL reference; it's a reserved word in some SQL
contexts.

**Per-project means**: `rv_trip` has its OWN `user` and `session`
tables; future `next_thing_db` will have its own. Users don't roam.

## 7. Adding a new project (the playbook)

1. **DB**: `CREATE DATABASE foo_db; CREATE USER foo_user ...; GRANT ...`.
2. **Schema dir**: `mkdir -p liquibase/foo/changelog/` and add
   `db.changelog-master.xml` + `001-initial-schema.xml` (copy from
   `liquibase/rv/changelog/` and tweak as needed).
3. **Config**: append a new project block to
   `~/.config/hobby-server/config.yaml`.
4. **Push** the new schema dir to GitHub (install.sh pulls source).
5. **Run** `deploy_latest_hobby_server` on the VM. Liquibase will run
   the new changelog against the new DB; the server picks up the new
   project and mounts its endpoints; you can `add-user --project foo`.
6. **Apache**: add a `<Location>` block proxying the new public URL
   slice → `127.0.0.1:5002/<url_prefix>/`.

## 8. Why mirror manuscript-studio

You already know the manuscript-studio deploy pattern (Docker image
build → Liquibase migrations → restart container). Mirroring it here
means:

- One mental model for both apps.
- Reuse the Cloud SQL Auth Proxy already running on the VM.
- Reuse the YAML config conventions, Apache reverse-proxy style, and
  the `install.sh` discipline (`SCRIPT_VERSION` bumps,
  `~/.config/<app>/logs/`).

When you fix something here that also applies there (or vice versa),
consider porting the fix.
