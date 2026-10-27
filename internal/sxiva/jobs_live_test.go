package sxiva

// A real run against the live sources, into the SXIVA_TEST_DB_URL
// database — for checking the parsers when a site changes. Opt-in:
//
//	SXIVA_JOBS_LIVE=1 SXIVA_TEST_DB_URL=postgres://… go test -run JobsLive -v ./internal/sxiva/

import (
	"context"
	"os"
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
	}
	t.Logf("%d roles, %d with a salary range", len(roles), salaries)
	if len(roles) == 0 {
		t.Error("no roles found")
	}
}

func deref(p *int) any {
	if p == nil {
		return "-"
	}
	return *p
}
