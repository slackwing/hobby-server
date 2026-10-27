package sxiva

import (
	"os"
	"strings"
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
	same := [][2]string{
		{"Software Engineer 5 – iOS", "Software Engineer 5 - iOS"}, // en dash
		{"Software Engineer 4/5 – Model Development and Management, AI Platform", "Software Engineer 5 – Model Development and Management, AI Platform"},
		{"Software Engineering 5 - Ads Agent", "Software Engineer 5 - Ads Agent"},
	}
	for _, c := range same {
		if RoleID(c[0]) != RoleID(c[1]) {
			t.Errorf("%q and %q must be one role", c[0], c[1])
		}
	}
	if RoleID("Software Engineer 5/6 - Streaming") == RoleID("Software Engineer 5 - Streaming") {
		t.Error("5/6 keeps both levels")
	}
	if got := normalizeTitle("Software Engineer 3/4 - X"); got != "software engineer 3/4 - x" {
		t.Errorf("a pair with no kept level is left alone: %q", got)
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

func TestFilters(t *testing.T) {
	places := map[string]bool{
		"New York, NY":                    true,
		"New York, United States":         true,
		"New York City Metropolitan Area": true,
		"NYC":                             true,
		"Hybrid - NYC office":             true,
		"United States":                   true,
		"US":                              true,
		"USA":                             true,
		"United States (Remote)":          true,
		"":                                true, // no location: kept
		"Los Gatos, CA":                   false,
		"Seattle, WA":                     false,
		"Los Angeles, CA":                 false,
		"Los Gatos, CA, United States":    false, // a city, though the country is named
		"Austin, TX (Remote)":             false,
	}
	for loc, want := range places {
		if got := wantedLinkedInPlace(loc); got != want {
			t.Errorf("wantedLinkedInPlace(%q) = %v, want %v", loc, got, want)
		}
	}
	roles := map[string]bool{
		"Software Engineer 5 - iOS":                    true,
		"Software Engineer 5 – IOS Platform":           true,
		"Software Engineer 6 - Mobile (iOS)":           true,
		"Software Engineer 5 - Android":                true,
		"Software Engineer 5 - Mobile (Android/iOS)":   true,
		"Security Software Engineer 5":                 true,
		"Software Engineer (L6), Platform Security":    true,
		"Software Engineer 5 - Netflix Studios":        false, // "ios" inside a word
		"Software Engineer 5 - TV Product UI":          false,
		"Software Engineer 6 - Ads Identity & Privacy": false,
	}
	for title, want := range roles {
		if got := unwantedRole(title); got != want {
			t.Errorf("unwantedRole(%q) = %v, want %v", title, got, want)
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

func metaValue(meta []MetaField, label string) (string, bool) {
	for _, m := range meta {
		if m.Label == label {
			return m.Value, true
		}
	}
	return "", false
}

func TestParseNetflix(t *testing.T) {
	count, list, err := parseNetflixList(fixture(t, "netflix-list.json"))
	if err != nil {
		t.Fatal(err)
	}
	if count != 65 || len(list) != 6 {
		t.Fatalf("count %d, %d positions", count, len(list))
	}
	first := list[0]
	if first.ExternalID != "790318081427" || first.Title != "Software Engineer 5 – Model Development and Management, AI Platform" ||
		first.Location != "Remote, United States" || first.URL != "https://explore.jobs.netflix.net/careers/job/790318081427" ||
		first.PostedAt.Format("2006-01-02") != "2026-08-24" || first.Source != SourceNetflix {
		t.Errorf("first position: %+v", first)
	}

	d, err := parseNetflixDetail(fixture(t, "netflix-detail.json"))
	if err != nil {
		t.Fatal(err)
	}
	if d.PostedAt == nil || d.PostedAt.Format("2006-01-02") != "2026-10-23" {
		t.Errorf("posted %v: want the site's Job Posting Date (2026-10-23), not t_create (2026-08-21)", d.PostedAt)
	}
	if lo, hi := parseSalary(d.SalaryText); lo == nil || *lo != 320000 || *hi != 500000 {
		t.Errorf("netflix detail salary: %v %v", lo, hi)
	}
	if !strings.HasPrefix(d.Description, "<p>At Netflix, our mission") {
		t.Errorf("description keeps the HTML: %.60q", d.Description)
	}
	want := []MetaField{
		{"Job Requisition ID", "JR42274"}, {"Teams", "Engineering"}, {"Work Type", "Remote"},
		{"Department", "Engineering"}, {"Business unit", "Streaming"}, {"Locations", "Remote, United States"},
		{"Created", "2026-08-21"},
	}
	if len(d.Meta) != len(want) {
		t.Fatalf("meta %+v", d.Meta)
	}
	for i := range want {
		if d.Meta[i] != want[i] {
			t.Errorf("meta[%d] = %+v, want %+v (empty fields and the posting date are left out)", i, d.Meta[i], want[i])
		}
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

	d := parseLinkedInDetail(fixture(t, "linkedin-detail.html"))
	if lo, hi := parseSalary(d.SalaryText); lo == nil || *lo != 388000 || *hi != 558000 {
		t.Errorf("linkedin detail salary: %v %v", lo, hi)
	}
	if !strings.HasPrefix(d.Description, "At Netflix, our mission") || strings.Contains(d.Description, "show-more-less") {
		t.Errorf("description is the posting's own markup: %.80q", d.Description)
	}
	for label, want := range map[string]string{
		"Location": "United States", "Seniority level": "Not Applicable", "Employment type": "Full-time",
		"Industries": "Entertainment Providers", "Applicants": "Over 200",
	} {
		if got, ok := metaValue(d.Meta, label); !ok || got != want {
			t.Errorf("meta %q = %q, want %q (all: %+v)", label, got, want, d.Meta)
		}
	}
	if d.PostedAt != nil {
		t.Error("LinkedIn's list already dates the posting")
	}
}
