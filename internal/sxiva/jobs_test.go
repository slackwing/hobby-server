package sxiva

import (
	"os"
	"testing"
)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/jobs/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestRoleIDIsUUIDv5OfTheNormalizedTitle(t *testing.T) {
	// Expected values from Python: uuid.uuid5(ns, normalized title), ns =
	// uuid5(NAMESPACE_URL, "https://andrewcheong.com/status/jobs").
	cases := map[string]string{
		"Software Engineer 5 – Model Development and Management, AI Platform": "1edfeec0-d725-5097-9551-9220b85dd20f",
		"Software Engineer 5 - iOS":       "860790af-392a-56e8-a1f8-504a5055b969",
		"  SOFTWARE   Engineer 5 -  iOS ": "860790af-392a-56e8-a1f8-504a5055b969", // same role
	}
	for title, want := range cases {
		if got := RoleID(title); got != want {
			t.Errorf("RoleID(%q) = %s, want %s", title, got, want)
		}
	}
	if RoleID("Software Engineer 5 – iOS") != RoleID("Software Engineer 5 - iOS") {
		t.Error("an en dash and a hyphen must give the same role")
	}
}

func TestWantedLevel(t *testing.T) {
	cases := map[string]bool{
		"Software Engineer 5 - iOS":                        true,
		"Software Engineer 6, Data Platform":               true,
		"Security Software Engineer 5":                     true,
		"Software Engineering 5 - Ads Agent":               true,
		"Software Engineer 4/5 – Model Serving Systems":    true,
		"Software Engineer 5/6 - Streaming":                true,
		"Senior Software Engineer (L5) - Personalization":  true,
		"Software Engineer 4 - Android":                    false,
		"Software Engineer 3":                              false,
		"Engineering Manager 5":                            false,
		"Senior Software Engineer (L4)":                    false,
		"Software Engineer 4 - Revenue Infrastructure (5)": false,
		"Software Engineer (L6) - Compute Runtime":         true,
		"Staff ML Software Engineer (L6) — Platform":       true,
		"Business Security Partner (L5)":                   false, // the fuzzy search's other jobs
		"Data Engineer (L5) - Ads":                         false,
		"Distributed Systems Engineer (L5) - Compute":      false,
		"Product Manager - Games (L5)":                     false,
	}
	for title, want := range cases {
		if got := wantedLevel(title); got != want {
			t.Errorf("wantedLevel(%q) = %v, want %v", title, got, want)
		}
	}
}

func TestParseSalary(t *testing.T) {
	cases := []struct {
		text     string
		min, max int
	}{
		{"The range for this role is $320,000.00 - $500,000.00. This compensation…", 320000, 500000},
		{"pays $388,000 – $720,000 depending", 388000, 720000},
		{"$150,000 to $210,000", 150000, 210000},
	}
	for _, c := range cases {
		lo, hi := parseSalary(c.text)
		if lo == nil || hi == nil || *lo != c.min || *hi != c.max {
			t.Errorf("parseSalary(%q) = %v %v, want %d %d", c.text, lo, hi, c.min, c.max)
		}
	}
	for _, text := range []string{"no range here", "$5 - $9 gift card", "$500,000 - $300,000"} {
		if lo, hi := parseSalary(text); lo != nil || hi != nil {
			t.Errorf("parseSalary(%q) should find nothing", text)
		}
	}
}

func TestParseCareers(t *testing.T) {
	count, list, err := parseCareersList(fixture(t, "careers-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if count != 65 || len(list) != 6 {
		t.Fatalf("count %d, %d positions", count, len(list))
	}
	first := list[0]
	if first.ExternalID != "790318081427" || first.Title != "Software Engineer 5 – Model Development and Management, AI Platform" ||
		first.Location != "Remote, United States" || first.URL != "https://explore.jobs.netflix.net/careers/job/790318081427" ||
		first.PostedAt.Format("2006-01-02") != "2026-08-24" || first.Source != SourceCareers {
		t.Errorf("first position: %+v", first)
	}
	text, err := parseCareersDetail(fixture(t, "careers-detail.json"))
	if err != nil {
		t.Fatal(err)
	}
	if lo, hi := parseSalary(text); lo == nil || *lo != 320000 || *hi != 500000 {
		t.Errorf("careers detail salary: %v %v from %q", lo, hi, text)
	}
}

func TestParseLinkedIn(t *testing.T) {
	list := parseLinkedInList(fixture(t, "linkedin-list.html"), "Netflix")
	if len(list) != 4 {
		t.Fatalf("%d cards: %+v", len(list), list)
	}
	first := list[0]
	if first.ExternalID != "4423102206" || first.Title != "Software Engineer 5 – Agent Platform, AI Platform" ||
		first.Location != "United States" || first.PostedAt.Format("2006-01-02") != "2026-10-16" || first.Source != SourceLinkedIn {
		t.Errorf("first card: %+v", first)
	}
	if first.URL != "https://www.linkedin.com/jobs/view/software-engineer-5-%E2%80%93-agent-platform-ai-platform-at-netflix-4423102206" {
		t.Errorf("url without the tracking query: %q", first.URL)
	}
	if last := list[3]; last.Location != "New York, NY" || !wantedLevel(last.Title) {
		t.Errorf("New York card: %+v", last)
	}
	if other := parseLinkedInList(fixture(t, "linkedin-list.html"), "Some Other Co"); len(other) != 0 {
		t.Errorf("cards from another company must be skipped, got %d", len(other))
	}
	if lo, hi := parseSalary(htmlText(string(fixture(t, "linkedin-detail.html")))); lo == nil || hi == nil || *lo >= *hi {
		t.Errorf("linkedin detail salary: %v %v", lo, hi)
	}
}
