package sxiva

// The Netflix roles watcher's fetching and storage (pure parts: jobs.go).
// Hourly, per source: list SE5 + SE6 postings, keep the wanted levels,
// save them (new roles, postings that came back, postings that are gone),
// then read each new posting's detail page once for its salary range. A
// failed list fetch changes nothing but the source's last-check row.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sort"
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
	jobsCareersPages = 15
	jobsLinkedInPage = 10 // cards per page
	jobsLinkedInMax  = 10 // pages
	jobsStartDelay   = 30 * time.Second
)

// jobsQueries: one search per wanted level on each source.
var jobsQueries = []string{"Software Engineer 5", "Software Engineer 6"}

// JobWatcher fetches the sources; one run at a time.
type JobWatcher struct {
	store        *Store
	client       *http.Client
	careersBase  string
	linkedInBase string
	pause        time.Duration // between requests, to be polite
	linkedInWait time.Duration // LinkedIn rate-limits quickly: slower still
	running      atomic.Bool
}

func NewJobWatcher(store *Store) *JobWatcher {
	return &JobWatcher{
		store:        store,
		client:       &http.Client{Timeout: 20 * time.Second},
		careersBase:  "https://explore.jobs.netflix.net",
		linkedInBase: "https://www.linkedin.com",
		pause:        time.Second,
		linkedInWait: 3 * time.Second,
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
	w.runSource(ctx, SourceCareers, w.listCareers, w.careersDetail)
	w.runSource(ctx, SourceLinkedIn, w.listLinkedIn, w.linkedInDetail)
}

func (w *JobWatcher) runSource(ctx context.Context, source string,
	list func(context.Context) ([]Listing, error), detail func(context.Context, Listing) (string, error)) {
	all, err := list(ctx)
	if err != nil {
		log.Printf("[jobs] %s: %v", source, err)
		if rerr := w.store.RecordJobFetch(source, false, err.Error(), 0); rerr != nil {
			log.Printf("[jobs] %s: record fetch: %v", source, rerr)
		}
		return
	}
	seen := map[string]bool{}
	var kept []Listing
	for _, l := range all {
		if wantedLevel(l.Title) && !seen[l.ExternalID] {
			seen[l.ExternalID] = true
			kept = append(kept, l)
		}
	}
	added, err := w.store.SaveListings(source, kept)
	if err != nil {
		log.Printf("[jobs] %s: save: %v", source, err)
		_ = w.store.RecordJobFetch(source, false, "save failed", 0)
		return
	}
	need, err := w.store.ListingsNeedingSalary(source)
	if err != nil {
		log.Printf("[jobs] %s: salary list: %v", source, err)
	}
	for _, l := range need {
		text, err := detail(ctx, l)
		if err != nil {
			log.Printf("[jobs] %s %s: detail: %v", source, l.ExternalID, err)
			continue
		}
		lo, hi := parseSalary(text)
		if err := w.store.SetJobSalary(source, l.ExternalID, lo, hi); err != nil {
			log.Printf("[jobs] %s %s: salary: %v", source, l.ExternalID, err)
		}
	}
	if err := w.store.RecordJobFetch(source, true, "", len(kept)); err != nil {
		log.Printf("[jobs] %s: record fetch: %v", source, err)
	}
	log.Printf("[jobs] %s: %d roles (%d new), %d salaries read", source, len(kept), added, len(need))
}

// get fetches a URL politely; anything but a 200 is an error.
func (w *JobWatcher) get(ctx context.Context, u string) ([]byte, error) {
	wait := w.pause
	if strings.HasPrefix(u, w.linkedInBase) && w.linkedInWait > wait {
		wait = w.linkedInWait
	}
	if wait > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", jobsUserAgent)
	resp, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d from %s", resp.StatusCode, strings.SplitN(u, "?", 2)[0])
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}

func (w *JobWatcher) listCareers(ctx context.Context) ([]Listing, error) {
	var out []Listing
	for _, q := range jobsQueries {
		for start, page := 0, 0; page < jobsCareersPages; page++ {
			body, err := w.get(ctx, fmt.Sprintf("%s/api/apply/v2/jobs?domain=netflix.com&start=%d&num=10&query=%s&Work%%20Type=remote&sort_by=relevance",
				w.careersBase, start, url.QueryEscape(strings.ToLower(q))))
			if err != nil {
				return nil, err
			}
			count, ls, err := parseCareersList(body)
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

func (w *JobWatcher) careersDetail(ctx context.Context, l Listing) (string, error) {
	body, err := w.get(ctx, fmt.Sprintf("%s/api/apply/v2/jobs/%s?domain=netflix.com", w.careersBase, url.PathEscape(l.ExternalID)))
	if err != nil {
		return "", err
	}
	return parseCareersDetail(body)
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

func (w *JobWatcher) linkedInDetail(ctx context.Context, l Listing) (string, error) {
	body, err := w.get(ctx, fmt.Sprintf("%s/jobs-guest/jobs/api/jobPosting/%s", w.linkedInBase, url.PathEscape(l.ExternalID)))
	if err != nil {
		return "", err
	}
	return htmlText(string(body)), nil
}

// ---- storage -----------------------------------------------------------------

// SaveListings records a successful fetch: roles and postings upserted
// (a posting that was gone and is back is live again), and the source's
// live postings it didn't find marked gone. Returns how many postings were
// new.
func (s *Store) SaveListings(source string, ls []Listing) (int, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
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
	if _, err := tx.Exec(ctx, `
		UPDATE sxiva_job_listing SET gone_at = NOW()
		WHERE source = $1 AND gone_at IS NULL AND NOT (external_id = ANY($2))`, source, ids); err != nil {
		return 0, err
	}
	return added, tx.Commit(ctx)
}

// ListingsNeedingSalary: live postings whose detail page hasn't been read,
// skipping roles another source already gave a range for (the careers site
// runs first, so most LinkedIn postings need no extra request).
func (s *Store) ListingsNeedingSalary(source string) ([]Listing, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT l.external_id, l.title FROM sxiva_job_listing l
		WHERE l.source = $1 AND l.gone_at IS NULL AND l.salary_checked_at IS NULL
		  AND NOT EXISTS (SELECT 1 FROM sxiva_job_listing o
		                  WHERE o.role_id = l.role_id AND o.salary_min IS NOT NULL)
		ORDER BY l.first_seen_at`, source)
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

// SetJobSalary stores a posting's range (nil = none stated) and marks its
// detail page as read.
func (s *Store) SetJobSalary(source, externalID string, lo, hi *int) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		UPDATE sxiva_job_listing SET salary_min = $3, salary_max = $4, salary_checked_at = NOW()
		WHERE source = $1 AND external_id = $2`, source, externalID, lo, hi)
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

// JobPostingView is one source's posting of a role.
type JobPostingView struct {
	Source   string     `json:"source"`
	URL      string     `json:"url"`
	Location string     `json:"location"`
	PostedAt *time.Time `json:"posted_at"`
	GoneAt   *time.Time `json:"gone_at"`
}

// JobRoleView is a role as the page shows it: one card, whatever sources
// post it.
type JobRoleView struct {
	RoleID    string           `json:"role_id"`
	Title     string           `json:"title"`
	Favorite  bool             `json:"favorite"`
	Hidden    bool             `json:"hidden"`
	PostedAt  *time.Time       `json:"posted_at"` // earliest posting still up (or ever, once gone)
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

// JobsView: every role ever seen (live, gone, hidden — the page decides
// what to show), newest posting first, and each source's last check.
func (s *Store) JobsView() ([]JobRoleView, []JobCheckView, error) {
	ctx, cancel := withCtx()
	defer cancel()
	checks := []JobCheckView{}
	for _, source := range []string{SourceCareers, SourceLinkedIn} {
		c := JobCheckView{Source: source}
		err := s.pool.QueryRow(ctx, `SELECT checked_at, ok, error FROM sxiva_job_fetch WHERE source = $1`, source).
			Scan(&c.CheckedAt, &c.OK, &c.Error)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return nil, nil, err
		}
		checks = append(checks, c)
	}

	rows, err := s.pool.Query(ctx, `
		SELECT r.role_id::text, r.title, r.favorite, r.hidden,
		       l.source, l.url, l.location, l.posted_at, l.gone_at, l.salary_min, l.salary_max
		FROM sxiva_job_role r JOIN sxiva_job_listing l ON l.role_id = r.role_id
		ORDER BY r.role_id, l.source`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byRole := map[string]*JobRoleView{}
	var order []string
	for rows.Next() {
		var (
			v          JobRoleView
			p          JobPostingView
			smin, smax *int
		)
		if err := rows.Scan(&v.RoleID, &v.Title, &v.Favorite, &v.Hidden,
			&p.Source, &p.URL, &p.Location, &p.PostedAt, &p.GoneAt, &smin, &smax); err != nil {
			return nil, nil, err
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
		// careers sorts first: its stated range wins
		if role.SalaryMin == nil && smin != nil {
			role.SalaryMin, role.SalaryMax = smin, smax
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	roles := make([]JobRoleView, 0, len(order))
	for _, id := range order {
		roles = append(roles, summarizeRole(*byRole[id]))
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

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// summarizeRole fills a role's posted/gone dates and locations from its
// postings: live postings count while there are any.
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
		if p.PostedAt != nil && (v.PostedAt == nil || p.PostedAt.Before(*v.PostedAt)) {
			v.PostedAt = p.PostedAt
		}
		if p.Location != "" && !seen[p.Location] {
			seen[p.Location] = true
			v.Locations = append(v.Locations, p.Location)
		}
	}
	return v
}

// UpdateJobRole sets a role's marks; false when there's no such role.
func (s *Store) UpdateJobRole(roleID string, favorite, hidden *bool) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
		UPDATE sxiva_job_role SET favorite = COALESCE($2, favorite), hidden = COALESCE($3, hidden), updated_at = NOW()
		WHERE role_id = $1::uuid`, roleID, favorite, hidden)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
