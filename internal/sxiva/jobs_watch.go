package sxiva

// The Netflix roles watcher's fetching and storage (pure parts: jobs.go).
// Hourly, per source: list SE5 + SE6 postings, keep the wanted levels,
// save them (new roles, postings that came back, postings that are gone),
// then read posting pages for the description, details, salary range and
// (Netflix) the displayed posting date. A failed list fetch changes nothing
// but the source's last-check row.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
)

const (
	jobsUserAgent    = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0 Safari/537.36"
	jobsCompany      = "Netflix"
	jobsLinkedInCo   = "165158"    // LinkedIn company id (f_C)
	jobsLinkedInGeo  = "103644278" // United States (geoId)
	jobsNetflixPages = 15
	jobsLinkedInPage = 10 // cards per page
	jobsLinkedInMax  = 10 // pages
	jobsStartDelay   = 30 * time.Second
	// LinkedIn pages read per run: new postings fill in over a few hours
	// instead of in one burst LinkedIn would answer with 429s.
	jobsLinkedInDetails = 10
)

// jobsQueries: one search per wanted level on each source.
var jobsQueries = []string{"Software Engineer 5", "Software Engineer 6"}

// JobWatcher fetches the sources; one run at a time.
type JobWatcher struct {
	store        *Store
	client       *http.Client
	netflixBase  string
	linkedInBase string
	pause        time.Duration   // between requests, to be polite
	linkedInWait time.Duration   // LinkedIn rate-limits quickly: slower still
	backoff      []time.Duration // waits before retrying a 429
	running      atomic.Bool
}

func NewJobWatcher(store *Store) *JobWatcher {
	jar, _ := cookiejar.New(nil) // keep the cookies a site hands out, like a browser
	return &JobWatcher{
		store:        store,
		client:       &http.Client{Timeout: 20 * time.Second, Jar: jar},
		netflixBase:  "https://explore.jobs.netflix.net",
		linkedInBase: "https://www.linkedin.com",
		pause:        time.Second,
		linkedInWait: 6 * time.Second,
		backoff:      []time.Duration{15 * time.Second, 45 * time.Second},
	}
}

// Run checks the sources shortly after startup and then every interval.
func (w *JobWatcher) Run(ctx context.Context, interval time.Duration) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(jobsStartDelay):
	}
	w.RunOnce(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.RunOnce(ctx)
		}
	}
}

// Trigger starts a run in the background (no-op if one is running).
func (w *JobWatcher) Trigger() {
	go w.RunOnce(context.Background())
}

// RunOnce checks every source.
func (w *JobWatcher) RunOnce(ctx context.Context) {
	if !w.running.CompareAndSwap(false, true) {
		return
	}
	defer w.running.Store(false)
	// Netflix: every live posting's page each run — the posting date the site
	// shows is only there, and a repost moves it. LinkedIn: each posting's
	// page once (its list already dates it), a few per run.
	// (Netflix's search is already remote-only; LinkedIn's covers every US
	// office, so its postings are kept to New York + country-wide.)
	w.runSource(ctx, SourceNetflix, w.listNetflix, w.netflixDetail, nil, true, 0)
	w.runSource(ctx, SourceLinkedIn, w.listLinkedIn, w.linkedInDetail, wantedLinkedInPlace, false, jobsLinkedInDetails)
}

