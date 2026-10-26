# PLAN: port the sxiva status dashboard API into hobby-server

Status: approved by Andrew 2026-10-26, not started. Delete this file when
step 10 is done.

## 0. What and why (read this first)

The status dashboard at andrewcheong.com/status/ is a static page
(feathers `foundry/website/html/status/`, already rewritten on Lightweight
Charts — do NOT touch it) that calls `/status/api/*`. Today that API is a
**Flask app in its own Docker container on the VM** (`sxiva-dashboard-api`,
:5000) backed by a **TimescaleDB container** (`sxiva-timescaledb`, pg16).
Source: feathers `11.sxiv/dashboard/server/app.py` (~700 lines) and
`11.sxiv/dashboard/docker/liquibase/changelog/*.xml`.

Goal: the API becomes a hobby-server project named **`sxiva`** (Go, in
this repo), its data moves to a new **Cloud SQL** database `sxiva`, the
two sxiva containers are retired. Public URLs stay exactly the same, so
the page and the `sxiva upload` CLI keep working.

Decisions already made (don't relitigate):
- **Go, not Flask.** hobby-server is one Go binary; there is no sidecar
  mechanism. The port is ~400 lines.
- **Plain Postgres table, no TimescaleDB.** Cloud SQL can't load the
  extension, and the only Timescale feature ever used was
  `create_hypertable` on a one-row-per-day table (329 rows). Nothing is
  lost.
- **No users, no auth floor, no admin user.** Reads are public; the one
  write (`POST sync/daily`) and `GET status/last-sync` are gated by a
  bearer token in the project's config block. sxiva is the first
  project with no login at all.
- **Table prefix `sxiva_`**, package `internal/sxiva`, URL prefix
  `/api/sxiva`, public path unchanged `/status/api/*`.
- **Rotate the token.** The current one is hardcoded as a default in
  feathers `11.sxiv/tools/sxiva/sync.py` (a public repo). Treat it as
  burned; never print any token in a transcript or commit.
- **Fix the slow query while porting.** `category-rolling-sum` takes
  ~5 s because of O(n²) correlated subqueries; compute it in Go over the
  rows fetched once (same numbers, milliseconds).

Rules from AGENTS.md that bite here: N1 schema only via Liquibase; N4
never log secrets; N5 cross-repo coordination (feathers changes are
listed below); N7 `sxiva_` prefix on every table. If you touch
`install.sh`, bump `SCRIPT_VERSION` (N2) — this plan shouldn't need to.

Access: `ssha` (loads the GCP key into the agent) then `ws_ssh` for the
VM (passwordless sudo). The Cloud SQL host/port are whatever the existing
projects use in the VM's `~/.config/hobby-server/config.yaml` (`host:`
under `database:`). Never `cat` that file — it holds passwords; use
`grep -v -i 'password\|token'`.

## 1. Schema — `liquibase/sxiva/changelog/`

Create `db.changelog-master.xml` + `001-initial-schema.xml` (copy the
XML skeleton from `liquibase/bap/changelog/`). Two tables, columns and
types copied EXACTLY from the Flask changelogs so the CSV copy in step 6
is lossless (read all seven files in feathers
`11.sxiv/dashboard/docker/liquibase/changelog/` — 003 adds `meet`, 005
makes `exe` NUMERIC(2,1), 006 adds `abi`/`save`, 007 adds the drc
columns):

`sxiva_daily_summary`
- `date DATE` PK
- `day_of_week CHAR(1) NOT NULL` (one of U M T W R F S)
- `category_minutes JSONB NOT NULL DEFAULT '{}'` — `{"bkc": 40, ...}`
  minutes per category code
- `sleep_score INTEGER`, `sleep_hours NUMERIC(2,1)`
- `dep_min`, `dep_max`, `dep_avg NUMERIC(2,1)`
- `soc INTEGER`, `out INTEGER`, `exe NUMERIC(2,1)`, `xmx INTEGER`
- `dist`, `alc`, `wea NUMERIC(2,1)`
- `meet INTEGER` (work meeting minutes — counted as work)
- `abi NUMERIC(4,1)`, `save INTEGER`
- `drc_triggers`, `drc_resisted`, `drc_indulged NUMERIC(5,1)`
- `created_at`, `updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`

