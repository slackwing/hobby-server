package sxiva

// The watcher end to end against fake sources serving the captured
// fixtures, on a real Postgres migrated with liquibase/sxiva. Skips unless
// SXIVA_TEST_DB_URL points at one (a throwaway — the test truncates the
// job tables).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeSources struct {
	netflixDown    atomic.Bool
	dropNetflix    atomic.Value // external id to leave out of the Netflix list
	linkedInPaused atomic.Bool  // LinkedIn's posting pages answer 429
	netflixList    []byte
	netflixEmpty   []byte
	postingDates   map[string]string // Netflix id → the page's Job Posting Date (else none)
	linkedInPages  atomic.Int32
}

func (f *fakeSources) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/apply/v2/jobs", func(w http.ResponseWriter, r *http.Request) {
		if f.netflixDown.Load() {
			http.Error(w, "down", http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Get("start") != "0" {
			_, _ = w.Write(f.netflixEmpty)
			return
		}
		var list struct {
			Count     int              `json:"count"`
			Positions []map[string]any `json:"positions"`
		}
		dec := json.NewDecoder(bytes.NewReader(f.netflixList))
		dec.UseNumber() // ids are bigger than a float prints exactly
		if err := dec.Decode(&list); err != nil {
			t.Fatal(err)
		}
		drop, _ := f.dropNetflix.Load().(string)
		kept := list.Positions[:0]
		for _, p := range list.Positions {
			if fmt.Sprint(p["id"]) != drop {
				kept = append(kept, p)
			}
		}
		list.Positions = kept
		_ = json.NewEncoder(w).Encode(list)
	})
	mux.HandleFunc("/api/apply/v2/jobs/", func(w http.ResponseWriter, r *http.Request) {
		var detail map[string]any
		if err := json.Unmarshal(fixture(t, "netflix-detail.json"), &detail); err != nil {
			t.Fatal(err)
		}
		dates := []string{}
		if d, ok := f.postingDates[strings.TrimPrefix(r.URL.Path, "/api/apply/v2/jobs/")]; ok {
			dates = append(dates, d)
		}
		detail["custom_JD"].(map[string]any)["data_fields"].(map[string]any)["posting_date"] = dates
		_ = json.NewEncoder(w).Encode(detail)
	})
	mux.HandleFunc("/jobs-guest/jobs/api/seeMoreJobPostings/search", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("start") == "0" {
			_, _ = w.Write(fixture(t, "linkedin-list.html"))
		}
	})
	mux.HandleFunc("/jobs-guest/jobs/api/jobPosting/", func(w http.ResponseWriter, _ *http.Request) {
		f.linkedInPages.Add(1)
		if f.linkedInPaused.Load() {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write(fixture(t, "linkedin-detail.html"))
	})
	return mux
}