func (w *JobWatcher) runSource(ctx context.Context, source string,
	list func(context.Context) ([]Listing, error), detail func(context.Context, Listing) (Detail, error),
	place func(string) bool, refresh bool, maxDetails int) {
	all, err := list(ctx)
	if err != nil {
		log.Printf("[jobs] %s: %v", source, err)
		if rerr := w.store.RecordJobFetch(source, false, err.Error(), 0); rerr != nil {
			log.Printf("[jobs] %s: record fetch: %v", source, rerr)
		}
		return
	}
	seen := map[string]bool{}
	var (
		kept      []Listing
		unwanted  []string // an unwanted role (iOS…): dropped, stored or not
		elsewhere []string // shown in an unwanted place this time: see SaveListings
	)
	for _, l := range all {
		if !wantedLevel(l.Title) || seen[l.ExternalID] {
			continue
		}
		seen[l.ExternalID] = true
		switch {
		case unwantedRole(l.Title):
			unwanted = append(unwanted, l.ExternalID)
		case place != nil && !place(l.Location):
			elsewhere = append(elsewhere, l.ExternalID)
		default:
			kept = append(kept, l)
		}
	}
	added, err := w.store.SaveListings(source, kept, unwanted, elsewhere)
	if err != nil {
		log.Printf("[jobs] %s: save: %v", source, err)
		_ = w.store.RecordJobFetch(source, false, "save failed", 0)
		return
	}
	need, err := w.store.ListingsNeedingDetail(source, refresh, maxDetails)
	if err != nil {
		log.Printf("[jobs] %s: detail list: %v", source, err)
	}
	read := 0
	for _, l := range need {
		d, err := detail(ctx, l)
		if err != nil {
			log.Printf("[jobs] %s %s: detail: %v — the rest wait for the next run", source, l.ExternalID, err)
			break
		}
		lo, hi := parseSalary(d.SalaryText)
		if err := w.store.SetJobDetail(source, l.ExternalID, d, lo, hi); err != nil {
			log.Printf("[jobs] %s %s: save detail: %v", source, l.ExternalID, err)
			continue
		}
		read++
	}
	if err := w.store.RecordJobFetch(source, true, "", len(kept)); err != nil {
		log.Printf("[jobs] %s: record fetch: %v", source, err)
	}
	log.Printf("[jobs] %s: %d roles (%d new), %d of %d pages read", source, len(kept), added, read, len(need))
}

// get fetches a URL politely; a 429 is retried after w.backoff (or the
// server's Retry-After); anything else but a 200 is an error.
func (w *JobWatcher) get(ctx context.Context, u string) ([]byte, error) {
	wait := w.pause
	if strings.HasPrefix(u, w.linkedInBase) && w.linkedInWait > wait {
		wait = w.linkedInWait
	}
	for attempt := 0; ; attempt++ {
		if err := sleepCtx(ctx, wait); err != nil {
			return nil, err
		}
		body, status, retryAfter, err := w.fetch(ctx, u)
		if err != nil {
			return nil, err
		}
		if status == http.StatusOK {
			return body, nil
		}
		if status != http.StatusTooManyRequests || attempt >= len(w.backoff) {
			return nil, fmt.Errorf("status %d from %s", status, strings.SplitN(u, "?", 2)[0])
		}
		wait = w.backoff[attempt]
		if retryAfter > wait {
			wait = retryAfter
		}
		log.Printf("[jobs] 429 from %s — retrying in %v", strings.SplitN(u, "?", 2)[0], wait)
	}
}

func (w *JobWatcher) fetch(ctx context.Context, u string) (body []byte, status int, retryAfter time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("User-Agent", jobsUserAgent)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 && s <= 300 {
		retryAfter = time.Duration(s) * time.Second
	}
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, retryAfter, nil
	}
	body, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return body, resp.StatusCode, 0, err
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func (w *JobWatcher) listNetflix(ctx context.Context) ([]Listing, error) {
	var out []Listing
	for _, q := range jobsQueries {
		for start, page := 0, 0; page < jobsNetflixPages; page++ {
			body, err := w.get(ctx, fmt.Sprintf("%s/api/apply/v2/jobs?domain=netflix.com&start=%d&num=10&query=%s&Work%%20Type=remote&sort_by=relevance",
				w.netflixBase, start, url.QueryEscape(strings.ToLower(q))))
			if err != nil {
				return nil, err
			}
			count, ls, err := parseNetflixList(body)
			if err != nil {
				return nil, err
			}
			out = append(out, ls...)
			start += len(ls)
			if len(ls) == 0 || start >= count {
				break
			}
		}
	}
	return out, nil
}

func (w *JobWatcher) netflixDetail(ctx context.Context, l Listing) (Detail, error) {
	body, err := w.get(ctx, fmt.Sprintf("%s/api/apply/v2/jobs/%s?domain=netflix.com", w.netflixBase, url.PathEscape(l.ExternalID)))
	if err != nil {
		return Detail{}, err
	}
	return parseNetflixDetail(body)
}

