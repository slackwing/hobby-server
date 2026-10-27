package sxiva

// Netflix roles watcher (admin-only plugin on the status page; feathers
// html/status/netflix-roles.js). Every hour it catalogs open Software
// Engineer 5 and 6 roles from two sources — Netflix's own job site
// (explore.jobs.netflix.net) and LinkedIn's public (no-login) job search —
// with each posting's description, details and salary range.
//
// This file holds the pure parts: titles, role ids, levels, salaries, and
// parsing each source's responses. Fetching and storage: jobs_watch.go.

import (
	"crypto/sha1"
	"encoding/json"
	"fmt"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sources.
const (
	SourceNetflix  = "netflix"
	SourceLinkedIn = "linkedin"
)

// Listing is one posting as a source shows it.
type Listing struct {
	Source     string
	ExternalID string
	Title      string
	Location   string
	URL        string
	PostedAt   time.Time
}

// jobRoleNamespace = uuid5(NAMESPACE_URL, "https://andrewcheong.com/status/jobs").
var jobRoleNamespace = [16]byte{0xa2, 0xdf, 0x09, 0xa3, 0x7a, 0xeb, 0x59, 0x2e, 0x91, 0x86, 0x26, 0x1f, 0x85, 0xfb, 0xbc, 0xb2}

// MetaField is one labeled detail of a posting, as its source shows it.
type MetaField struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// Detail is what a posting's own page adds to its list entry.
type Detail struct {
	Description string      // the source's HTML (the page sanitizes it)
	Meta        []MetaField // in the source's order
	PostedAt    *time.Time  // the source's displayed posting date, when the list's differs
	SalaryText  string      // text to read a "$X - $Y" range from
}

var (
	engineeringRe = regexp.MustCompile(`software engineering\b`)
	levelPairRe   = regexp.MustCompile(`software engineer (\d) ?/ ?(\d)`)
)

// normalizeTitle: lowercase, en/em dashes as "-", whitespace collapsed,
// "engineering" as "engineer", and a level pair narrowed to the levels we
// keep ("4/5" → "5") — so the ways sources spell one role don't make two.
func normalizeTitle(t string) string {
	t = strings.ToLower(t)
	t = strings.NewReplacer("–", "-", "—", "-").Replace(t)
	t = strings.Join(strings.Fields(t), " ")
	t = engineeringRe.ReplaceAllString(t, "software engineer")
	return levelPairRe.ReplaceAllStringFunc(t, func(m string) string {
		p := levelPairRe.FindStringSubmatch(m)
		var keep []string
		for _, l := range p[1:] {
			if l == "5" || l == "6" {
				keep = append(keep, l)
			}
		}
		if len(keep) == 0 {
			return m
		}
		return "software engineer " + strings.Join(keep, "/")
	})
}

// RoleID is the role's UUID: a version-5 (SHA-1) UUID of its normalized
// title, so a role keeps its id whenever and wherever it reappears.
func RoleID(title string) string {
	h := sha1.New()
	h.Write(jobRoleNamespace[:])
	h.Write([]byte(normalizeTitle(title)))
	var u [16]byte
	copy(u[:], h.Sum(nil))
	u[6] = (u[6] & 0x0f) | 0x50 // version 5
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return fmt.Sprintf("%x-%x-%x-%x-%x", u[0:4], u[4:6], u[6:8], u[8:10], u[10:])
}

var (
	softwareEngRe = regexp.MustCompile(`(?i)software\s+engineer`)
	levelRe       = regexp.MustCompile(`(?i)software\s+engineer(?:ing)?\s+(\d)(?:\s*/\s*(\d))?`)
	lLevelRe      = regexp.MustCompile(`(?i)\(L(\d)\)`)
)

// wantedLevel: a Software Engineer title naming level 5 or 6 — "Software
// Engineer 5", "Software Engineering 6", "Software Engineer 4/5", "Senior
// Software Engineer (L5)". Pure SE4 roles, and other jobs at L5/L6 (the
// careers search is fuzzy), are dropped.
func wantedLevel(title string) bool {
	if !softwareEngRe.MatchString(title) {
		return false
	}
	var levels []string
	for _, m := range levelRe.FindAllStringSubmatch(title, -1) {
		levels = append(levels, m[1], m[2])
	}
	for _, m := range lLevelRe.FindAllStringSubmatch(title, -1) {
		levels = append(levels, m[1])
	}
	for _, l := range levels {
		if l == "5" || l == "6" {
			return true
		}
	}
	return false
}

var iosRe = regexp.MustCompile(`(?i)\bios\b`)

// unwantedRole: roles Andrew doesn't want from any source — iOS.
func unwantedRole(title string) bool {
	return iosRe.MatchString(title)
}

var (
	newYorkRe     = regexp.MustCompile(`(?i)new york|\bnyc\b`)
	placeNoiseRe  = regexp.MustCompile(`(?i)\bremote\b|[(),.\-–—]`)
	countryLabels = map[string]bool{"us": true, "usa": true, "united states": true, "united states of america": true}
)

// wantedLinkedInPlace: LinkedIn's US search also lists on-site jobs in
// Los Gatos, LA, Seattle…; Andrew wants New York only, so a posting stays
// when its location mentions New York / NYC, is just the country ("United
// States", "US", "USA" — how LinkedIn labels Netflix's remote US jobs; a
// "Remote" beside it is fine), or is missing. "Los Gatos, CA, United
// States" names a city: dropped.
func wantedLinkedInPlace(location string) bool {
	if newYorkRe.MatchString(location) {
		return true
	}
	bare := strings.ToLower(strings.Join(strings.Fields(placeNoiseRe.ReplaceAllString(location, " ")), " "))
	return bare == "" || countryLabels[bare]
}

var salaryRe = regexp.MustCompile(`\$\s?([\d,]+(?:\.\d+)?)\s*(?:-|–|—|to)\s*\$\s?([\d,]+(?:\.\d+)?)`)

// parseSalary finds the first "$X - $Y" range in text ("The range for
// this role is $320,000.00 - $500,000.00"), in whole dollars.
func parseSalary(text string) (min, max *int) {
	m := salaryRe.FindStringSubmatch(text)
	if m == nil {
		return nil, nil
	}
	lo, err1 := strconv.ParseFloat(strings.ReplaceAll(m[1], ",", ""), 64)
	hi, err2 := strconv.ParseFloat(strings.ReplaceAll(m[2], ",", ""), 64)
	if err1 != nil || err2 != nil || lo < 1000 || hi < lo {
		return nil, nil
	}
	a, b := int(lo), int(hi)
	return &a, &b
}

var tagRe = regexp.MustCompile(`<[^>]+>`)

// htmlText strips tags and decodes entities.
func htmlText(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(tagRe.ReplaceAllString(s, " "))), " ")
}

