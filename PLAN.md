# PLAN: Netflix roles watcher (status page, admin-only)

Status: approved by Andrew 2026-10-26, executing the same day. Delete this
file when §6 is done. Changed mid-build (Andrew): one COMBINED list with
a source mark per card (§4), gone roles in red for a week, and a toggle
that opens everything (hidden and past roles).

## 0. What and why

A small dashboard on andrewcheong.com/status/, right under the Sabbatical
'27 card, that tracks open **Netflix Software Engineer 5 and 6** roles.
Visible only to a signed-in admin (Andrew is the only one). A job on the
server checks two sources hourly and catalogs what it finds.

Decisions (Andrew, 2026-10-26):
- **Build plainly** — no need to hide "Netflix" in code or commits.
- **Sources:** the Netflix careers site and LinkedIn's **public, no-login**
  job search (never Andrew's LinkedIn account). No third source (every
  aggregator mirrors the same postings).
- **Levels:** SE5 and SE6 only; drop SE4 everywhere. Titles spanning a
  level ("Software Engineer 4/5") count when they include 5 or 6; so do
  "(L5)"/"(L6)" titles.
- **Salary range** from both sources.
- One combined list, each card marked with its source(s); simple,
  glanceable.

## 1. Sources (verified 2026-10-26)

**Careers site** (Eightfold JSON — what the site's own page calls):
- List: `GET https://explore.jobs.netflix.net/api/apply/v2/jobs?domain=netflix.com&start=N&num=10&query=software%20engineer%205&Work%20Type=remote&sort_by=relevance`
  (and `…%206`) → `{count, positions: [{id, name, location, t_create,
  canonicalPositionUrl}]}`. Page with `start` until `count`/empty (cap 15
  pages). `Work Type=remote` is Andrew's filter from his URL.
- Detail (salary): `GET …/api/apply/v2/jobs/{id}?domain=netflix.com` →
  `job_description` HTML: "The range for this role is $320,000.00 - $500,000".
- Posted = `t_create` (epoch seconds).

**LinkedIn** (guest endpoint behind the public jobs search page):
- List: `GET https://www.linkedin.com/jobs-guest/jobs/api/seeMoreJobPostings/search?keywords=Software%20Engineer%205&f_C=165158&geoId=103644278&start=N`
  (and `…%206`) → HTML job cards (10 per page; cap 10 pages):
  `data-entity-urn="urn:li:jobPosting:<id>"`, `base-search-card__title`,
  `job-search-card__location`, `<time datetime="YYYY-MM-DD">`, link
  href. `f_C=165158` = Netflix, `geoId=103644278` = United States (from
  Andrew's URL; New York roles show their location).
- Detail (salary): `GET https://www.linkedin.com/jobs-guest/jobs/api/jobPosting/{id}`
  → description with the same "The range for this role is …" sentence.
- Be polite: browser UA, ~1 s between requests; a 429/999/non-200 marks
  the source's fetch failed and leaves its catalog untouched.

**Salary**: parse the first `$X - $Y` range in the description text (`-`,
`–`, `—` or `to`); store whole dollars. Only fetched for listings never
seen before (one detail request per new role).

## 2. Schema — `liquibase/sxiva/changelog/002-jobs.xml` (sxiva DB)

- `sxiva_job_role` — `role_id UUID` PK = **UUIDv5(namespace, normalized
  title)** (lowercase, dashes unified, whitespace collapsed), so the same
  role is recognized whenever and wherever it reappears; `title`,
  `favorite BOOL`, `hidden BOOL`, `first_seen_at`, `updated_at`. Hide and
  favorite live here — they follow the role across both sources.
- `sxiva_job_listing` — PK `(source, external_id)`; `role_id` → role,
  `title`, `location`, `url`, `posted_at TIMESTAMPTZ`, `salary_min`,
  `salary_max` (nullable ints), `first_seen_at`, `last_seen_at`,
  `active BOOL` (seen in the source's last successful fetch).
- `sxiva_job_fetch` — one row per source: `checked_at`, `ok`, `error`,
  `found`.

## 3. Server — `internal/sxiva/jobs*.go` (hobby-server)

- Pure parts, unit-tested with real fixtures: UUIDv5 (checked against
  Python's `uuid.uuid5`), title levels (`Software Engineer(ing) 5`,
  `4/5`, `(L5)`), salary parsing, careers JSON + LinkedIn HTML parsing.
- `Watcher`: fetch both sources at startup (+30 s) and every hour; per
  source: list → keep SE5/6 → upsert roles + listings → details (salary)
  for new listings → mark unseen listings inactive (only after a
  successful list fetch).
- Endpoints under `/api/sxiva` (public `/status/api`), all
  `shared.RequireAdmin`:
  - `GET /jobs` → `{roles: [{role_id, title, favorite, hidden, posted_at,
    gone_at, salary_min, salary_max, locations, postings: [{source, url,
    location, posted_at, gone_at}]}], checks: [...]}` — every role ever
    seen, sources combined; posted_at = earliest live posting, gone_at
    set once every source dropped it.
  - `PATCH /jobs/roles/{role_id}` `{favorite?, hidden?}` → 204.
  - `POST /jobs/refresh` → 202, runs a fetch now.
  The session cookie is SameSite=Lax, so cross-site writes can't ride it.
- Docs: README endpoints, CLAUDE.md sxiva entry.

## 4. Page — `html/status/netflix-roles.js` (feathers)

A second plugin, inserted right after the Sabbatical card. On load it asks
`/admin/api/me`; only for an admin does it fetch `/status/api/jobs` and
render. Logged out: nothing, plus a small "sign in" link in the footer
(→ `/admin/`; the SSO cookie covers /status).

One list for both sources (a role on both is one card), each row:
- ★ (favorite toggle) · short title (`Software Engineer` → `SE`, so
  "SE5 – iOS"), linked to the posting · source mark(s) (careers site /
  LinkedIn) · days since posted ("today", "1d", "12d") · salary
  ("$320–500K") · location when not remote.
- Newest posting first. Posted today or yesterday → highlighted.
- Gone roles stay, in red, for a week after they disappear.
- × hides a role. An "all" toggle opens everything: hidden roles and
  every past role, to unhide or look back.
- Per source: when it was last checked; a failed check shows quietly.

## 5. Verify

- `go test ./internal/sxiva/` (fixtures, UUIDv5 vectors, levels, salary).
- One live fetch locally into a throwaway Postgres; inspect the catalog.
- Deploy hobby-server; `GET /status/api/jobs` → 401 logged out.
- Page: `check_new.mjs` logged out (no card); a Playwright run with the
  two API calls intercepted (admin `me` + real catalog data) to check the
  rendering, hide/favorite requests, highlight and order. Andrew checks
  signed in.

## 6. Done

Memory note, remove this plan.