func (w *JobWatcher) listLinkedIn(ctx context.Context) ([]Listing, error) {
	var out []Listing
	for _, q := range jobsQueries {
		for page := 0; page < jobsLinkedInMax; page++ {
			body, err := w.get(ctx, fmt.Sprintf("%s/jobs-guest/jobs/api/seeMoreJobPostings/search?keywords=%s&f_C=%s&geoId=%s&start=%d",
				w.linkedInBase, url.QueryEscape(q), jobsLinkedInCo, jobsLinkedInGeo, page*jobsLinkedInPage))
			if err != nil {
				return nil, err
			}
			ls := parseLinkedInList(body, jobsCompany)
			out = append(out, ls...)
			if len(liCardRe.FindAllIndex(body, -1)) < jobsLinkedInPage {
				break
			}
		}
	}
	return out, nil
}

func (w *JobWatcher) linkedInDetail(ctx context.Context, l Listing) (Detail, error) {
	body, err := w.get(ctx, fmt.Sprintf("%s/jobs-guest/jobs/api/jobPosting/%s", w.linkedInBase, url.PathEscape(l.ExternalID)))
	if err != nil {
		return Detail{}, err
	}
	return parseLinkedInDetail(body), nil
}

// ---- storage -----------------------------------------------------------------

// SaveListings records a successful fetch: roles and postings upserted
// (a posting that was gone and is back is live again); postings of an
// unwanted role (drop) deleted — not gone: they're still up, just not
// wanted; postings listed this time in an unwanted place (elsewhere) left
// as they were but live — LinkedIn shows a multi-city posting under a
// different one of its cities from fetch to fetch, so one kept for New
// York must not vanish when it next reads Los Gatos (one never kept stays
// out); and the source's live postings it didn't find marked gone.
// Returns how many postings were new.
func (s *Store) SaveListings(source string, ls []Listing, drop, elsewhere []string) (int, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if len(drop) > 0 {
		if _, err := tx.Exec(ctx, `DELETE FROM sxiva_job_listing WHERE source = $1 AND external_id = ANY($2)`, source, drop); err != nil {
			return 0, err
		}
	}
	added := 0
	ids := make([]string, 0, len(ls))
	for _, l := range ls {
		role := RoleID(l.Title)
		if _, err := tx.Exec(ctx, `
			INSERT INTO sxiva_job_role (role_id, title) VALUES ($1::uuid, $2)
			ON CONFLICT (role_id) DO UPDATE SET title = EXCLUDED.title, updated_at = NOW()
			WHERE sxiva_job_role.title <> EXCLUDED.title`, role, l.Title); err != nil {
			return 0, err
		}
		var inserted bool
		if err := tx.QueryRow(ctx, `
			INSERT INTO sxiva_job_listing (source, external_id, role_id, title, location, url, posted_at)
			VALUES ($1, $2, $3::uuid, $4, $5, $6, $7)
			ON CONFLICT (source, external_id) DO UPDATE SET
				role_id = EXCLUDED.role_id, title = EXCLUDED.title, location = EXCLUDED.location,
				url = EXCLUDED.url, posted_at = EXCLUDED.posted_at, last_seen_at = NOW(), gone_at = NULL
			RETURNING (xmax = 0)`,
			source, l.ExternalID, role, l.Title, l.Location, l.URL, l.PostedAt).Scan(&inserted); err != nil {
			return 0, err
		}
		if inserted {
			added++
		}
		ids = append(ids, l.ExternalID)
	}
	if len(elsewhere) > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE sxiva_job_listing SET last_seen_at = NOW(), gone_at = NULL
			WHERE source = $1 AND external_id = ANY($2)`, source, elsewhere); err != nil {
			return 0, err
		}
		ids = append(ids, elsewhere...)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sxiva_job_listing SET gone_at = NOW()
		WHERE source = $1 AND gone_at IS NULL AND NOT (external_id = ANY($2))`, source, ids); err != nil {
		return 0, err
	}
	// A retitled posting moves to its new title's role; the old role (or a
	// role whose postings were all dropped), with nothing posting it and no
	// marks, goes.
	if _, err := tx.Exec(ctx, `
		DELETE FROM sxiva_job_role r WHERE NOT r.favorite AND NOT r.hidden
		  AND NOT EXISTS (SELECT 1 FROM sxiva_job_listing l WHERE l.role_id = r.role_id)`); err != nil {
		return 0, err
	}
	return added, tx.Commit(ctx)
}

