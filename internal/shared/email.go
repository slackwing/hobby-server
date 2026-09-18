package shared

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/slackwing/hobby-server/internal/mailer"
)

// Email templates are NOT stored on this server. Each website keeps
// them as static files in its own directory of the feathers repo, in a
// standard-named subdirectory:
//
//	/<website>/_email/templates.json   manifest (see manifest below)
//	/<website>/_email/_layout.html     optional shared frame (title bar,
//	                                   kicker, heading, footer) with
//	                                   {{title}} {{kicker}} {{heading}} {{body}}
//	/<website>/_email/<id>.html        one body per template
//
// so a site's emails ship, version, and re-theme together with the
// site, and the frame is written once. The server fetches them over
// HTTP from the public site (base URL derived from the request, or
// config email.site_base_url in dev) and substitutes {{variables}}. The
// same files render client-side in each site's /_email/ admin preview
// page, which uses the identical variable set — keep the two in sync
// (docs/SHARED_AUTH.md lists them).
//
// Variables: display_name, username, initial, color, color_text, email,
// website, site_url, base_url, invite_url + expires_at (templates with
// "invite": true — a fresh 7-day invite link is minted per send),
// reset_url + expires_at (templates with "reset": true — a 1-hour reset
// link), year. Values are HTML-escaped in bodies and the layout, raw in
// the manifest's subject/title/kicker/heading.

type emailTemplate struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Subject string `json:"subject"`
	Title   string `json:"title"`   // layout: title-bar text
	Kicker  string `json:"kicker"`  // layout: small caps line
	Heading string `json:"heading"` // layout: heading (raw HTML allowed)
	Invite  bool   `json:"invite"`
	Reset   bool   `json:"reset"`
	// On names an event the server sends this template on
	// automatically: "accept" = after the user sets a password via an
	// invite link for this website (the account-created email).
	On string `json:"on"`
}

type manifest struct {
	Layout    string          `json:"layout"` // default "_layout.html"; "" after load = none
	Templates []emailTemplate `json:"templates"`
}

// Email is the shared-auth email facility: a mailer plus how to reach
// the sites' template files.
type Email struct {
	Mailer  mailer.Mailer
	BaseURL string // "" → derive from request

	mu    sync.Mutex
	pages map[string]pageProbe // "<base>/<site>/_invite/" → exists?
}

type pageProbe struct {
	ok   bool
	when time.Time
}

func (e *Email) Configured() bool { return e.Mailer.Configured() }

// base returns the public site root, no trailing slash.
func (e *Email) base(r *http.Request) string {
	if e.BaseURL != "" {
		return strings.TrimRight(e.BaseURL, "/")
	}
	return requestBase(r)
}

var (
	templateIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)
	varRe        = regexp.MustCompile(`\{\{\s*([a-z_]+)\s*\}\}`)

	ErrNoTemplate = errors.New("no such template")
	ErrNoEmail    = errors.New("user has no email address")

	// Times in emails are shown in the party's timezone.
	eastern = func() *time.Location {
		if l, err := time.LoadLocation("America/New_York"); err == nil {
			return l
		}
		return time.FixedZone("ET", -5*3600)
	}()
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

func (e *Email) loadManifest(ctx context.Context, base, website string) (manifest, error) {
	raw, err := fetchText(ctx, fmt.Sprintf("%s/%s/_email/templates.json", base, website))
	if err != nil {
		return manifest{}, fmt.Errorf("manifest: %w", err)
	}
	m := manifest{Layout: "_layout.html"}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return manifest{}, fmt.Errorf("manifest: %w", err)
	}
	return m, nil
}

// loadTemplate fetches a website's manifest, the named template body,
// and the layout (empty when the site has none).
func (e *Email) loadTemplate(ctx context.Context, base, website, id string) (emailTemplate, string, string, error) {
	if !templateIDRe.MatchString(id) {
		return emailTemplate{}, "", "", ErrNoTemplate
	}
	m, err := e.loadManifest(ctx, base, website)
	if err != nil {
		return emailTemplate{}, "", "", err
	}
	for _, t := range m.Templates {
		if t.ID != id {
			continue
		}
		body, err := fetchText(ctx, fmt.Sprintf("%s/%s/_email/%s.html", base, website, id))
		if err != nil {
			return emailTemplate{}, "", "", fmt.Errorf("template body: %w", err)
		}
		layout := ""
		if m.Layout != "" {
			if layout, err = fetchText(ctx, fmt.Sprintf("%s/%s/_email/%s", base, website, m.Layout)); err != nil {
				return emailTemplate{}, "", "", fmt.Errorf("layout: %w", err)
			}
		}
		return t, body, layout, nil
	}
	return emailTemplate{}, "", "", ErrNoTemplate
}

// TemplateFor finds the id of a website's template matching pred.
func (e *Email) TemplateFor(ctx context.Context, base, website string, pred func(emailTemplate) bool) (string, bool) {
	m, err := e.loadManifest(ctx, base, website)
	if err != nil {
		return "", false
	}
	for _, t := range m.Templates {
		if pred(t) {
			return t.ID, true
		}
	}
	return "", false
}