// ---- Netflix job site (explore.jobs.netflix.net, Eightfold JSON) ------------

// parseNetflixList reads one page of /api/apply/v2/jobs: the total match
// count and the page's positions. PostedAt here is the position's creation
// (t_create) — often months before the posting date the site shows, which
// only the detail has (parseNetflixDetail).
func parseNetflixList(body []byte) (int, []Listing, error) {
	var resp struct {
		Count     int `json:"count"`
		Positions []struct {
			ID       json.Number `json:"id"`
			Name     string      `json:"name"`
			Location string      `json:"location"`
			TCreate  int64       `json:"t_create"`
			URL      string      `json:"canonicalPositionUrl"`
		} `json:"positions"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return 0, nil, fmt.Errorf("netflix list: %w", err)
	}
	out := make([]Listing, 0, len(resp.Positions))
	for _, p := range resp.Positions {
		out = append(out, Listing{
			Source:     SourceNetflix,
			ExternalID: p.ID.String(),
			Title:      strings.TrimSpace(p.Name),
			Location:   strings.TrimSpace(p.Location),
			URL:        p.URL,
			PostedAt:   time.Unix(p.TCreate, 0).UTC(),
		})
	}
	return resp.Count, out, nil
}

// parseNetflixDetail reads /api/apply/v2/jobs/{id}: the description, the
// labeled fields the site lists beside it (custom_JD), the position's own
// fields, and the displayed posting date.
func parseNetflixDetail(body []byte) (Detail, error) {
	var resp struct {
		Description  string   `json:"job_description"`
		Department   string   `json:"department"`
		BusinessUnit string   `json:"business_unit"`
		Locations    []string `json:"locations"`
		WorkLocation *string  `json:"work_location_option"`
		Flexibility  *string  `json:"location_flexibility"`
		TCreate      int64    `json:"t_create"`
		CustomJD     struct {
			Fields  map[string][]string `json:"data_fields"`
			Display []struct {
				Label string `json:"label"`
				Value string `json:"value"`
			} `json:"display"`
		} `json:"custom_JD"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return Detail{}, fmt.Errorf("netflix detail: %w", err)
	}
	d := Detail{Description: resp.Description, SalaryText: htmlText(resp.Description)}
	add := func(label, value string) {
		if value = strings.TrimSpace(value); value != "" {
			d.Meta = append(d.Meta, MetaField{label, value})
		}
	}
	for _, f := range resp.CustomJD.Display {
		values := resp.CustomJD.Fields[f.Value]
		if f.Value == "posting_date" {
			if len(values) > 0 {
				if t, err := time.Parse("01-02-2006", strings.TrimSpace(values[0])); err == nil {
					d.PostedAt = &t
				}
			}
			continue // shown as the posting's date
		}
		add(f.Label, strings.Join(values, ", "))
	}
	add("Department", resp.Department)
	add("Business unit", resp.BusinessUnit)
	add("Locations", strings.Join(resp.Locations, "; "))
	if resp.WorkLocation != nil {
		add("Work location", *resp.WorkLocation)
	}
	if resp.Flexibility != nil {
		add("Location flexibility", *resp.Flexibility)
	}
	if resp.TCreate > 0 {
		add("Created", time.Unix(resp.TCreate, 0).UTC().Format("2006-01-02"))
	}
	return d, nil
}