No hypertable, no GIN index (329 rows). The PK is the only index.

`sxiva_sync_metadata`
- `id INTEGER` PK, `last_sync_timestamp TIMESTAMPTZ NOT NULL`,
  `last_sync_file_count INTEGER DEFAULT 0`, `notes TEXT`
- seed one row: `id=1, last_sync_timestamp='1970-01-01 00:00:00+00',
  last_sync_file_count=0` (the CSV copy in step 6 overwrites it).

## 2. Config — `internal/config/config.go`

Add to `Project`:

```go
// SyncToken gates the sxiva write endpoints (bearer token the sxiva
// CLI sends). Empty = those endpoints answer 503.
SyncToken string `yaml:"sync_token"`
```

Add a commented `sxiva` example block to `config.example.yaml` next to
the others (`url_prefix: "/api/sxiva"`, `cookie_path: "/status/"` —
unused but required by the loader — and `sync_token: "REPLACE_ME"`).

## 3. Code — `internal/sxiva/sxiva.go` (+ `sxiva_test.go`)

Mirror `internal/bap/bap.go`'s shape: `Store{pool}`, `NewStore`,
`Mount(r chi.Router, store *Store, token string)`. Handlers write JSON
with the same field names Flask uses (the page depends on them). Null
columns → JSON `null` (`*float64`), never 0, except where noted.

### 3.1 Public reads

All take `limit` (positive int, else 400) = how many of the MOST RECENT
rows to return; the response lists them in chronological order. "Rows"
means rows in `sxiva_daily_summary` ordered by date — a missing calendar
day is simply not a row, and all windows below count ROWS, not days,
unless stated.

`GET /category-rolling-sum?hobby=a,b,c&work=sp&days=7&limit=30&lambda=0.2`
- `hobby`, `work`: comma lists of category codes; 400 if both empty.
  `days` default 7, `limit` default 30, `lambda` default 0.2; `days`
  and `limit` must be ≥ 1.
- Per row compute, from `category_minutes` (values are ints) and `meet`:
  - `hobby_minutes` = sum of values whose key is in `hobby`
  - `work_minutes` = sum of values whose key is in `work` + `meet` (null→0)
  - `other_minutes` = sum of values whose key is in NEITHER list (`meet`
    is not part of other)
  - `total_minutes` = sum of all values + `meet`
- Then per row:
  - `hobby_raw`, `other_raw`, `total_raw` = sum of that quantity over
    the current row and the `days-1` preceding rows (int)
  - `hobby_weighted` = over rows with `row.date <= d` and
    `row.date > d - days` **calendar days** (d = current date):
    `Σ hobby_minutes·w / Σ w · days` with `w = exp(-lambda·(d - row.date))`
    (difference in whole days); rounded to 1 decimal, half away from zero
  - `work_raw` = sum of `work_minutes` over the last 5 WEEKDAY rows with
    date ≤ d (Mon–Fri by the date's day of week; the current row counts
    only if it is a weekday), int
  - `work_weighted` = over those same ≤5 weekday rows:
    `Σ work_minutes·w / Σ w · 5`, same `w`, rounded to 1 decimal; 0 if
    there are no such rows
- Response: `{"data": [{date, hobby_raw, hobby_weighted, work_raw,
  work_weighted, other_raw, total_raw}...], "hobby_categories": [...],
  "work_categories": [...], "other_categories": [...], "window_days": N,
  "decay_lambda": x}` where the three category lists are the DISTINCT
  keys across all rows' `category_minutes`, sorted, partitioned by the
  two input lists (other = in neither). Raw fields are ints, weighted
  are floats.
- Implementation: one query `SELECT date, category_minutes, meet FROM
  sxiva_daily_summary ORDER BY date`, then pure Go. Put the math in a
  function like `rollingCategory(rows, hobby, work, days, lambda)` so
  it's unit-testable without a DB.

`GET /alcohol-depression?limit=60`
- Per row: `alc_value` = `alc` (null→0); `dep_raw` = `dep_avg` (nullable).
- `alc_7day_sum` = sum of `alc_value` over current + 6 preceding rows.
- `dep_7day_avg` = mean of the NON-null `dep_avg` over current + 6
  preceding rows; null if all seven are null.