// render substitutes {{vars}}. Keys in raw are inserted unescaped (the
// layout's pre-rendered slots); everything else is HTML-escaped unless
// escape is false. Unknown names become empty.
func render(s string, vars map[string]string, escape bool, raw ...string) string {
	isRaw := map[string]bool{}
	for _, k := range raw {
		isRaw[k] = true
	}
	return varRe.ReplaceAllStringFunc(s, func(m string) string {
		key := varRe.FindStringSubmatch(m)[1]
		v := vars[key]
		if escape && !isRaw[key] {
			return html.EscapeString(v)
		}
		return v
	})
}

// PagePath returns the page for a token kind on a website — the site's
// own /_invite/ or /_reset/ skin when it exists, else the default page
// under /admin/. Existence is probed with a HEAD request and cached.
func (e *Email) PagePath(ctx context.Context, base, website, kind string) string {
	if website == "" || website == "admin" {
		return "/admin/_" + kind + "/"
	}
	own := fmt.Sprintf("/%s/_%s/", website, kind)
	url := base + own
	e.mu.Lock()
	if e.pages == nil {
		e.pages = map[string]pageProbe{}
	}
	p, hit := e.pages[url]
	e.mu.Unlock()
	if !hit || time.Since(p.when) > 10*time.Minute {
		p = pageProbe{when: time.Now()}
		if req, err := http.NewRequestWithContext(ctx, http.MethodHead, url, nil); err == nil {
			if resp, err := http.DefaultClient.Do(req); err == nil {
				resp.Body.Close()
				p.ok = resp.StatusCode == http.StatusOK
			}
		}
		e.mu.Lock()
		e.pages[url] = p
		e.mu.Unlock()
	}
	if p.ok {
		return own
	}
	return "/admin/_" + kind + "/"
}

// SendResult is what the console shows after a send.
type SendResult struct {
	To        string     `json:"to"`
	Subject   string     `json:"subject"`
	Template  string     `json:"template"`
	URL       string     `json:"url,omitempty"` // the minted invite/reset link
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
}

// Send renders website's template id for acct and emails it. Invite and
// reset templates mint a fresh link via store.
func (e *Email) Send(ctx context.Context, store *Store, base, website, id string, acct *Account) (*SendResult, error) {
	if !e.Configured() {
		return nil, mailer.ErrNotConfigured
	}
	if acct.Email == "" {
		return nil, ErrNoEmail
	}
	tpl, body, layout, err := e.loadTemplate(ctx, base, website, id)
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
		"year":         fmt.Sprint(time.Now().In(eastern).Year()),
	}
	res := &SendResult{To: acct.Email, Template: id}
	if tpl.Invite || tpl.Reset {
		kind, ttl := KindInvite, InviteTTL
		if tpl.Reset {
			kind, ttl = KindReset, ResetTTL
		}
		code, expiresAt, err := store.CreateToken(acct.Username, website, kind, ttl)
		if err != nil {
			return nil, err
		}
		url := fmt.Sprintf("%s%s?code=%s", base, e.PagePath(ctx, base, website, kind), code)
		vars[kind+"_url"] = url
		if tpl.Reset {
			vars["expires_at"] = expiresAt.In(eastern).Format("Mon, Jan 2 at 3:04 PM MST")
		} else {
			vars["expires_at"] = expiresAt.In(eastern).Format("Mon, Jan 2")
		}
		res.URL = url
		res.ExpiresAt = &expiresAt
	}
	res.Subject = render(tpl.Subject, vars, false)
	html := render(body, vars, true)
	if layout != "" {
		vars["title"] = render(tpl.Title, vars, false)
		vars["kicker"] = render(tpl.Kicker, vars, false)
		vars["heading"] = render(tpl.Heading, vars, false)
		vars["subject"] = res.Subject
		vars["body"] = html
		html = render(layout, vars, true, "title", "kicker", "heading", "body")
	}
	if err := e.Mailer.Send(acct.Email, res.Subject, html); err != nil {
		return nil, err
	}
	return res, nil
}

// SendOnAccept fires a website's "on": "accept" template (the
// account-created email) after an invite is accepted. Best effort, run
// in a goroutine; silent when email is unconfigured or the site has no
// such template.
func (e *Email) SendOnAccept(store *Store, base, website, username string) {
	if !e.Configured() || website == "admin" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := e.TemplateFor(ctx, base, website, func(t emailTemplate) bool { return t.On == "accept" })
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

// SendReset emails the user's active site's reset template. Used by
// the public forgot-password endpoint; errors are logged, not returned,
// so the caller can answer the same way whether or not a mail went out.
func (e *Email) SendReset(store *Store, base string, acct *Account) {
	if !e.Configured() || acct == nil || acct.Email == "" || acct.ActiveSite == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	id, ok := e.TemplateFor(ctx, base, acct.ActiveSite, func(t emailTemplate) bool { return t.Reset })
	if !ok {
		log.Printf("[admin] forgot-password: %s has no reset template", acct.ActiveSite)
		return
	}
	if _, err := e.Send(ctx, store, base, acct.ActiveSite, id, acct); err != nil {
		log.Printf("[admin] reset email (%s/%s) for %s failed: %v", acct.ActiveSite, id, acct.Username, err)
		return
	}
	log.Printf("[admin] sent %s/%s email to %s", acct.ActiveSite, id, acct.Username)
}
