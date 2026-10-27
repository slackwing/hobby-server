package sxiva

// Netflix roles watcher (admin-only plugin on the status page; feathers
// html/status/netflix-roles.js). Every hour it catalogs open Software
// Engineer 5 and 6 roles from two sources — the Netflix careers site and
// LinkedIn's public (no-login) job search — with their salary ranges.
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
	SourceCareers  = "careers"
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

// normalizeTitle: lowercase, en/em dashes as "-", whitespace collapsed —
// so trivial differences between sources don't make a new role.
func normalizeTitle(t string) string {
	t = strings.ToLower(t)
	t = strings.NewReplacer("–", "-", "—", "-").Replace(t)
	return strings.Join(strings.Fields(t), " ")
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

// ---- careers site (Eightfold JSON) ----------------------------------------

// parseCareersList reads one page of /api/apply/v2/jobs: the total match
// count and the page's positions.
func parseCareersList(body []byte) (int, []Listing, error) {
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
		return 0, nil, fmt.Errorf("careers list: %w", err)
	}
	out := make([]Listing, 0, len(resp.Positions))
	for _, p := range resp.Positions {
		out = append(out, Listing{
			Source:     SourceCareers,
			ExternalID: p.ID.String(),
			Title:      strings.TrimSpace(p.Name),
			Location:   strings.TrimSpace(p.Location),
			URL:        p.URL,
			PostedAt:   time.Unix(p.TCreate, 0).UTC(),
		})
	}
	return resp.Count, out, nil
}

// parseCareersDetail returns a position's description as text.
func parseCareersDetail(body []byte) (string, error) {
	var resp struct {
		Description string `json:"job_description"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("careers detail: %w", err)
	}
	return htmlText(resp.Description), nil
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