- `alc_15day_avg` = mean of `alc_7day_sum` over current + 14 preceding
  rows.
- Response `{"data": [{date, alc_7day_sum, alc_15day_avg, dep_raw,
  dep_7day_avg}]}`; the two alc fields are floats (0.0 when null), the
  dep fields float-or-null.

`GET /sleep-score?limit=60`
- `sleep_raw` = `sleep_score` (nullable); `sleep_7day_avg` = mean of the
  non-null `sleep_score` over current + 6 preceding rows (null if none).
- Response `{"data": [{date, sleep_raw, sleep_7day_avg}]}`, floats or null.

`GET /drc?limit=60`
- Raw columns. Response `{"data": [{date, drc_triggers, drc_resisted,
  drc_indulged}]}`, floats or null.

`GET /health` → `{"status":"healthy","database":"connected"}` after a
`SELECT 1`, else 500.

### 3.2 Token-gated (bearer)

`Authorization: Bearer <token>` compared with `SyncToken` using
`crypto/subtle.ConstantTimeCompare`; missing/wrong → 401
`{"error":"Unauthorized"}`; `SyncToken` empty → 503.

`POST /sync/daily` — body is the JSON the CLI sends, one day:
`date` (required, "YYYY-MM-DD"), `day_of_week` (required),
`category_minutes` (object, default `{}`), and the optional numeric
fields `sleep_score, sleep_hours, dep_min, dep_max, dep_avg, dist, soc,
out, exe, alc, xmx, wea, meet, abi, save, drc_triggers, drc_resisted,
drc_indulged` (absent or null → NULL). Upsert with `INSERT ... ON
CONFLICT (date) DO UPDATE SET <every column> = EXCLUDED.<column>,
updated_at = NOW()` (copy the column list from Flask's `sync_daily`;
pass `category_minutes` as `$n::jsonb`), then `UPDATE
sxiva_sync_metadata SET last_sync_timestamp = NOW(),
last_sync_file_count = last_sync_file_count + 1 WHERE id = 1`. Both in
one transaction. Reply 200 `{"status":"success","date":...,
"message":"Data synced successfully"}`; bad JSON / missing fields → 400.

`GET /status/last-sync` → `{"last_sync_timestamp": "<RFC3339>",
"last_sync_file_count": N}`.

### 3.3 Wiring — `cmd/server/main.go`

Follow the `bap` pattern: capture the `sxiva` project + pool in the
project loop with `continue` (data only, no auth floor), log
`auth=token`, and mount inside the timed group:

```go
if sxivaProject != nil {
    r.Route(sxivaProject.URLPrefix, func(sub chi.Router) {
        sxiva.Mount(sub, sxiva.NewStore(sxivaPool), sxivaProject.SyncToken)
    })
}
```

Unlike hxh/bap it must NOT require `adminStore`.

### 3.4 Tests

`sxiva_test.go`: pure tests for the three rolling computations with a
small fixture (~12 rows spanning two weekends and one missing day),
asserting hand-computed values — in particular: windows count rows not
days; `work_raw` uses the last 5 weekday rows; `hobby_weighted` uses
calendar days; `meet` lands in work and total but not other; nulls stay
null in the dep/sleep averages. `go test ./...` and `gofmt -l .` clean.

### 3.5 Docs in this repo

Add `sxiva` to the "Currently hosted projects" lists in README.md,
CLAUDE.md, ARCHITECTURE.md and AGENTS.md (README also gets the endpoint
list). Commit + push to main. **Push before step 5** — `install.sh`
refuses to run if a configured project has no `liquibase/<name>/changelog`.

## 4. Cloud SQL database (on the VM)

Using the `hobby_server` role (it has CREATEDB; its password is the
`admin` project's in config.yaml — read it into a variable without
printing it):

```sql
CREATE USER sxiva_user WITH PASSWORD '<openssl rand -hex 24>';
CREATE DATABASE sxiva OWNER sxiva_user;
-- then \c sxiva
GRANT ALL ON SCHEMA public TO sxiva_user;
```