// ListingsNeedingDetail: live postings whose page hasn't been read — or,
// with refresh, every live posting — newest first, at most limit (0 = all).
func (s *Store) ListingsNeedingDetail(source string, refresh bool, limit int) ([]Listing, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT external_id, title FROM sxiva_job_listing
		WHERE source = $1 AND gone_at IS NULL AND ($2 OR detail_at IS NULL)
		ORDER BY first_seen_at DESC, external_id
		LIMIT NULLIF($3, 0)`, source, refresh, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Listing
	for rows.Next() {
		l := Listing{Source: source}
		if err := rows.Scan(&l.ExternalID, &l.Title); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetJobDetail stores what a posting's page says: description, details,
// salary range (nil = none stated) and the displayed posting date.
func (s *Store) SetJobDetail(source, externalID string, d Detail, lo, hi *int) error {
	ctx, cancel := withCtx()
	defer cancel()
	meta := d.Meta
	if meta == nil {
		meta = []MetaField{}
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `
		UPDATE sxiva_job_listing SET description = $3, meta = $4::jsonb, detail_posted_at = $5,
			salary_min = $6, salary_max = $7, detail_at = NOW()
		WHERE source = $1 AND external_id = $2`, source, externalID, d.Description, string(metaJSON), d.PostedAt, lo, hi)
	return err
}

// RecordJobFetch stamps a source's last check.
func (s *Store) RecordJobFetch(source string, ok bool, errMsg string, found int) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO sxiva_job_fetch (source, checked_at, ok, error, found) VALUES ($1, NOW(), $2, $3, $4)
		ON CONFLICT (source) DO UPDATE SET checked_at = NOW(), ok = EXCLUDED.ok, error = EXCLUDED.error,
			found = CASE WHEN EXCLUDED.ok THEN EXCLUDED.found ELSE sxiva_job_fetch.found END`,
		source, ok, errMsg, found)
	return err
}

// JobPostingView is one source's posting of a role. Description and Meta
// come only with a single role (JobRole).
type JobPostingView struct {
	Source      string      `json:"source"`
	Title       string      `json:"title"`
	URL         string      `json:"url"`
	Location    string      `json:"location"`
	PostedAt    *time.Time  `json:"posted_at"` // the date the source shows
	GoneAt      *time.Time  `json:"gone_at"`
	SalaryMin   *int        `json:"salary_min"`
	SalaryMax   *int        `json:"salary_max"`
	Description string      `json:"description,omitempty"`
	Meta        []MetaField `json:"meta,omitempty"`
}

// JobRoleView is a role as the page shows it: one card, whatever sources
// post it.
type JobRoleView struct {
	RoleID    string           `json:"role_id"`
	Title     string           `json:"title"`
	Favorite  bool             `json:"favorite"`
	Hidden    bool             `json:"hidden"`
	SeenAt    *time.Time       `json:"seen_at"`   // Andrew last starred, hid or opened it
	PostedAt  *time.Time       `json:"posted_at"` // latest posting still up (or ever, once gone)
	GoneAt    *time.Time       `json:"gone_at"`   // set once every source dropped it: the last to go
	SalaryMin *int             `json:"salary_min"`
	SalaryMax *int             `json:"salary_max"`
	Locations []string         `json:"locations"`
	Postings  []JobPostingView `json:"postings"`
}

// JobCheckView is a source's last check.
type JobCheckView struct {
	Source    string     `json:"source"`
	CheckedAt *time.Time `json:"checked_at"`
	OK        bool       `json:"ok"`
	Error     string     `json:"error"`
}

// jobSources in display order: Netflix's own posting first.
var jobSources = []string{SourceNetflix, SourceLinkedIn}

// JobsView: every role ever seen (live, gone, hidden — the page decides
// what to show), newest posting first, and each source's last check.
func (s *Store) JobsView() ([]JobRoleView, []JobCheckView, error) {
	ctx, cancel := withCtx()
	defer cancel()
	checks := []JobCheckView{}
	for _, source := range jobSources {
		c := JobCheckView{Source: source}
		err := s.pool.QueryRow(ctx, `SELECT checked_at, ok, error FROM sxiva_job_fetch WHERE source = $1`, source).
			Scan(&c.CheckedAt, &c.OK, &c.Error)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		checks = append(checks, c)
	}
	roles, err := s.jobRoles(ctx, "", false)
	if err != nil {
		return nil, nil, err
	}
	sort.SliceStable(roles, func(i, j int) bool {
		a, b := roles[i].PostedAt, roles[j].PostedAt
		if a == nil || b == nil {
			return a != nil
		}
		if !a.Equal(*b) {
			return a.After(*b)
		}
		return roles[i].Title < roles[j].Title
	})
	return roles, checks, nil
}