// ---- LinkedIn public job search (HTML cards) -------------------------------

var (
	liCardRe     = regexp.MustCompile(`(?s)<li>.*?</li>`)
	liIDRe       = regexp.MustCompile(`data-entity-urn="urn:li:jobPosting:(\d+)"`)
	liTitleRe    = regexp.MustCompile(`(?s)base-search-card__title">\s*(.*?)\s*</h3>`)
	liCompanyRe  = regexp.MustCompile(`(?s)base-search-card__subtitle">.*?<a[^>]*>\s*(.*?)\s*</a>`)
	liLocationRe = regexp.MustCompile(`(?s)job-search-card__location">\s*(.*?)\s*</span>`)
	liDateRe     = regexp.MustCompile(`<time[^>]*datetime="(\d{4}-\d{2}-\d{2})"`)
	liURLRe      = regexp.MustCompile(`base-card__full-link[^"]*"\s+href="([^"]+)"`)
)

// parseLinkedInList reads one page of job cards, keeping company's own
// (company filter) cards that have an id, title and date.
func parseLinkedInList(body []byte, company string) []Listing {
	var out []Listing
	for _, card := range liCardRe.FindAllString(string(body), -1) {
		id, title, date := liIDRe.FindStringSubmatch(card), liTitleRe.FindStringSubmatch(card), liDateRe.FindStringSubmatch(card)
		if id == nil || title == nil || date == nil {
			continue
		}
		if c := liCompanyRe.FindStringSubmatch(card); c == nil || !strings.EqualFold(htmlText(c[1]), company) {
			continue
		}
		posted, err := time.Parse("2006-01-02", date[1])
		if err != nil {
			continue
		}
		l := Listing{
			Source:     SourceLinkedIn,
			ExternalID: id[1],
			Title:      htmlText(title[1]),
			PostedAt:   posted,
			URL:        "https://www.linkedin.com/jobs/view/" + id[1],
		}
		if loc := liLocationRe.FindStringSubmatch(card); loc != nil {
			l.Location = htmlText(loc[1])
		}
		if u := liURLRe.FindStringSubmatch(card); u != nil {
			full := html.UnescapeString(u[1])
			if i := strings.IndexByte(full, '?'); i > 0 {
				full = full[:i]
			}
			l.URL = full
		}
		out = append(out, l)
	}
	return out
}

var (
	liDescRe       = regexp.MustCompile(`(?s)<div class="show-more-less-html__markup[^"]*">(.*?)</div>`)
	liCriterionRe  = regexp.MustCompile(`(?s)<h3 class="description__job-criteria-subheader">(.*?)</h3>\s*<span class="description__job-criteria-text[^"]*">(.*?)</span>`)
	liApplicantsRe = regexp.MustCompile(`(?s)class="num-applicants__caption[^"]*">(.*?)</`)
	liPlaceRe      = regexp.MustCompile(`(?s)<span class="topcard__flavor topcard__flavor--bullet">(.*?)</span>`)
	liPayRe        = regexp.MustCompile(`(?s)<div class="salary compensation__salary">(.*?)</div>`)
)

// parseLinkedInDetail reads a /jobs-guest/jobs/api/jobPosting/{id} page:
// the description, the job criteria (seniority, employment type, function,
// industries), location, applicants and any stated pay.
func parseLinkedInDetail(body []byte) Detail {
	page := string(body)
	var d Detail
	if m := liDescRe.FindStringSubmatch(page); m != nil {
		d.Description = strings.TrimSpace(m[1])
	}
	add := func(label, value string) {
		if value = htmlText(value); value != "" {
			d.Meta = append(d.Meta, MetaField{label, value})
		}
	}
	if m := liPlaceRe.FindStringSubmatch(page); m != nil {
		add("Location", m[1])
	}
	for _, m := range liCriterionRe.FindAllStringSubmatch(page, -1) {
		add(htmlText(m[1]), m[2])
	}
	if m := liApplicantsRe.FindStringSubmatch(page); m != nil {
		add("Applicants", strings.TrimSuffix(htmlText(m[1]), " applicants"))
	}
	pay := ""
	if m := liPayRe.FindStringSubmatch(page); m != nil {
		pay = htmlText(m[1])
		add("Pay", pay)
	}
	d.SalaryText = pay + " " + htmlText(d.Description)
	return d
}
