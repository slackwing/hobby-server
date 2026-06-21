# rv-server

Authentication backend for [Andrew & Abi's RV trip site](https://andrewcheong.com/rv).

Static frontend lives in the
[`feathers`](https://github.com/slackwing/feathers) repo at
`foundry/website/html/rv/`. This server provides login/session endpoints so
parts of the frontend (e.g. the prep checklist) can be gated.

Mirrors [`manuscript-studio`](https://github.com/slackwing/manuscript-studio)'s
deploy patterns: Go binary in Docker, Liquibase for schema, Apache reverse
proxy, Cloud SQL via Cloud SQL Auth Proxy.

## Endpoints

Mounted under `/api/` (Apache proxies `andrewcheong.com/rv/api/*` →
`127.0.0.1:5002`):

- `POST /api/login` — `{username, password}` → sets `rv_session` cookie
- `POST /api/logout` — clears cookie + deletes session
- `GET /api/me` — returns `{username}` when logged in, 401 otherwise
- `GET /healthz` — liveness probe

## Install

One-liner (re-runs the latest):

```bash
deploy_latest_rv_server() {
  bash <(curl -sSL -H "Cache-Control: no-cache" \
    "https://raw.githubusercontent.com/slackwing/rv-server/main/install.sh")
}
deploy_latest_rv_server
```

First run writes `~/.config/rv-server/config.yaml`. Edit it with the DB
password, then re-run.

## Add a user

```bash
docker run --rm --network host \
  -v "$HOME/.config/rv-server/config.yaml:/config/config.yaml:ro" \
  rv-server:latest \
  add-user --config /config/config.yaml <username> <password>
```

## Local dev

```bash
go run ./cmd/server --config config.dev.yaml
```

Or build + run:

```bash
go build -o rv-server ./cmd/server
./rv-server --config ./config.dev.yaml
```

See `ARCHITECTURE.md` for the deeper tour, `AGENTS.md` for AI-agent rules.
