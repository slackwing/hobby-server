package shared

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
)

// The console scoped to ONE website (Andrew, 2026-10-01): /<site>/_admin
// runs the very same console as /admin/, against /admin/api/site/{site}/…,
// open to that site's admins (role "admin" on it) as well as global
// admins (admin/admin).
//
// The boundary that matters: a site admin may only change people WHOLLY
// within the site — every role they hold is on it (or, freshly created
// from it, none yet), not a bot, not the anonymous account. Anyone else —
// a global admin, someone who is also on another site — is read-only to
// them: editing a person's email, or minting their reset link, is
// account takeover, and a site admin must not take over an account
// that reaches beyond the site. Global admins may act on anyone listed.
// Site admins never delete users, see bots, or set active site / bot.
//
// Each write checks the boundary, then hands a cleaned request (the
// website forced to {site}) to the global handler, so every action has
// one implementation.

type siteCtxKey int

const (
	ctxSiteCaller siteCtxKey = iota
	ctxSiteGlobal
)

func requireSiteAdmin(store *Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			username, ok := sessionUser(store, r)
			if !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			site := chi.URLParam(r, "site")
			exists, err := store.WebsiteExists(site)
			if err != nil {
				log.Printf("[admin] site check error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !exists {
				http.Error(w, "no such website", http.StatusNotFound)
				return
			}
			global, err := store.HasRole(username, "admin", "admin")
			if err == nil && !global {
				var siteAdmin bool
				siteAdmin, err = store.HasRole(username, site, "admin")
				if err == nil && !siteAdmin {
					http.Error(w, "Forbidden", http.StatusForbidden)
					return
				}
			}
			if err != nil {
				log.Printf("[admin] site role check error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			ctx := context.WithValue(context.WithValue(r.Context(), ctxSiteCaller, username), ctxSiteGlobal, global)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func siteCaller(r *http.Request) (string, bool) {
	u, _ := r.Context().Value(ctxSiteCaller).(string)
	g, _ := r.Context().Value(ctxSiteGlobal).(bool)
	return u, g
}

// whollyWithin: every role the user holds is on site (none yet counts when
// their active site is it — a user just created from this console), and
// they are neither a bot nor the anonymous account.
func whollyWithin(u User, site string) bool {
	if u.IsBot || u.Username == AnonymousUser {
		return false
	}
	if len(u.Roles) == 0 {
		return u.ActiveSite == site
	}
	for _, r := range u.Roles {
		if r.Website != site {
			return false
		}
	}
	return true
}

// siteUser looks a user up for a site write and enforces the boundary.
// It answers the HTTP error itself and returns ok=false when refused.
func siteUser(store *Store, w http.ResponseWriter, r *http.Request, username string) (User, bool) {
	site := chi.URLParam(r, "site")
	_, global := siteCaller(r)
	users, err := store.ListUsers()
	if err != nil {
		log.Printf("[admin] site user lookup error: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return User{}, false
	}
	for _, u := range users {
		if u.Username != username {
			continue
		}
		if !global && !whollyWithin(u, site) {
			http.Error(w, "that account reaches beyond "+site+": only a global admin can change it", http.StatusForbidden)
			return User{}, false
		}
		return u, true
	}
	http.Error(w, "no such user", http.StatusNotFound)
	return User{}, false
}

// delegate re-issues the request to a global handler with a new JSON body.
func delegate(w http.ResponseWriter, r *http.Request, body any, h http.HandlerFunc) {
	b, err := json.Marshal(body)
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	r2 := r.Clone(r.Context())
	r2.Body = io.NopCloser(bytes.NewReader(b))
	r2.ContentLength = int64(len(b))
	h(w, r2)
}

// SiteUser is a user as a site's console sees them: only that site's roles, and whether the caller may change them.
type SiteUser struct {
	User
	Editable bool `json:"editable"`
}

func mountSiteAdmin(r chi.Router, store *Store, email *Email) {
	r.Route("/site/{site}", func(g chi.Router) {
		g.Use(requireSiteAdmin(store))
		g.Get("/users", handleSiteUsers(store))
		g.Get("/website", handleSiteWebsite(store))
		g.Get("/email-status", handleEmailStatus(email))
		g.Post("/users", handleSiteCreateUser(store))
		g.Patch("/users/{username}", handleSitePatchUser(store))
		g.Post("/roles", handleSiteRole(store, true))
		g.Delete("/roles", handleSiteRole(store, false))
		g.Post("/links", handleSiteLink(store, email))
		g.Post("/email", handleSiteEmail(store, email))
	})
}

// handleSiteUsers: everyone with a role on the site (or created from it, no role yet) — never bots or the anonymous account.
func handleSiteUsers(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site := chi.URLParam(r, "site")
		_, global := siteCaller(r)
		users, err := store.ListUsers()
		if err != nil {
			log.Printf("[admin] site list users error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		out := []SiteUser{}
		for _, u := range users {
			if u.IsBot || u.Username == AnonymousUser {
				continue
			}
			mine := []Role{}
			for _, ro := range u.Roles {
				if ro.Website == site {
					mine = append(mine, ro)
				}
			}
			if len(mine) == 0 && !(len(u.Roles) == 0 && u.ActiveSite == site) {
				continue
			}
			editable := global || whollyWithin(u, site)
			u.Roles = mine
			out = append(out, SiteUser{User: u, Editable: editable})
		}
		writeJSON(w, http.StatusOK, out)
	}
}

func handleSiteWebsite(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		site := chi.URLParam(r, "site")
		sites, err := store.ListWebsites()
		if err != nil {
			log.Printf("[admin] site website error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		for _, s := range sites {
			if s.Website == site {
				writeJSON(w, http.StatusOK, s)
				return
			}
		}
		http.Error(w, "no such website", http.StatusNotFound)
	}
}

// handleSiteCreateUser: name, username, email (all required) — created on this site (its active site).
func handleSiteCreateUser(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username    string `json:"username"`
			DisplayName string `json:"display_name"`
			Email       string `json:"email"`
			Initial     string `json:"initial"`
			Color       string `json:"color"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Email) == "" {
			http.Error(w, "email required", http.StatusBadRequest)
			return
		}
		site := chi.URLParam(r, "site")
		body := map[string]any{"username": req.Username, "display_name": req.DisplayName, "email": req.Email, "active_site": site}
		if req.Initial != "" {
			body["initial"] = req.Initial
		}
		if req.Color != "" {
			body["color"] = req.Color
		}
		delegate(w, r, body, handleCreateUser(store))
	}
}

// handleSitePatchUser: name, avatar, email — never active site or bot status.
func handleSitePatchUser(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := chi.URLParam(r, "username")
		if _, ok := siteUser(store, w, r, username); !ok {
			return
		}
		var req struct {
			DisplayName *string `json:"display_name,omitempty"`
			Initial     *string `json:"initial,omitempty"`
			Color       *string `json:"color,omitempty"`
			Email       *string `json:"email,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		delegate(w, r, req, handlePatchUser(store))
	}
}

// handleSiteRole adds or removes one of THIS site's roles. Nobody removes their own admin role here (no locking yourself out).
func handleSiteRole(store *Store, add bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Role     string `json:"role"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if _, ok := siteUser(store, w, r, req.Username); !ok {
			return
		}
		site := chi.URLParam(r, "site")
		caller, _ := siteCaller(r)
		if !add && req.Username == caller && req.Role == "admin" {
			http.Error(w, "you can't remove your own admin role", http.StatusConflict)
			return
		}
		body := roleReq{Username: req.Username, Website: site, Role: req.Role}
		if add {
			delegate(w, r, body, handleAddRole(store))
		} else {
			delegate(w, r, body, handleRemoveRole(store))
		}
	}
}

func handleSiteLink(store *Store, email *Email) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Type     string `json:"type"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if _, ok := siteUser(store, w, r, req.Username); !ok {
			return
		}
		delegate(w, r, map[string]string{"username": req.Username, "type": req.Type, "website": chi.URLParam(r, "site")}, handleCreateLink(store, email))
	}
}

func handleSiteEmail(store *Store, email *Email) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Username string `json:"username"`
			Template string `json:"template"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if _, ok := siteUser(store, w, r, req.Username); !ok {
			return
		}
		delegate(w, r, map[string]string{"username": req.Username, "website": chi.URLParam(r, "site"), "template": req.Template}, handleSendEmail(store, email))
	}
}
