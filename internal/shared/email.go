package shared

import (
	"context"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"

	"encoding/json"

	"github.com/slackwing/hobby-server/internal/mailer"
)

// Email templates are NOT stored on this server. Each website keeps
// them as static files in its own directory of the feathers repo, in a
// standard-named subdirectory:
//
//	/<website>/_email/templates.json   manifest (see manifest below)
//	/<website>/_email/<id>.html        one HTML body per template
//
// so a site's emails ship, version, and re-theme together with the
// site. The server fetches them over HTTP from the public site (base
// URL derived from the request, or config email.site_base_url in dev)
// and substitutes {{variables}}. The same files render client-side in
// each site's /_email/ admin preview page, which uses the identical
// variable set — keep the two in sync (docs/SHARED_AUTH.md lists them).
//
// Variables: display_name, username, initial, color, color_text,
// email, website, site_url, base_url, invite_url, expires_at, year.
// Values are HTML-escaped in bodies, raw in subjects. invite_url and
// expires_at are only set for templates with "invite": true (a fresh
// 7-day invite link is minted per send).

type emailTemplate struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Invite  bool   `json:"invite"`
	// On names an event the server sends this template on
	// automatically: "accept" = after the user sets a password via an
	// invite link for this website (the welcome email).
	On string `json:"on"`
}

type manifest struct {
	Templates []emailTemplate `json:"templates"`
}

// Email is the shared-auth email facility: a mailer plus how to reach
// the sites' template files.
type Email struct {
	Mailer  mailer.Mailer
	BaseURL string // "" → derive from request
}

func (e Email) Configured() bool { return e.Mailer.Configured() }

// base returns the public site root, no trailing slash.
func (e Email) base(r *http.Request) string {
	if e.BaseURL != "" {
		return strings.TrimRight(e.BaseURL, "/")
	}
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
	}
	return proto + "://" + r.Host
}

var (
	templateIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	varRe        = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`)

	ErrNoTemplate = errors.New("no such template")
	ErrNoEmail    = errors.New("user has no email address")
)

func fetchText(ctx context.Context, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// loadTemplate fetches a website's manifest and the named template body.
func (e Email) loadTemplate(ctx context.Context, base, website, id string) (emailTemplate, string, error) {
	if !templateIDRe.MatchString(id) {
		return emailTemplate{}, "", ErrNoTemplate
	}
	raw, err := fetchText(ctx, fmt.Sprintf("%s/%s/_email/templates.json", base, website))
	if err != nil {
		return emailTemplate{}, "", fmt.Errorf("manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return emailTemplate{}, "", fmt.Errorf("manifest: %w", err)
	}
	for _, t := range m.Templates {
		if t.ID == id {
			body, err := fetchText(ctx, fmt.Sprintf("%s/%s/_email/%s.html", base, website, id))
			if err != nil {
				return emailTemplate{}, "", fmt.Errorf("template body: %w", err)
			}
			return t, body, nil
		}
	}
	return emailTemplate{}, "", ErrNoTemplate
}

// templateFor finds the template a website sends on an event, if any.
func (e Email) templateFor(ctx context.Context, base, website, on string) (string, bool) {
	raw, err := fetchText(ctx, fmt.Sprintf("%s/%s/_email/templates.json", base, website))
	if err != nil {
		return "", false
	}
	var m manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return "", false
	}
	for _, t := range m.Templates {
		if t.On == on {
			return t.ID, true
		}
	}
	return "", false
}

func render(s string, vars map[string]string, escape bool) string {
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		key := varRe.FindStringSubmatch(m)[1]
		v := vars[key]
		if escape {
			return html.EscapeString(v)
		}
		return v
	})
}

// SendResult is what the console shows after a send.
type SendResult struct {
	To        string     `json:"to"`
	Subject   string     `json:"subject"`
	Template  string     `json:"template"`
	InviteURL string     `json:"invite_url,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Send renders website's template id for acct and emails it. For
// invite templates a fresh invite link is minted via store.
func (e Email) Send(ctx context.Context, store *Store, base, website, id string, acct *Account) (*SendResult, error) {
	if !e.Configured() {
		return nil, mailer.ErrNotConfigured
	}
	if acct.Email == "" {
		return nil, ErrNoEmail
	}
	tpl, body, err := e.loadTemplate(ctx, base, website, id)
	if err != nil {
		return nil, err
	}
	vars := map[string]string{
		"display_name": acct.DisplayName,
		"username":     acct.Username,
		"initial":      acct.Initial,
		"color":        acct.Color,
		"color_text":   TextColorFor(acct.Color),
		"email":        acct.Email,
		"website":      website,
		"site_url":     base + "/" + website + "/",
		"base_url":     base,
		"year":         fmt.Sprint(time.Now().Year()),
	}
	res := &SendResult{To: acct.Email, Template: id}
	if tpl.Invite {
		code, expiresAt, err := store.CreateToken(acct.Username, website, InviteTTL)
		if err != nil {
			return nil, err
		}
		vars["invite_url"] = fmt.Sprintf("%s/%s/_invite/?code=%s", base, website, code)
		vars["expires_at"] = expiresAt.Format("Mon, Jan 2")
		res.InviteURL = vars["invite_url"]
		res.ExpiresAt = &expiresAt
	}
	res.Subject = render(tpl.Subject, vars, false)
	if err := e.Mailer.Send(acct.Email, res.Subject, render(body, vars, true)); err != nil {
		return nil, err
	}
	return res, nil
}

// SendOnAccept fires a website's "on": "accept" template (the welcome
// email) after an invite is accepted. Best effort, run in a goroutine;
// silent when email is unconfigured or the site has no such template.
func (e Email) SendOnAccept(store *Store, base, website, username string) {
	if !e.Configured() || website == "admin" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := e.templateFor(ctx, base, website, "accept")
	if !ok {
		return
	}
	acct, err := store.GetUser(username)
	if err != nil || acct == nil || acct.Email == "" {
		return
	}
	if _, err := e.Send(ctx, store, base, website, id, acct); err != nil {
		log.Printf("[admin] on-accept email (%s/%s) for %s failed: %v", website, id, username, err)
		return
	}
	log.Printf("[admin] sent %s/%s email to %s", website, id, username)
}