// JobRole is one role with each posting's description and details; nil
// when there's no such role.
func (s *Store) JobRole(roleID string) (*JobRoleView, error) {
	ctx, cancel := withCtx()
	defer cancel()
	roles, err := s.jobRoles(ctx, roleID, true)
	if err != nil || len(roles) == 0 {
		return nil, err
	}
	return &roles[0], nil
}

// jobRoles reads roles with their postings (all roles when roleID is "";
// descriptions and details only when full).
func (s *Store) jobRoles(ctx context.Context, roleID string, full bool) ([]JobRoleView, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT r.role_id::text, r.title, r.favorite, r.hidden, r.seen_at,
		       l.source, l.title, l.url, l.location, COALESCE(l.detail_posted_at, l.posted_at), l.gone_at,
		       l.salary_min, l.salary_max,
		       CASE WHEN $2 THEN l.description ELSE '' END, CASE WHEN $2 THEN l.meta ELSE '[]'::jsonb END
		FROM sxiva_job_role r JOIN sxiva_job_listing l ON l.role_id = r.role_id
		WHERE $1 = '' OR r.role_id::text = $1
		ORDER BY r.role_id, array_position($3::text[], l.source::text), l.external_id`, roleID, full, jobSources)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byRole := map[string]*JobRoleView{}
	var order []string
	for rows.Next() {
		var (
			v    JobRoleView
			p    JobPostingView
			meta []byte
		)
		if err := rows.Scan(&v.RoleID, &v.Title, &v.Favorite, &v.Hidden, &v.SeenAt,
			&p.Source, &p.Title, &p.URL, &p.Location, &p.PostedAt, &p.GoneAt, &p.SalaryMin, &p.SalaryMax,
			&p.Description, &meta); err != nil {
			return nil, err
		}
		if full {
			if err := json.Unmarshal(meta, &p.Meta); err != nil {
				return nil, err
			}
		}
		p.PostedAt, p.GoneAt = utc(p.PostedAt), utc(p.GoneAt) // dates mean UTC days
		role := byRole[v.RoleID]
		if role == nil {
			v.Locations, v.Postings = []string{}, []JobPostingView{}
			role = &v
			byRole[v.RoleID] = role
			order = append(order, v.RoleID)
		}
		role.Postings = append(role.Postings, p)
		// Netflix sorts first: its stated range wins
		if role.SalaryMin == nil && p.SalaryMin != nil {
			role.SalaryMin, role.SalaryMax = p.SalaryMin, p.SalaryMax
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	roles := make([]JobRoleView, 0, len(order))
	for _, id := range order {
		roles = append(roles, summarizeRole(*byRole[id]))
	}
	return roles, nil
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// summarizeRole fills a role's posted/gone dates and locations from its
// postings: live postings count while there are any, and the role is as
// new as its latest posting (a repost is news).
func summarizeRole(v JobRoleView) JobRoleView {
	var live []JobPostingView
	for _, p := range v.Postings {
		if p.GoneAt == nil {
			live = append(live, p)
		}
	}
	use := live
	if len(live) == 0 {
		use = v.Postings
		for _, p := range v.Postings {
			if p.GoneAt != nil && (v.GoneAt == nil || p.GoneAt.After(*v.GoneAt)) {
				v.GoneAt = p.GoneAt
			}
		}
	}
	seen := map[string]bool{}
	for _, p := range use {
		if p.PostedAt != nil && (v.PostedAt == nil || p.PostedAt.After(*v.PostedAt)) {
			v.PostedAt = p.PostedAt
		}
		if p.Location != "" && !seen[p.Location] {
			seen[p.Location] = true
			v.Locations = append(v.Locations, p.Location)
		}
	}
	return v
}

// UpdateJobRole sets a role's marks (nil = unchanged; seen true stamps
// seen_at now, false clears it); false when there's no such role.
func (s *Store) UpdateJobRole(roleID string, favorite, hidden, seen *bool) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
		UPDATE sxiva_job_role SET favorite = COALESCE($2, favorite), hidden = COALESCE($3, hidden),
			seen_at = CASE WHEN $4::boolean IS NULL THEN seen_at WHEN $4 THEN NOW() END, updated_at = NOW()
		WHERE role_id = $1::uuid`, roleID, favorite, hidden, seen)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
