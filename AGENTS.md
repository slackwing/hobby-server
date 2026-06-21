# Agent Instructions

For AI coding agents (Claude Code, Cursor, etc.) working on this repo.

This repo mirrors the patterns of
[`slackwing/manuscript-studio`](https://github.com/slackwing/manuscript-studio).
The frontend that talks to this server lives in
[`slackwing/feathers`](https://github.com/slackwing/feathers) at
`foundry/website/html/rv/` (locally: `~/src/feathers/foundry/website/html/rv/`).
When making changes here, check whether the frontend's
`assets/auth.js` / `index.html` / `prep.html` need a corresponding update.

---

## 🚨 CRITICAL NOTICES — READ FIRST 🚨

### N1 — Never modify schema outside Liquibase

No `ALTER TABLE` / `CREATE TABLE` in Go code, no manual `psql` schema
changes. All schema changes go through a new changeset under
`liquibase/changelog/` (002+, never edit landed changesets).

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

A frontend change (in feathers) that adds a new endpoint call needs the
endpoint to exist here first. A backend change that breaks the
auth-cookie shape will break the frontend. Both sides have CLAUDE.md
files pointing at each other; check both before changing the wire
format.

---

## 1. Adding a new endpoint

1. Add the handler under `cmd/server/main.go` (or a new
   `internal/api/<feature>.go` file if it grows).
2. Wire it in the chi router under `/api/...`.
3. If it needs auth, wrap with `auth.Middleware(store)`.
4. If state-changing (POST/PUT/DELETE), consider CSRF (not implemented
   yet — port from manuscript-studio's `auth.CSRFMiddleware` if/when
   needed).
5. Update `feathers/foundry/website/html/rv/assets/auth.js` (or a sibling
   `*-client.js`) with a wrapper.
6. Document in `README.md` "Endpoints" section.

## 2. Adding a new table or column

1. New changeset file `liquibase/changelog/00N-<name>.xml`. Sequential.
2. Add `<include>` in `liquibase/changelog/db.changelog-master.xml`.
3. Re-run `install.sh` on the dev VM to verify the migration applies.
4. Add Go code that reads/writes the new column.

## 3. Local testing

```bash
# 1. Run a local Postgres (or point at dev Cloud SQL via the proxy):
docker run -d --name rv-pg -p 5433:5432 \
  -e POSTGRES_USER=rv_user -e POSTGRES_PASSWORD=devpass -e POSTGRES_DB=rv_trip \
  postgres:16-alpine

# 2. Apply Liquibase:
docker build -f Dockerfile.liquibase -t rv-liquibase .
docker run --rm --network host rv-liquibase \
  --changeLogFile=changelog/db.changelog-master.xml \
  --url=jdbc:postgresql://localhost:5433/rv_trip \
  --username=rv_user --password=devpass update

# 3. Add a user:
go run ./cmd/add-user --config config.dev.yaml abi beeboweebo

# 4. Run the server:
go run ./cmd/server --config config.dev.yaml

# 5. Test:
curl -sS -X POST http://localhost:5002/api/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"abi","password":"beeboweebo"}' \
  -c /tmp/cookies.txt
curl -sS http://localhost:5002/api/me -b /tmp/cookies.txt
```

## 4. Style

- Standard Go: `gofmt`, no extra linter rules yet.
- Errors flow up; only the top-level handlers translate them to HTTP
  status codes.
- pgx queries use `$1, $2, ...` placeholders; never string-concat SQL.
- Constants for magic numbers (TTLs, cookie names) live at the top of
  the file that uses them.

## 5. When in doubt

- For deploy / Docker / Liquibase questions: read `manuscript-studio`'s
  equivalent file. Its patterns are battle-tested.
- For frontend integration: read the feathers repo's
  `foundry/website/html/rv/assets/auth.js`.
