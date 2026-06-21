# Setup walkthrough: hobby-server on the production VM

First-time guide for Andrew. Follows the same flow as
manuscript-studio's install: Docker images, Liquibase, container runs
with `--restart unless-stopped`.

hobby-server is **multi-project**: this guide covers setting up the
**rv** project, but the same steps apply for each new project (just
swap names).

Most of this should be familiar from manuscript-studio. The pieces
that are new (or different):

- One config file lists multiple projects under `projects:`.
- One Liquibase pass per project (against that project's DB).
- One Apache `<Location>` block per project.

## Prerequisites

Already on the VM (you set these up for manuscript-studio):
- Docker
- `psql` client
- `python3` + `python3-yaml` (PyYAML) — install.sh uses it to parse
  the config. `apt install python3-yaml` or `pip3 install pyyaml`.
- Cloud SQL Auth Proxy running, exposing Postgres on `127.0.0.1:5432`
- Apache serving andrewcheong.com/rv (static files via rsync)

## Step 1 — Create the rv project's database

SSH to the VM (or use Cloud SQL Shell):

```bash
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

Save the password.

## Step 2 — Run the install script

On the VM:

```bash
deploy_latest_hobby_server() {
  bash <(curl -sSL -H "Cache-Control: no-cache" \
    "https://raw.githubusercontent.com/slackwing/hobby-server/main/install.sh")
}
deploy_latest_hobby_server
```

First run: writes `~/.config/hobby-server/config.yaml` (a template
listing only the rv project) and exits.

Edit `~/.config/hobby-server/config.yaml`:
- Under `projects.[name=rv].database.password`: paste the password
  from step 1.
- `server.env: production` (already the default).

Re-run `deploy_latest_hobby_server`. This time:
- Tests the rv DB connection
- Builds the Docker images
- Runs Liquibase against `rv_trip` (creates `user`, `session` tables)
- Starts the container (`docker run --restart unless-stopped`)

When done, verify:

```bash
curl -sS http://127.0.0.1:5002/healthz   # → "ok"
docker ps | grep hobby-server             # → running
docker logs hobby-server | tail -20       # see 'project "rv" ready'
```

## Step 3 — Add the two RV users

```bash
docker run --rm --network host \
  -v "$HOME/.config/hobby-server/config.yaml:/config/config.yaml:ro" \
  hobby-server:latest \
  add-user --config /config/config.yaml --project rv abi beeboweebo

docker run --rm --network host \
  -v "$HOME/.config/hobby-server/config.yaml:/config/config.yaml:ro" \
  hobby-server:latest \
  add-user --config /config/config.yaml --project rv andrew quailtail
```

Each command upserts (insert or overwrite password). To rotate, re-run
with the new password.

## Step 4 — Apache proxy block for the rv project

The frontend calls `/rv/api/*`. The server mounts the rv project at
`/api/rv/*`. So Apache needs to map: public `/rv/api/` → backend
`/api/rv/`.

Add to your andrewcheong.com vhost (probably
`/etc/apache2/sites-enabled/000-default-le-ssl.conf` or similar):

```apache
# hobby-server backend (rv project)
ProxyRequests Off
ProxyPreserveHost On

<Location /rv/api/>
    ProxyPass        http://127.0.0.1:5002/api/rv/
    ProxyPassReverse http://127.0.0.1:5002/api/rv/
</Location>
```

Notes:
- Trailing slashes on **both sides** of `ProxyPass` matter.
- We strip the public `/rv` prefix and add the backend `/api/rv`
  prefix as the request crosses to the Go server (so
  `/rv/api/login` → `/api/rv/login`).
- The Go server doesn't serve static content. Apache continues serving
  `/rv/*` files (index.html, prep.html, assets/, etc.) directly.

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
# 1. /me without cookie → 401
curl -sS https://andrewcheong.com/rv/api/me

# 2. Log in:
curl -sS -c /tmp/cookies.txt -X POST \
  -H 'Content-Type: application/json' \
  -d '{"username":"abi","password":"beeboweebo"}' \
  https://andrewcheong.com/rv/api/login
# → {"username":"abi"}

# 3. /me with cookie:
curl -sS -b /tmp/cookies.txt https://andrewcheong.com/rv/api/me
# → {"username":"abi"}

# 4. Log out:
curl -sS -b /tmp/cookies.txt -X POST \
  https://andrewcheong.com/rv/api/logout
# → 204 No Content
```

Then load https://andrewcheong.com/rv in your browser:
- Top-right corner: "Log in" button (left of °C/°F)
- "Prep checklist" link hidden until you log in
- Log in with `abi` / `beeboweebo` → modal closes, prep link appears,
  button reads "abi · Log out"
- Click → "Log out?" confirm → logs out

## Adding a second project later

Say you start `next_thing`. Steps:

1. **DB**: `CREATE DATABASE next_thing_db;` + user + grants (same as
   step 1 above).
2. **Schema**: in this repo, `mkdir -p liquibase/next_thing/changelog/`
   + `db.changelog-master.xml` + at least one changeset (copy the
   `liquibase/rv/changelog/` template).
3. **Push** the new schema dir to GitHub main.
4. **Edit** `~/.config/hobby-server/config.yaml` and append:
   ```yaml
     - name: next_thing
       database:
         host: "127.0.0.1"
         port: 5432
         name: "next_thing_db"
         user: "next_thing_user"
         password: "<from step 1>"
       url_prefix: "/api/next_thing"
       cookie_path: "/next_thing/"
   ```
5. **Re-run** `deploy_latest_hobby_server` on the VM. Liquibase will
   migrate the new DB; the server will pick up the new project on
   startup.
6. **Add users**: `add-user --project next_thing <user> <pass>`
7. **Apache**: add a new `<Location /next_thing/api/>` block matching
   the rv pattern but with `/api/next_thing/` as the backend.

## Future redeploys

```bash
deploy_latest_hobby_server
```

The script:
1. Pulls latest source into `~/.config/hobby-server/src/`
2. Rebuilds the Docker images
3. Runs Liquibase per project (idempotent — no-op if up-to-date)
4. Restarts the container

## Troubleshooting

- **`curl localhost:5002/healthz` works, but `andrewcheong.com/rv/api/me`
  returns 404** → Apache config not applied. Run `apachectl configtest`
  and `systemctl reload apache2`.

- **`docker logs hobby-server` shows DB connection refused for project
  X** → Cloud SQL Auth Proxy isn't running, or the password in
  `config.yaml` for that project is wrong.

- **Login returns 401 with the right credentials** → either the user
  wasn't added (check `psql -d <project_db> -c 'SELECT username FROM "user";'`)
  or the password is mistyped. Re-run `add-user --project ...` to
  reset.

- **Cookie isn't being set** → check the browser dev tools Network tab
  for the login response. `Set-Cookie` must be present. If the site is
  HTTP rather than HTTPS in development, set `server.env: development`
  in config so the `Secure` flag is dropped.

- **install.sh fails on `python3-yaml`** → install it with
  `sudo apt install python3-yaml` (Debian/Ubuntu) or
  `pip3 install pyyaml`.
