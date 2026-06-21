# rv-server — Architecture

A high-level guide to how the pieces fit together. The code wins when in
doubt.

---

## 1. Purpose

Login backend for the [feathers RV trip site](https://andrewcheong.com/rv).
Gates parts of the static frontend (currently: the prep checklist link) on
a cookie-based session.

Deliberately small — three HTTP endpoints, two DB tables.

---

## 2. Sibling repo

The static frontend lives in
[`slackwing/feathers`](https://github.com/slackwing/feathers) at
`foundry/website/html/rv/`. It calls this server via `fetch("/rv/api/...")`.
Apache on the VM proxies that path to `127.0.0.1:5002`.

When you change auth behavior here, check whether the frontend needs an
adjustment in:
- `rv/assets/auth.js` (the client-side caller)
- `rv/index.html` and `rv/prep.html` (login button + gated UI)

---

## 3. Request topology

```
┌───────────────┐    ┌────────────────────┐    ┌──────────────┐
│   Browser     │───►│ Apache (TLS, proxy)│───►│ rv-server    │
└───────────────┘    │  - serves static   │    │ (Go, :5002)  │
                     │    at /rv/         │    └──────┬───────┘
                     │  - proxies         │           │
                     │    /rv/api/* →     │           ▼
                     │    127.0.0.1:5002  │    ┌──────────────┐
                     └────────────────────┘    │ PostgreSQL   │
                                               │ (Cloud SQL via
                                               │  Auth Proxy)  │
                                               └──────────────┘
```

- **Static site** is rsynced to `/var/www/html/rv/` and served by Apache
  directly. No backend involvement.
- **`rv-server`** runs in a Docker container with `--restart unless-stopped`
  (no separate systemd unit; Docker handles the restart).
- **Postgres** runs on the same VM via the Cloud SQL Auth Proxy on
  `127.0.0.1:5432`. Same instance as `manuscript-studio`, separate database
  (`rv_trip`).

---

## 4. Repository layout

```
cmd/
  server/             cmd/server/main.go — the long-running HTTP server.
  add-user/           Upsert a user (one-shot CLI; runs inside the same
                      Docker image as the server).
internal/
  auth/               Password hashing (bcrypt) + DB-backed cookie sessions.
  config/             YAML loader (~/.config/rv-server/config.yaml).
  database/           pgx pool wrapper.
liquibase/
  changelog/          XML changesets. ALL schema changes go here — never
                      via raw SQL or Go.
config.example.yaml   Template copied on first install.sh run.
Dockerfile            Multi-stage build of the rv-server binaries.
Dockerfile.liquibase  Liquibase image with the changelog baked in.
install.sh            One-liner deploy: pulls source, builds images, runs
                      migrations, restarts the container.
```

---

## 5. Auth model

- **Passwords** are bcrypt-hashed and stored in the `user` table. The
  `password_hash` column is the only user-facing secret.
- **Sessions** are random 32-byte tokens stored in the `session` table.
  The cookie value IS the row's primary key. No JWT — DB lookup on every
  request (sliding-window update on every read; refresh only when the
  remaining lifetime drops under 7 days).
- **Cookie**: `rv_session`, `HttpOnly`, `Secure` (in production),
  `SameSite=Lax`, `Path=/rv/`. Path-scoping prevents the cookie from
  leaking to other apps on the same domain.
- **30-day TTL**, sliding. Configurable in `internal/auth/auth.go`.
- **Failed-login timing**: always do a bcrypt comparison even when the
  username doesn't exist, to avoid leaking user existence via timing.

## 6. Schema

Two tables (see `liquibase/changelog/001-initial-schema.xml`):

- `"user"` — `username` (PK), `password_hash`, `created_at`.
- `"session"` — `id` (PK = cookie token), `username` (FK), timestamps.
  Cascades on user delete.

Quote `"user"` in every SQL reference; it's a reserved word in some SQL
contexts.

## 7. Why mirror manuscript-studio

You already know the manuscript-studio deploy pattern (Docker image build
→ Liquibase migrations → restart container). Mirroring it here means:

- One mental model for both apps.
- Reuse the Cloud SQL Auth Proxy already running on the VM.
- Reuse the YAML config conventions, Apache reverse-proxy style, and the
  `install.sh` discipline (`SCRIPT_VERSION` bumps, `~/.config/<app>/logs/`).

When you fix something here that also applies there (or vice versa),
consider porting the fix.
