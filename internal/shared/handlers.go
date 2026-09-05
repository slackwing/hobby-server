package shared

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
)

// CookieName is the SSO session cookie, shared by every website on the
// domain (Path=/ via the admin project's cookie_path).
const CookieName = "hobby_session"

// Mount wires the shared-auth endpoints onto the admin project's
// sub-router. cookiePath comes from config (should be "/"); secure
// mirrors the server env.
func Mount(r chi.Router, store *Store, cookiePath string, secure bool) {
	// Public.
	r.Post("/login", handleLogin(store, cookiePath, secure))
	r.Post("/logout", handleLogout(store, cookiePath, secure))
	r.Get("/me", handleMe(store))
	r.Get("/token-info", handleTokenInfo(store))
	r.Post("/set-password", handleSetPassword(store, cookiePath, secure))

	// Admin-only: requires role "admin" on website "admin".
	r.Group(func(g chi.Router) {
		g.Use(requireAdmin(store))
		g.Get("/users", handleListUsers(store))
		g.Post("/users", handleCreateUser(store))
		g.Patch("/users/{username}", handlePatchUser(store))
		g.Get("/websites", handleListWebsites(store))
		g.Post("/roles", handleAddRole(store))
		g.Delete("/roles", handleRemoveRole(store))
		g.Post("/links", handleCreateLink(store))
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
	displayName, _, ok, err := store.GetUser(username)
	if err != nil || !ok {
		return nil, fmt.Errorf("user %q lookup failed: %w", username, err)
	}
	roles, err := store.UserRoles(username)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"username":     username,
		"display_name": displayName,
		"roles":        roles,
	}, nil
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
		_, hash, ok, err := store.GetUser(req.Username)
		if err != nil {
			log.Printf("[admin] login lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Unknown user and unset password both take the dummy-verify
		// path so timing doesn't leak which usernames exist.
		if !ok || hash == nil {
			VerifyDummy(req.Password)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if !VerifyPassword(req.Password, *hash) {
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
		username, website, expiresAt, ok, err := store.LookupToken(code)
		if err != nil {
			log.Printf("[admin] token lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "invalid or expired link", http.StatusNotFound)
			return
		}
		displayName, _, _, err := store.GetUser(username)
		if err != nil {
			log.Printf("[admin] token user lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"username":     username,
			"display_name": displayName,
			"website":      website,
			"expires_at":   expiresAt,
		})
	}
}

func handleSetPassword(store *Store, cookiePath string, secure bool) http.HandlerFunc {
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
		username, ok, err := store.ConsumeToken(req.Code, hash)
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
		writeJSON(w, http.StatusOK, body)
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

func handleCreateUser(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if err := ValidateUsername(req.Username); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if req.DisplayName == "" {
			http.Error(w, "display_name required", http.StatusBadRequest)
			return
		}
		if err := store.CreateUser(req.Username, req.DisplayName); err != nil {
			http.Error(w, "could not create user (already exists?)", http.StatusConflict)
			return
		}
		writeJSON(w, http.StatusCreated, map[string]string{"username": req.Username})
	}
}

func handlePatchUser(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		var req struct {
			DisplayName string `json:"display_name"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.DisplayName == "" {
			http.Error(w, "display_name required", http.StatusBadRequest)
			return
		}
		found, err := store.UpdateDisplayName(username, req.DisplayName)
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
//	{"username": "abi", "type": "reset"}                      → <base>/admin/reset.html?code=...   (1h)
//	{"username": "abi", "type": "invite", "website": "hxh"}   → <base>/hxh/invite.html?code=...    (7d)
//
// The base URL is derived from the request (X-Forwarded-Proto + Host,
// which Apache sets). The code appears only in the response — the DB
// keeps just its hash.
func handleCreateLink(store *Store) http.HandlerFunc {
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
		_, _, userExists, err := store.GetUser(req.Username)
		if err != nil {
			log.Printf("[admin] link user lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !userExists {
			http.Error(w, "no such user", http.StatusNotFound)
			return
		}

		var website, path string
		var ttl time.Duration
		switch req.Type {
		case "reset":
			website, path, ttl = "admin", "/admin/reset.html", ResetTTL
		case "invite":
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
			website, path, ttl = req.Website, "/"+req.Website+"/invite.html", InviteTTL
		default:
			http.Error(w, `type must be "reset" or "invite"`, http.StatusBadRequest)
			return
		}

		code, expiresAt, err := store.CreateToken(req.Username, website, ttl)
		if err != nil {
			log.Printf("[admin] create token error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		proto := r.Header.Get("X-Forwarded-Proto")
		if proto == "" {
			proto = "http"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"url":        fmt.Sprintf("%s://%s%s?code=%s", proto, r.Host, path, code),
			"expires_at": expiresAt,
		})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
