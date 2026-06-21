# Agent Instructions

For AI coding agents (Claude Code, Cursor, etc.) working on this repo.

This repo mirrors the patterns of
[`slackwing/manuscript-studio`](https://github.com/slackwing/manuscript-studio).
hobby-server is **multi-project**: one binary, one container, multiple
isolated project namespaces. The currently hosted projects are listed
in `README.md` and `CLAUDE.md`.

For the **rv** project, the frontend lives in
[`slackwing/feathers`](https://github.com/slackwing/feathers) at
`foundry/website/html/rv/` (locally:
`~/src/feathers/foundry/website/html/rv/`). Other projects will have
their own frontend repos; update this file when adding them.

When making changes here, check whether the affected project's
frontend needs an update too (especially anything that touches the
wire format: cookie name, request/response shapes, endpoint paths).

---

## 🚨 CRITICAL NOTICES — READ FIRST 🚨

### N1 — Never modify schema outside Liquibase

No `ALTER TABLE` / `CREATE TABLE` in Go code, no manual `psql` schema
changes. All schema changes go through a new changeset under
`liquibase/<project>/changelog/` (002+, never edit landed changesets).

Each project's schema is independent — `rv` has its own changelog
under `liquibase/rv/changelog/`; future projects will have siblings.

Violation = failed task.

### N2 — Update `install.sh` → bump `SCRIPT_VERSION`

The script is distributed via `curl | bash` and GitHub's CDN caches it
for several minutes. The version string printed at the top of each run
lets the user confirm they got the edit, not a cached copy.

Format: `YYYY-MM-DD.N`. If today's date is used, increment `.N`; if
newer date, reset `.N` to `1`. Bump on every change, even trivial ones.

### N3 — Quote `"user"` in every SQL statement

`user` is a reserved word in some SQL contexts. The Liquibase changeset
creates the table with that name, but every `SELECT/INSERT/UPDATE` in
Go must reference it as `"user"`.

### N4 — Never log secrets

No log line should contain the user's password, the bcrypt hash, the
DB password, or a session token. The session middleware already keeps
the token in cookies, never in logs. Audit before merging.

### N5 — Cross-repo coordination

A frontend change that adds a new endpoint call needs the endpoint to
exist here first. A backend change that breaks the auth-cookie shape
will break the frontend. Each project has paired repos:

- **rv**: feathers' `foundry/website/html/rv/` ↔ this repo's `rv`
  project (config + `liquibase/rv/`)

Check both before changing the wire format.

### N6 — Project isolation must hold

Per design: each project's data lives in its own database. Don't add
queries that join across project DBs, don't add a shared "all users"
table, don't share session stores. If two projects need to share
something, it gets its own service.

---

## 1. Adding a new endpoint (to an existing project)

1. Add the handler under `cmd/server/main.go`. Endpoints are wired in
   the chi `r.Route(p.URLPrefix, ...)` block — add yours there.
2. If it needs auth, wrap with
   `auth.Middleware(state.store, state.cookieName)`.
3. If state-changing (POST/PUT/DELETE), consider CSRF (not implemented
   yet — port from manuscript-studio's `auth.CSRFMiddleware` if
   needed).
4. Update the project's frontend (e.g. `feathers/.../rv/assets/auth.js`
   for the rv project) with a wrapper.
5. Document in `README.md` "Endpoints" section.

## 2. Adding a new table or column (to an existing project)

1. New changeset file `liquibase/<project>/changelog/00N-<name>.xml`.
   Sequential.
2. Add `<include>` in
   `liquibase/<project>/changelog/db.changelog-master.xml`.
3. Re-run `install.sh` on the dev VM to verify the migration applies.
4. Add Go code that reads/writes the new column.

## 3. Adding a whole new project

See `ARCHITECTURE.md` §7. Short version:

1. `CREATE DATABASE <name>_db;` + user + grants
2. `mkdir -p liquibase/<name>/changelog/` + master file + first
   changeset (copy from `liquibase/rv/changelog/`)
3. Append a `projects:` entry to `~/.config/hobby-server/config.yaml`
4. Push to GitHub; run `deploy_latest_hobby_server` on the VM
5. Apache `<Location>` block

Then **update README.md, CLAUDE.md, ARCHITECTURE.md, and this file**
with the new project in their "Currently hosted projects" lists.

## 4. Local testing

```bash
# 1. Local Postgres:
docker run -d --name hobby-pg -p 5433:5432 \
  -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=devpass \
  postgres:16-alpine

# 2. Create the dev DB:
PGPASSWORD=devpass psql -h localhost -p 5433 -U postgres \
  -c 'CREATE DATABASE rv_trip_dev;'

# 3. Apply Liquibase for the rv project:
docker build -f Dockerfile.liquibase -t hobby-liquibase .
docker run --rm --network host \
  -v "$PWD/liquibase/rv:/liquibase/project:ro" \
  hobby-liquibase \
  --searchPath=/liquibase/project \
  --changeLogFile=changelog/db.changelog-master.xml \
  --url=jdbc:postgresql://localhost:5433/rv_trip_dev \
  --username=postgres --password=devpass update

# 4. Add a user to the rv project:
go run ./cmd/add-user --config config.dev.yaml --project rv abi beeboweebo

# 5. Run the server:
go run ./cmd/server --config config.dev.yaml

# 6. Test:
curl -sS -X POST http://localhost:5002/api/rv/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"abi","password":"beeboweebo"}' \
  -c /tmp/cookies.txt
curl -sS http://localhost:5002/api/rv/me -b /tmp/cookies.txt
```

## 5. Style

- Standard Go: `gofmt`, no extra linter rules yet.
- Errors flow up; only the top-level handlers translate them to HTTP
  status codes.
- pgx queries use `$1, $2, ...` placeholders; never string-concat SQL.
- Constants for magic numbers (TTLs, cookie names) live at the top of
  the file that uses them.

## 6. When in doubt

- For deploy / Docker / Liquibase questions: read `manuscript-studio`'s
  equivalent file. Its patterns are battle-tested.
- For per-project frontend integration: read that project's frontend
  repo (e.g. feathers' `assets/auth.js` for rv).
