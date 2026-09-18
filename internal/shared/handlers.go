package shared

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/slackwing/hobby-server/internal/mailer"
)

// CookieName is the SSO session cookie, shared by every website on the
// domain (Path=/ via the admin project's cookie_path).
const CookieName = "hobby_session"

// Mount wires the shared-auth endpoints onto the admin project's
// sub-router. cookiePath comes from config (should be "/"); secure
// mirrors the server env; email may be unconfigured (sends then 503).
func Mount(r chi.Router, store *Store, cookiePath string, secure bool, email *Email) {
	// Public.
	r.Post("/login", handleLogin(store, cookiePath, secure))
	r.Post("/logout", handleLogout(store, cookiePath, secure))
	r.Get("/me", handleMe(store))
	r.Get("/token-info", handleTokenInfo(store))
	r.Post("/set-password", handleSetPassword(store, cookiePath, secure, email))
	r.Post("/forgot", handleForgot(store, email))

	// Admin-only: requires role "admin" on website "admin".
	r.Group(func(g chi.Router) {
		g.Use(requireAdmin(store))
		g.Get("/users", handleListUsers(store))
		g.Post("/users", handleCreateUser(store))
		g.Patch("/users/{username}", handlePatchUser(store))
		g.Delete("/users/{username}", handleDeleteUser(store))
		g.Get("/websites", handleListWebsites(store))
		g.Post("/roles", handleAddRole(store))
		g.Delete("/roles", handleRemoveRole(store))
		g.Post("/links", handleCreateLink(store, email))
		g.Get("/email-status", handleEmailStatus(email))
		g.Post("/email", handleSendEmail(store, email))
	})
}

// sessionUser resolves the request's hobby_session cookie to a username.
func sessionUser(store *Store, r *http.Request) (string, bool) {
	cookie, err := r.Cookie(CookieName)
	if err != nil {
		return "", false
	}
	return store.GetSession(cookie.Value)
}

