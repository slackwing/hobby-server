package sxiva

// A real run against the live sources, into the SXIVA_TEST_DB_URL
// database — for checking the parsers when a site changes. Opt-in:
//
//	SXIVA_JOBS_LIVE=1 SXIVA_TEST_DB_URL=postgres://… go test -run JobsLive -v ./internal/sxiva/
//
// SXIVA_JOBS_DUMP=<dir> also writes what the API would answer — jobs.json
// and roles/<role_id>.json — for working on the page offline.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestJobsLive(t *testing.T) {
	if os.Getenv("SXIVA_JOBS_LIVE") == "" {
		t.Skip("set SXIVA_JOBS_LIVE=1 to fetch from the real sites")
	}
	store := jobsTestStore(t)
	NewJobWatcher(store).RunOnce(context.Background())
	roles, checks, err := store.JobsView()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range checks {
		t.Logf("check %s ok=%v error=%q", c.Source, c.OK, c.Error)
		if !c.OK {
			t.Errorf("%s fetch failed", c.Source)
		}
	}
	salaries := 0
	for _, r := range roles {
		sources := ""
		for _, p := range r.Postings {
			sources += p.Source + " "
		}
		if r.SalaryMin != nil {
			salaries++
		}
		t.Logf("%s  %-80s  %v-%v  %v  [%s]", r.PostedAt.Format("2006-01-02"), r.Title, deref(r.SalaryMin), deref(r.SalaryMax), r.Locations, sources)
		if full, err := store.JobRole(r.RoleID); err == nil && full != nil {
			for _, p := range full.Postings {
				t.Logf("    %-8s %s  %d chars  %+v", p.Source, p.PostedAt.Format("2006-01-02"), len(p.Description), p.Meta)
			}
		}
	}
	t.Logf("%d roles, %d with a salary range", len(roles), salaries)
	if len(roles) == 0 {
		t.Error("no roles found")
	}
	if dir := os.Getenv("SXIVA_JOBS_DUMP"); dir != "" {
		dump(t, filepath.Join(dir, "jobs.json"), map[string]any{"roles": roles, "checks": checks})
		for _, r := range roles {
			full, err := store.JobRole(r.RoleID)
			if err != nil {
				t.Fatal(err)
			}
			dump(t, filepath.Join(dir, "roles", r.RoleID+".json"), full)
		}
	}
}

func dump(t *testing.T, path string, v any) {
	t.Helper()
	b, err := json.MarshalIndent(v, "", " ")
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err == nil {
		err = os.WriteFile(path, b, 0o644)
	}
	if err != nil {
		t.Fatal(err)
	}
}

func deref(p *int) any {
	if p == nil {
		return "-"
	}
	return *p
}