Add the project to the VM's `~/.config/hobby-server/config.yaml`
(back it up first: `cp config.yaml config.yaml.bak-sxiva`; insert the
block by text, don't round-trip the file through a YAML dumper — it
would drop the comments):

```yaml
  - name: sxiva
    database:
      host: "<same host as the other projects>"
      port: 5432
      name: "sxiva"
      user: "sxiva_user"
      password: "<the one above>"
    # Public path is andrewcheong.com/status/api/* ; Apache proxies it
    # to 127.0.0.1:5002/api/sxiva/* . No login: reads are public, the
    # sxiva CLI writes with sync_token.
    url_prefix: "/api/sxiva"
    cookie_path: "/status/"
    sync_token: "<openssl rand -hex 32>"
```

## 5. Deploy hobby-server

Take the shared deploy lock (`~/.config/my/deploy-lock.sh wait
sxiva-port`), run `deploy_latest_hobby_server` on the VM (the one-liner
in CLAUDE.md), release the lock. Liquibase creates the two tables;
`docker logs hobby-server` should show `project "sxiva" ready`. Check:
`curl -s 127.0.0.1:5002/api/sxiva/health` on the VM.

Flask is still serving the public URL at this point — nothing user-facing
has changed.

## 6. Copy the data

1. On the laptop run `sxiva upload` so the Flask DB is current, then
   don't upload again until step 8 is done (the CLI is the only writer).