func jobsTestStore(t *testing.T) *Store {
	t.Helper()
	url := os.Getenv("SXIVA_TEST_DB_URL")
	if url == "" {
		t.Skip("SXIVA_TEST_DB_URL not set (a throwaway Postgres migrated with liquibase/sxiva)")
	}
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE sxiva_job_listing, sxiva_job_role, sxiva_job_fetch`); err != nil {
		t.Fatal(err)
	}
	return NewStore(pool)
}

func findRole(roles []JobRoleView, title string) *JobRoleView {
	for i := range roles {
		if roles[i].Title == title {
			return &roles[i]
		}
	}
	return nil
}

func TestJobWatcherEndToEnd(t *testing.T) {
	store := jobsTestStore(t)
	fake := &fakeSources{
		netflixList:  fixture(t, "netflix-list.json"),
		netflixEmpty: []byte(`{"count": 65, "positions": []}`),
		postingDates: map[string]string{"790318043838": "10-23-2026"}, // TV Product UI, reposted
	}
	fake.dropNetflix.Store("")
	fake.linkedInPaused.Store(true)
	srv := httptest.NewServer(fake.handler(t))
	defer srv.Close()
	w := NewJobWatcher(store)
	w.netflixBase, w.linkedInBase, w.pause, w.linkedInWait, w.backoff = srv.URL, srv.URL, 0, 0, nil
	ctx := context.Background()

	// LinkedIn's posting pages are rate-limited on the first run: the list
	// still counts, and the first 429 stops the page reads until next run.
	w.RunOnce(ctx)
	if n := fake.linkedInPages.Load(); n != 1 {
		t.Errorf("%d LinkedIn page requests after a 429, want 1", n)
	}
	fake.linkedInPaused.Store(false)
	w.RunOnce(ctx)
	if n := fake.linkedInPages.Load(); n != 5 {
		t.Errorf("%d LinkedIn page requests, want 1 + 4 (each posting's page once)", n)
	}
	w.RunOnce(ctx)
	if n := fake.linkedInPages.Load(); n != 5 {
		t.Errorf("LinkedIn pages already read were fetched again (%d requests)", n)
	}

	roles, checks, err := store.JobsView()
	if err != nil {
		t.Fatal(err)
	}
	if len(checks) != 2 || checks[0].Source != SourceNetflix || !checks[0].OK || !checks[1].OK {
		t.Fatalf("checks %+v", checks)
	}
	// 6 Netflix positions + 4 LinkedIn cards, "Software Engineer 5 - iOS" on both.
	if len(roles) != 9 {
		t.Fatalf("%d roles, want 9 (sources combined)", len(roles))
	}
	ios := findRole(roles, "Software Engineer 5 - iOS")
	if ios == nil || len(ios.Postings) != 2 || ios.Postings[0].Source != SourceNetflix || ios.Postings[1].Source != SourceLinkedIn {
		t.Fatalf("iOS role should combine both sources: %+v", ios)
	}
	if ios.SalaryMin == nil || *ios.SalaryMin != 320000 || *ios.SalaryMax != 500000 {
		t.Errorf("iOS salary from Netflix's page: %v-%v", ios.SalaryMin, ios.SalaryMax)
	}
	if ios.PostedAt.Format("2006-01-02") != "2026-10-26" || ios.PostedAt.Location() != time.UTC ||
		ios.Postings[0].PostedAt.Format("2006-01-02") != "2026-08-21" || ios.Postings[1].PostedAt.Format("2006-01-02") != "2026-10-26" {
		t.Errorf("iOS posted %v (postings %v, %v): want the latest posting, LinkedIn's 2026-10-26", ios.PostedAt, ios.Postings[0].PostedAt, ios.Postings[1].PostedAt)
	}
	if ios.Postings[0].Description != "" || ios.Postings[0].Meta != nil {
		t.Error("the roles list leaves descriptions to the single-role view")
	}
	tv := findRole(roles, "Software Engineer 5 - TV Product UI")
	if tv == nil || tv.PostedAt.Format("2006-01-02") != "2026-10-23" {
		t.Errorf("TV Product UI posted %v: want its page's Job Posting Date 2026-10-23, not t_create", tv.PostedAt)
	}
	if roles[0].Title != ios.Title || roles[1].Title != tv.Title {
		t.Errorf("newest first: %q, %q", roles[0].Title, roles[1].Title)
	}
	li := findRole(roles, "Software Engineering 5 - Ads Agent (Agentic Platform for Enterprise)")
	if li == nil || len(li.Postings) != 1 || li.SalaryMin == nil || len(li.Locations) != 1 || li.Locations[0] != "New York, NY" {
		t.Fatalf("LinkedIn-only role: %+v", li)
	}
	for i := 1; i < len(roles); i++ {
		if roles[i].PostedAt.After(*roles[i-1].PostedAt) {
			t.Fatalf("roles not newest first at %d", i)
		}
	}

	// One role in full: each source's description and details.
	full, err := store.JobRole(ios.RoleID)
	if err != nil || full == nil || len(full.Postings) != 2 {
		t.Fatalf("JobRole: %+v %v", full, err)
	}
	nf, ln := full.Postings[0], full.Postings[1]
	if !strings.HasPrefix(nf.Description, "<p>At Netflix") || !strings.HasPrefix(ln.Description, "At Netflix") {
		t.Errorf("descriptions: %.40q / %.40q", nf.Description, ln.Description)
	}
	if v, _ := metaValue(nf.Meta, "Job Requisition ID"); v != "JR42274" {
		t.Errorf("netflix meta %+v", nf.Meta)
	}
	if v, _ := metaValue(ln.Meta, "Employment type"); v != "Full-time" {
		t.Errorf("linkedin meta %+v", ln.Meta)
	}
	if missing, err := store.JobRole("00000000-0000-5000-8000-000000000000"); err != nil || missing != nil {
		t.Errorf("unknown role: %+v %v", missing, err)
	}
	if few, err := store.ListingsNeedingDetail(SourceNetflix, true, 2); err != nil || len(few) != 2 {
		t.Errorf("limit: %d %v", len(few), err)
	}

	// A Netflix posting disappears: its posting is gone; a Netflix-only
	// role is gone, the iOS role stays live through LinkedIn.
	fake.dropNetflix.Store("790318041034") // iOS on Netflix's site
	w.RunOnce(ctx)
	roles, _, _ = store.JobsView()
	ios = findRole(roles, "Software Engineer 5 - iOS")
	if ios.GoneAt != nil || ios.Postings[0].GoneAt == nil || ios.Postings[1].GoneAt != nil {
		t.Errorf("iOS after Netflix drops it: role gone %v, postings %+v", ios.GoneAt, ios.Postings)
	}
	fake.dropNetflix.Store("790318081427") // Netflix-only: Model Development…
	w.RunOnce(ctx)
	roles, _, _ = store.JobsView()
	md := findRole(roles, "Software Engineer 5 – Model Development and Management, AI Platform")
	if md == nil || md.GoneAt == nil {
		t.Fatalf("a role no source lists any more is gone: %+v", md)
	}
	if ios = findRole(roles, "Software Engineer 5 - iOS"); ios.Postings[0].GoneAt != nil {
		t.Error("the Netflix iOS posting came back — it should be live again")
	}

	// A failed fetch changes nothing.
	before, _, _ := store.JobsView()
	fake.netflixDown.Store(true)
	fake.dropNetflix.Store("")
	w.RunOnce(ctx)
	after, checks, _ := store.JobsView()
	if checks[0].OK || !strings.Contains(checks[0].Error, "status 503") || len(after) != len(before) {
		t.Fatalf("failed fetch: check %+v, %d → %d roles", checks[0], len(before), len(after))
	}
	if md = findRole(after, "Software Engineer 5 – Model Development and Management, AI Platform"); md.GoneAt == nil {
		t.Error("a failed fetch must not revive or drop anything")
	}

	// Marks.
	yes := true
	if ok, err := store.UpdateJobRole(ios.RoleID, &yes, nil); err != nil || !ok {
		t.Fatalf("favorite: %v %v", ok, err)
	}
	if ok, err := store.UpdateJobRole(ios.RoleID, nil, &yes); err != nil || !ok {
		t.Fatalf("hide: %v %v", ok, err)
	}
	roles, _, _ = store.JobsView()
	if ios = findRole(roles, "Software Engineer 5 - iOS"); !ios.Favorite || !ios.Hidden {
		t.Errorf("marks: %+v", ios)
	}
	if ok, _ := store.UpdateJobRole("00000000-0000-5000-8000-000000000000", &yes, nil); ok {
		t.Error("unknown role must report false")
	}
}

// A 429 is retried after the backoff; past the retries it's an error.
func TestJobWatcherRetries429(t *testing.T) {
	var calls atomic.Int32
	limitFor := int32(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if calls.Add(1) <= limitFor {
			http.Error(w, "slow down", http.StatusTooManyRequests)
			return
		}
		_, _ = w.Write([]byte("ok"))
	}))
	defer srv.Close()
	w := NewJobWatcher(nil)
	w.pause, w.linkedInWait, w.backoff = 0, 0, []time.Duration{time.Millisecond, time.Millisecond}
	body, err := w.get(context.Background(), srv.URL+"/x")
	if err != nil || string(body) != "ok" || calls.Load() != 3 {
		t.Fatalf("two 429s then ok: body %q err %v calls %d", body, err, calls.Load())
	}
	calls.Store(0)
	limitFor = 5
	if _, err := w.get(context.Background(), srv.URL+"/x"); err == nil || !strings.Contains(err.Error(), "status 429") || calls.Load() != 3 {
		t.Fatalf("past the retries: err %v calls %d", err, calls.Load())
	}
}
