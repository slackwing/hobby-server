# rv-server

Authentication backend for the RV trip site.

**Sibling repo (the frontend):**
[`slackwing/feathers`](https://github.com/slackwing/feathers) →
`foundry/website/html/rv/` (locally:
`~/src/feathers/foundry/website/html/rv/`).

**Pattern this mirrors:**
[`slackwing/manuscript-studio`](https://github.com/slackwing/manuscript-studio)
— Go binary in Docker, Liquibase schema, Apache reverse proxy, Cloud SQL
via Auth Proxy. When you don't know how to do something deploy- or
schema-related here, look at how manuscript-studio does it.

## What this server does

Three endpoints under `/api/`:
- `POST /api/login` — sets `rv_session` cookie
- `POST /api/logout` — clears it
- `GET /api/me` — returns username when logged in

Plus `GET /healthz`.

That's it for now. Will grow as the frontend needs more stateful features
(persistent prep checklist, "current location" tracking, etc.).

## Where things live

- `cmd/server/main.go` — the HTTP server
- `cmd/add-user/main.go` — one-shot CLI to insert/update a user
- `internal/auth/auth.go` — sessions + bcrypt
- `liquibase/changelog/` — all schema
- `install.sh` — Docker build + Liquibase + restart

## Docs

- `README.md` — quick start.
- `ARCHITECTURE.md` — deeper tour.
- `AGENTS.md` — critical notices for AI agents (read first).

## Deploy

```bash
deploy_latest_rv_server() {
  bash <(curl -sSL -H "Cache-Control: no-cache" \
    "https://raw.githubusercontent.com/slackwing/rv-server/main/install.sh")
}
deploy_latest_rv_server
```
