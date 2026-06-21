# Setup walkthrough: rv-server on the production VM

A first-time guide for Andrew. Follows the same flow as
manuscript-studio's install, with rv-specific paths/ports/db.

Most of this is **identical to what you already did for
manuscript-studio**. The only new bits are: a new database in Cloud SQL,
a new Apache `<Location>` block, a new VM systemd-via-Docker container.

## Prerequisites

Already on the VM (you set these up for manuscript-studio):
- Docker
- `psql` client
- Cloud SQL Auth Proxy running, exposing Postgres on `127.0.0.1:5432`
- Apache serving andrewcheong.com/rv (static files via rsync)

## Step 1 — Create the database

SSH to the VM (or use Cloud SQL Shell), then:

```bash
# Connect via the auth proxy (which is already running):
psql -h 127.0.0.1 -p 5432 -U postgres
```

Then in psql:

```sql
CREATE DATABASE rv_trip;

CREATE USER rv_user WITH PASSWORD '<PICK A STRONG PASSWORD>';
GRANT ALL PRIVILEGES ON DATABASE rv_trip TO rv_user;

-- For Postgres 15+, also grant schema privileges:
\c rv_trip
GRANT ALL ON SCHEMA public TO rv_user;
```

Save the password — you'll paste it into the config in step 2.

## Step 2 — Run the install script

On the VM:

```bash
deploy_latest_rv_server() {
  bash <(curl -sSL -H "Cache-Control: no-cache" \
    "https://raw.githubusercontent.com/slackwing/rv-server/main/install.sh")
}
deploy_latest_rv_server
```

First run: it'll write `~/.config/rv-server/config.yaml` and exit.

Edit `~/.config/rv-server/config.yaml` and set:
- `database.password` — the password you set in step 1
- `server.env: production` (already the default)

Re-run `deploy_latest_rv_server`. This time it'll:
- Test the DB connection
- Build the Docker images
- Run Liquibase to create the `user` and `session` tables
- Start the container (`docker run --restart unless-stopped`)

When done, verify:

```bash
curl -sS http://127.0.0.1:5002/healthz   # should print "ok"
docker ps | grep rv-server                # should show running
```

## Step 3 — Add the two users

```bash
docker run --rm --network host \
  -v "$HOME/.config/rv-server/config.yaml:/config/config.yaml:ro" \
  rv-server:latest \
  add-user --config /config/config.yaml abi beeboweebo

docker run --rm --network host \
  -v "$HOME/.config/rv-server/config.yaml:/config/config.yaml:ro" \
  rv-server:latest \
  add-user --config /config/config.yaml andrew quailtail
```

Each command upserts (insert or overwrite password). Re-run with a
different password later to rotate.

## Step 4 — Apache proxy block

Add to your andrewcheong.com vhost (probably `/etc/apache2/sites-enabled/000-default-le-ssl.conf`
or similar — wherever the existing `/rv` static config lives):

```apache
# rv-server backend (login + sessions)
ProxyRequests Off
ProxyPreserveHost On

<Location /rv/api/>
    ProxyPass        http://127.0.0.1:5002/api/
    ProxyPassReverse http://127.0.0.1:5002/api/
</Location>
```

Important notes:
- The trailing slash on **both sides** of `ProxyPass` matters.
- We're stripping the `/rv` prefix as the request crosses to the Go
  server (so `/rv/api/login` → `/api/login`).
- The Go server doesn't serve any static content. Apache continues
  serving `/rv/*` files (index.html, prep.html, assets/, etc.) directly.

Make sure `mod_proxy` and `mod_proxy_http` are enabled:
```bash
sudo a2enmod proxy proxy_http
```

Test config + reload:
```bash
sudo apachectl configtest
sudo systemctl reload apache2
```

## Step 5 — Verify end to end

From your laptop:

```bash
# 1. Healthcheck through Apache:
curl -sS https://andrewcheong.com/rv/api/me     # should be 401 Unauthorized

# 2. Log in:
curl -sS -c /tmp/cookies.txt -X POST \
  -H 'Content-Type: application/json' \
  -d '{"username":"abi","password":"beeboweebo"}' \
  https://andrewcheong.com/rv/api/login        # should be {"username":"abi"}

# 3. Check me with the cookie:
curl -sS -b /tmp/cookies.txt https://andrewcheong.com/rv/api/me
                                                # should be {"username":"abi"}

# 4. Log out:
curl -sS -b /tmp/cookies.txt -X POST \
  https://andrewcheong.com/rv/api/logout       # 204 No Content
```

Then load https://andrewcheong.com/rv in your browser:
- Top-right corner: a "Log in" button (left of °C/°F).
- The "prep checklist" link in the subtitle/footer should be HIDDEN
  until you log in.
- Click "Log in", enter `abi` / `beeboweebo`, modal closes, prep link
  appears, button now reads "abi · Log out".
- Click again → "Log out?" confirm → logs out.

## Future redeploys

Any time you push a change to `slackwing/rv-server` main:

```bash
deploy_latest_rv_server
```

The script:
1. Pulls latest source into `~/.config/rv-server/src/`
2. Rebuilds the Docker images
3. Runs Liquibase (idempotent — no-op if schema is current)
4. Restarts the container

## Troubleshooting

- **`curl localhost:5002/healthz` works, but `andrewcheong.com/rv/api/me`
  returns 404** → Apache config not applied. Run `apachectl configtest`
  and `systemctl reload apache2`.

- **`docker logs rv-server` shows DB connection refused** → Cloud SQL
  Auth Proxy isn't running, or the password in `config.yaml` is wrong.

- **Login returns 401 with the right credentials** → either the user
  wasn't added (check `psql -d rv_trip -c 'SELECT username FROM "user";'`)
  or the password is mistyped. Re-run `add-user` to reset.

- **Cookie isn't being set** → check the browser dev tools Network tab
  for the login response. `Set-Cookie` must be present. If the site is
  on HTTP rather than HTTPS in development, set `server.env: development`
  in config so the `Secure` flag is dropped.