func requireAdmin(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username, ok := sessionUser(store, r)
			if !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			isAdmin, err := store.HasRole(username, "admin", "admin")
			if err != nil {
				log.Printf("[admin] role check error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !isAdmin {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func setCookie(w http.ResponseWriter, token, cookiePath string, secure, clear bool) {
	expires := time.Now().Add(SessionTTL)
	maxAge := int(SessionTTL.Seconds())
	if clear {
		expires = time.Unix(0, 0)
		maxAge = -1
	}
	http.SetCookie(w, &http.Cookie{
		Name:     CookieName,
		Value:    token,
		Path:     cookiePath,
		Expires:  expires,
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

// mePayload is what login, set-password, and /me all return.
func mePayload(store *Store, username string) (map[string]any, error) {
	acct, err := store.GetUser(username)
	if err != nil || acct == nil {
		return nil, fmt.Errorf("user %q lookup failed: %w", username, err)
	}
	roles, err := store.UserRoles(username)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"username":     acct.Username,
		"display_name": acct.DisplayName,
		"initial":      acct.Initial,
		"color":        acct.Color,
		"email":        acct.Email,
		"active_site":  acct.ActiveSite,
		"roles":        roles,
	}, nil
}

// requestBase mirrors Email.base for link building.
func requestBase(r *http.Request) string {
	proto := r.Header.Get("X-Forwarded-Proto")
	if proto == "" {
		proto = "http"
	}
	return proto + "://" + r.Host
}

// ---------- public handlers ----------

func handleLogin(store *Store, cookiePath string, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password required", http.StatusBadRequest)
			return
		}
		acct, err := store.GetUser(req.Username)
		if err != nil {
			log.Printf("[admin] login lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Unknown user and unset password both take the dummy-verify
		// path so timing doesn't leak which usernames exist.
		if acct == nil || acct.PasswordHash == nil {
			VerifyDummy(req.Password)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if !VerifyPassword(req.Password, *acct.PasswordHash) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		token, err := store.CreateSession(req.Username)
		if err != nil {
			log.Printf("[admin] session create error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		setCookie(w, token, cookiePath, secure, false)
		body, err := mePayload(store, req.Username)
		if err != nil {
			log.Printf("[admin] me payload error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, body)
	}
}

func handleLogout(store *Store, cookiePath string, secure bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie(CookieName); err == nil {
			store.DeleteSession(cookie.Value)
		}
		setCookie(w, "", cookiePath, secure, true)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleMe(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username, ok := sessionUser(store, r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		body, err := mePayload(store, username)
		if err != nil {
			log.Printf("[admin] me payload error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// handleTokenInfo lets the invite/reset pages prefill the (readonly)
// username field. Only valid codes get an answer, so this reveals
// nothing an invitee doesn't already hold.
func handleTokenInfo(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		code := r.URL.Query().Get("code")
		if code == "" {
			http.Error(w, "code required", http.StatusBadRequest)
			return
		}
		username, website, kind, expiresAt, ok, err := store.LookupToken(code)
		if err != nil {
			log.Printf("[admin] token lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "invalid or expired link", http.StatusNotFound)
			return
		}
		acct, err := store.GetUser(username)
		if err != nil || acct == nil {
			log.Printf("[admin] token user lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"username":     username,
			"display_name": acct.DisplayName,
			"website":      website,
			"kind":         kind,
			"expires_at":   expiresAt,
		})
	}
}

func handleSetPassword(store *Store, cookiePath string, secure bool, email *Email) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Code     string `json:"code"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Code == "" {
			http.Error(w, "code required", http.StatusBadRequest)
			return
		}
		if err := ValidatePassword(req.Password); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		hash, err := HashPassword(req.Password)
		if err != nil {
			log.Printf("[admin] hash error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		username, website, kind, ok, err := store.ConsumeToken(req.Code, hash)
		if err != nil {
			log.Printf("[admin] consume token error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "invalid or expired link", http.StatusNotFound)
			return
		}
		// Log them straight in — they just proved control of the link.
		token, err := store.CreateSession(username)
		if err != nil {
			log.Printf("[admin] session create error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		setCookie(w, token, cookiePath, secure, false)
		body, err := mePayload(store, username)
		if err != nil {
			log.Printf("[admin] me payload error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Account-created email, if the site defines one (best effort;
		// invites only — a reset is not a new account).
		if kind == KindInvite {
			go email.SendOnAccept(store, email.base(r), website, username)
		}
		writeJSON(w, http.StatusOK, body)
	}
}

// handleForgot is the public "forgot password" action: emails the
// user's active site's reset template. It always answers 204 so it
// reveals nothing about which usernames or emails exist, and it
// accepts at most one request per username per minute.
var forgotSeen = struct {
	sync.Mutex
	at map[string]time.Time
}{at: map[string]time.Time{}}

func handleForgot(store *Store, email *Email) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Username) == "" {
			http.Error(w, "username required", http.StatusBadRequest)
			return
		}
		u := strings.ToLower(strings.TrimSpace(req.Username))
		forgotSeen.Lock()
		last, seen := forgotSeen.at[u]
		if !seen || time.Since(last) > time.Minute {
			forgotSeen.at[u] = time.Now()
			seen = false
		}
		forgotSeen.Unlock()
		if !seen {
			if acct, err := store.GetUser(u); err == nil && acct != nil {
				go email.SendReset(store, email.base(r), acct)
			}
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- admin handlers ----------

func handleListUsers(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		users, err := store.ListUsers()
		if err != nil {
			log.Printf("[admin] list users error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, users)
	}
}

type userReq struct {
	Username    string  `json:"username"`
	DisplayName *string `json:"display_name"`
	Initial     *string `json:"initial"`
	Color       *string `json:"color"`
	Email       *string `json:"email"`
	ActiveSite  *string `json:"active_site"`
}

// validateProfile trims and checks whichever profile fields are
// present, returning them as an update map.
func validateProfile(store *Store, req userReq) (map[string]string, error) {
	fields := map[string]string{}
	if req.ActiveSite != nil {
		v := strings.TrimSpace(*req.ActiveSite)
		if v != "" {
			ok, err := store.WebsiteExists(v)
			if err != nil {
				return nil, err
			}
			if !ok {
				return nil, fmt.Errorf("no such website")
			}
		}
		fields["active_site"] = v
	}
	if req.DisplayName != nil {
		v := strings.TrimSpace(*req.DisplayName)
		if v == "" {
			return nil, fmt.Errorf("display_name required")
		}
		fields["display_name"] = v
	}
	if req.Initial != nil {
		v := strings.TrimSpace(*req.Initial)
		if err := ValidateInitial(v); err != nil {
			return nil, err
		}
		fields["initial"] = v
	}
	if req.Color != nil {
		v := strings.ToLower(strings.TrimSpace(*req.Color))
		if err := ValidateColor(v); err != nil {
			return nil, err
		}
		fields["color"] = v
	}
	if req.Email != nil {
		v := strings.TrimSpace(*req.Email)
		if v != "" {
			if err := ValidateEmail(v); err != nil {
				return nil, err
			}
		}
		fields["email"] = v
	}
	return fields, nil
}

func handleCreateUser(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req userReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := ValidateUsername(req.Username); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.DisplayName == nil || strings.TrimSpace(*req.DisplayName) == "" {
			http.Error(w, "display_name required", http.StatusBadRequest)
			return
		}
		fields, err := validateProfile(store, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		acct := Account{
			Username:    req.Username,
			DisplayName: fields["display_name"],
			Initial:     fields["initial"],
			Color:       fields["color"],
			Email:       fields["email"],
			ActiveSite:  fields["active_site"],
		}
		// Defaults the console normally supplies but scripts may omit.
		if acct.Initial == "" {
			acct.Initial = DefaultInitial(acct.DisplayName)
		}
		if acct.Color == "" {
			acct.Color = RandomColor()
		}
		if err := store.CreateUser(acct); err != nil {
			http.Error(w, "could not create user (already exists?)", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{
			"username": acct.Username, "initial": acct.Initial, "color": acct.Color,
		})
	}
}

func handlePatchUser(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		var req userReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		fields, err := validateProfile(store, req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(fields) == 0 {
			http.Error(w, "nothing to update", http.StatusBadRequest)
			return
		}
		found, err := store.UpdateUser(username, fields)
		if err != nil {
			log.Printf("[admin] patch user error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleDeleteUser(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		// Roles, sessions, and tokens cascade — deleting yourself would
		// end your own session mid-request, so refuse it.
		if self, _ := sessionUser(store, r); self == username {
			http.Error(w, "cannot delete your own account", http.StatusBadRequest)
			return
		}
		found, err := store.DeleteUser(username)
		if err != nil {
			log.Printf("[admin] delete user error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !found {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleListWebsites(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sites, err := store.ListWebsites()
		if err != nil {
			log.Printf("[admin] list websites error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, sites)
	}
}

type roleReq struct {
	Username string `json:"username"`
	Website  string `json:"website"`
	Role     string `json:"role"`
}

func handleAddRole(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req roleReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Username == "" || req.Website == "" || req.Role == "" {
			http.Error(w, "username, website, role required", http.StatusBadRequest)
			return
		}
		if err := store.AddRole(req.Username, req.Website, req.Role); err != nil {
			// FK violations arrive here: unknown user, or a role that
			// isn't registered for that website.
			http.Error(w, "invalid user/website/role combination", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleRemoveRole(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req roleReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := store.RemoveRole(req.Username, req.Website, req.Role); err != nil {
			log.Printf("[admin] remove role error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// handleCreateLink mints an invite or reset link.
//
//	{"username": "abi", "type": "invite", "website": "hxh"}  → <base>/hxh/_invite/?code=...  (7d)
//	{"username": "abi", "type": "reset",  "website": "hxh"}  → <base>/hxh/_reset/?code=...   (1h)
//
// website defaults to the user's active site. A site without its own
// /_invite/ or /_reset/ page gets the default page under /admin/. The
// code appears only in the response — the DB keeps just its hash.
func handleCreateLink(store *Store, email *Email) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Type     string `json:"type"`
			Website  string `json:"website"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Type != KindInvite && req.Type != KindReset {
			http.Error(w, `type must be "invite" or "reset"`, http.StatusBadRequest)
			return
		}
		acct, err := store.GetUser(req.Username)
		if err != nil {
			log.Printf("[admin] link user lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if acct == nil {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}
		website := req.Website
		if website == "" {
			website = acct.ActiveSite
		}
		if website == "" {
			if req.Type == KindInvite {
				http.Error(w, "user has no active site", http.StatusBadRequest)
				return
			}
			website = "admin"
		}
		exists, err := store.WebsiteExists(website)
		if err != nil {
			log.Printf("[admin] website lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "no such website", http.StatusBadRequest)
			return
		}
		ttl := InviteTTL
		if req.Type == KindReset {
			ttl = ResetTTL
		}
		code, expiresAt, err := store.CreateToken(req.Username, website, req.Type, ttl)
		if err != nil {
			log.Printf("[admin] create token error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		base := email.base(r)
		writeJSON(w, http.StatusOK, map[string]any{
			"url":        fmt.Sprintf("%s%s?code=%s", base, email.PagePath(r.Context(), base, website, req.Type), code),
			"expires_at": expiresAt,
		})
	}
}

// handleEmailStatus tells the console whether sending is possible.
func handleEmailStatus(email *Email) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"configured": email.Configured(),
			"from":       email.Mailer.From,
		})
	}
}

// handleSendEmail renders a website's _email/ template for a user and
// sends it. {"username","website","template"}; website defaults to the
// user's active site. Invite/reset templates mint a fresh link,
// returned alongside so the admin can also copy it.
func handleSendEmail(store *Store, email *Email) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Website  string `json:"website"`
			Template string `json:"template"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Username == "" || req.Template == "" {
			http.Error(w, "username, template required", http.StatusBadRequest)
			return
		}
		if !email.Configured() {
			http.Error(w, "email not configured on the server", http.StatusServiceUnavailable)
			return
		}
		if req.Website == "" {
			if acct, err := store.GetUser(req.Username); err == nil && acct != nil {
				req.Website = acct.ActiveSite
			}
		}
		if req.Website == "" {
			http.Error(w, "user has no active site", http.StatusBadRequest)
			return
		}
		exists, err := store.WebsiteExists(req.Website)
		if err != nil {
			log.Printf("[admin] website lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !exists {
			http.Error(w, "no such website", http.StatusBadRequest)
			return
		}
		acct, err := store.GetUser(req.Username)
		if err != nil {
			log.Printf("[admin] email user lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if acct == nil {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}
		res, err := email.Send(r.Context(), store, email.base(r), req.Website, req.Template, acct)
		switch {
		case err == nil:
		case errors.Is(err, ErrNoEmail):
			http.Error(w, "user has no email address", http.StatusBadRequest)
			return
		case errors.Is(err, ErrNoTemplate):
			http.Error(w, "no such template for that website", http.StatusNotFound)
			return
		case errors.Is(err, mailer.ErrNotConfigured):
			http.Error(w, "email not configured on the server", http.StatusServiceUnavailable)
			return
		default:
			log.Printf("[admin] send email (%s/%s) to %s failed: %v", req.Website, req.Template, req.Username, err)
			http.Error(w, "sending failed: "+err.Error(), http.StatusBadGateway)
			return
		}
		log.Printf("[admin] sent %s/%s email to %s", req.Website, req.Template, req.Username)
		writeJSON(w, http.StatusOK, res)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