2. On the VM, export with COPY (a `pg_dump` of a hypertable emits
   Timescale chunk tables — don't use it):
   ```
   COLS="date, day_of_week, category_minutes, sleep_score, sleep_hours, dep_min, dep_max, dep_avg, soc, out, exe, xmx, dist, alc, wea, meet, abi, save, drc_triggers, drc_resisted, drc_indulged, created_at, updated_at"
   docker exec sxiva-timescaledb psql -U sxiva_user -d sxiva_stats \
     -c "\copy (SELECT $COLS FROM daily_summary ORDER BY date) TO STDOUT WITH CSV HEADER" \
     > ~/backups/sxiva-daily_summary-$(date +%F).csv
   docker exec sxiva-timescaledb psql -U sxiva_user -d sxiva_stats \
     -c "\copy (SELECT id, last_sync_timestamp, last_sync_file_count, notes FROM sync_metadata) TO STDOUT WITH CSV HEADER" \
     > ~/backups/sxiva-sync_metadata-$(date +%F).csv
   ```
   (the Timescale container's password is in
   `/var/www/dashboard/docker/.env`; `docker exec` as the pg user
   doesn't need it).
3. Import into Cloud SQL (as `sxiva_user`, `PGPASSWORD` from the config,
   not echoed): `\copy sxiva_daily_summary ($COLS) FROM '<csv>' WITH CSV
   HEADER`; for sync_metadata `DELETE FROM sxiva_sync_metadata;` then the
   same `\copy`.
4. Verify on BOTH sides with the same expression (only the table name
   differs):
   ```sql
   SELECT count(*), min(date), max(date),
          md5(string_agg(concat_ws('|', date, day_of_week, category_minutes::text,
            sleep_score, sleep_hours, dep_min, dep_max, dep_avg, soc, out, exe, xmx,
            dist, alc, wea, meet, abi, save, drc_triggers, drc_resisted, drc_indulged),
            '\n' ORDER BY date))
   FROM <table>;
   ```
   Count (329 as of 2026-10-26, more by now), dates and md5 must match.
   The CSVs stay in `~/backups/` as the pre-cutover backup.

## 7. Prove the Go API equals the Flask API

Both are live on the VM now (`:5000/api/...` and `:5002/api/sxiva/...`).
Write a small Python script (on the VM, `/tmp` is fine) that fetches each
of the four read endpoints from both with the SAME query string and
`limit=5000` and compares the parsed JSON:

- `category-rolling-sum`: `hobby=<the HOBBY_CATEGORIES list from
  feathers html/status/js/config.js, comma-joined>&work=sp&days=7&lambda=0.5`
  — this is exactly what the page sends
- `alcohol-depression`, `sleep-score`, `drc`: no extra params

Compare numerically: ints exact; floats within 1e-6, except the two
`*_weighted` fields within 0.1 (Postgres and Go can round a value sitting
exactly on a .x5 boundary differently). Keys, row counts, dates and
the three category lists must match exactly. Fix the Go until the diff
is empty. Do not cut over with a non-empty diff.

## 8. Cutover (Apache)

`/etc/apache2/conf-available/andrewcheong.conf` has:

```
<Location /status/api>
    ProxyPass http://127.0.0.1:5000/api
    ProxyPassReverse http://127.0.0.1:5000/api
```

Back the file up, change both lines to `http://127.0.0.1:5002/api/sxiva`,
`sudo apachectl configtest`, `sudo systemctl reload apache2`. Then:

- `curl -s https://andrewcheong.com/status/api/sleep-score?limit=2`
  returns data (now from Go).
- From the laptop run the page's check suite against prod:
  `node ~/src/feathers/foundry/website/docs/screenshots/status-2026-10-09/check_new.mjs /tmp/shots`
  (needs playwright-core from `~/src/manuscript-studio/node_modules` and
  system Chrome; it drives the live page and asserts the charts) — ALL OK.
- Load andrewcheong.com/status/ in a browser: the three category charts
  should now appear in well under a second instead of ~5 s.

Rollback at any point: put `:5000/api` back in those two lines and
reload Apache (Flask is untouched until step 10).

## 9. The CLI and the new token (feathers repo, public)

In `11.sxiv/tools/sxiva/sync.py`:
- Delete the hardcoded token default. Token = `SXIVA_API_TOKEN` env var,
  else the contents of `~/.sxiva/token` (strip whitespace), else fail
  with a clear message. (`~/.sxiva/` is deliberately outside `~/.config`,
  which is a git repo.)
- Make the base URL the API root: default
  `https://andrewcheong.com/status/api`, paths `/sync/daily` and
  `/status/last-sync` (today the code appends `/api/...` to a base that
  ends in `/status`). Update the `--local` option in `cli.py` to
  `http://127.0.0.1:5002/api/sxiva`.
- Fix the docstring/README mentions.

On the laptop: `mkdir -p ~/.sxiva && (umask 077; printf '%s\n' '<token>' > ~/.sxiva/token)`.
Test: `sxiva upload` → prints synced/skipped counts without errors;
`curl -s -H "Authorization: Bearer $(cat ~/.sxiva/token)"
https://andrewcheong.com/status/api/status/last-sync` shows a fresh
timestamp. Commit + push feathers (verify with `git diff` that no token
is in the diff).

## 10. Decommission the old stack

Only after steps 7–9 are green.

- VM: `cd /var/www/dashboard/docker && docker compose down` (removes
  the two containers; the `timescaledb-data` volume stays — leave it two
  weeks, then `docker volume rm`). `docker ps` shows only
  manuscript-studio-server and hobby-server.
- `~/.config/my/myrc2.sh`: remove `dashboard_sync` and `start_local`;
  `~/.config/my/CLAUDE.md`: drop `dashboard_sync` from "Other syncs".
  Commit + push `~/.config`.
- feathers: `git rm -r 11.sxiv/dashboard/server 11.sxiv/dashboard/docker
  11.sxiv/dashboard/deploy`; rewrite `11.sxiv/dashboard/README.md` to
  say the API is hobby-server's `sxiva` project. Keep
  `tools/sxiva/` (parser + sync client). Commit + push.
- Memory (`~/.claude/projects/-home-slackwing--config/memory/`): update
  `gcp-infrastructure-map.md` (containers, Apache map, new DB),
  `status-dashboard-rewrite.md` (API now hobby-server, load time fixed),
  `sxiva-dashboard-category-lists.md` (unchanged paths; mention
  `~/.sxiva/token`).
- Delete this PLAN.md.

## Order of operations, condensed

1 schema → 2 config → 3 code + tests + docs → push → 4 Cloud SQL DB +
VM config → 5 deploy → 6 upload, copy, checksum → 7 golden diff →
8 Apache cutover + page check → 9 CLI token → 10 decommission.

Steps 1–3 are pure local work in this repo; nothing on the VM changes
until step 4, and nothing user-visible until step 8.
