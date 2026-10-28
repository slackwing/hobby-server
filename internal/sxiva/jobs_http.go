package sxiva

// Netflix roles watcher endpoints (shared auth; the hobby_session cookie
// is SameSite=Lax, so other sites can't ride it into the writes). Reads
// are open to the global admin and to anyone holding a role on the status
// site (Andrew shares the card read-only: tampopo is a "viewer"); writes
// stay the global admin's.
//
//	GET   /jobs                    every role (live, gone, hidden), sources combined   — readers
//	GET   /jobs/roles/{role_id}    one role, with each posting's description and details — readers
//	PATCH /jobs/roles/{role_id}    {favorite?, hidden?, seen?}                          — admin
//	POST  /jobs/refresh            check the sources now                                 — admin

import (
	"encoding/json"
	"log"
	"net/http"
	"regexp"

	"github.com/go-chi/chi/v5"

	"github.com/slackwing/hobby-server/internal/shared"
)

// Jobs is what the job endpoints need; nil = not mounted.
type Jobs struct {
	Watcher *JobWatcher
	Admin   JobsAuth // *shared.Store
}

// JobsAuth is the slice of the shared auth store the endpoints use.
type JobsAuth interface {
	GetSession(token string) (string, bool)
	HasRole(username, website, role string) (bool, error)
	MayAct(username, website string) (bool, error)
}

// JobsSite is the status page's website in the shared auth system.
const JobsSite = "status"

// requireJobs lets the global admin through; with read, also anyone who
// may act on the status site (a role there, not the anonymous account).
func requireJobs(auth JobsAuth, read bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(shared.CookieName)
			if err != nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			username, ok := auth.GetSession(cookie.Value)
			if !ok {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			allowed, err := auth.HasRole(username, "admin", "admin")
			if err == nil && !allowed && read {
				allowed, err = auth.MayAct(username, JobsSite)
			}
			if err != nil {
				log.Printf("[jobs] role check error: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			if !allowed {
				http.Error(w, "Forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func mountJobs(r chi.Router, store *Store, jobs *Jobs) {
	r.Group(func(g chi.Router) {
		g.Use(requireJobs(jobs.Admin, true))
		g.Get("/jobs", handleJobs(store))
		g.Get("/jobs/roles/{role_id}", handleJobRoleDetail(store))
	})
	r.Group(func(g chi.Router) {
		g.Use(requireJobs(jobs.Admin, false))
		g.Patch("/jobs/roles/{role_id}", handleJobRole(store))
		g.Post("/jobs/refresh", func(w http.ResponseWriter, _ *http.Request) {
			jobs.Watcher.Trigger()
			w.WriteHeader(http.StatusAccepted)
		})
	})
}

func handleJobs(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		roles, checks, err := store.JobsView()
		if err != nil {
			log.Printf("[jobs] view: %v", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"roles": roles, "checks": checks})
	}
}

func handleJobRoleDetail(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roleID := chi.URLParam(r, "role_id")
		if !uuidRe.MatchString(roleID) {
			writeError(w, http.StatusBadRequest, "bad role_id")
			return
		}
		role, err := store.JobRole(roleID)
		if err != nil {
			log.Printf("[jobs] role %s: %v", roleID, err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if role == nil {
			writeError(w, http.StatusNotFound, "no such role")
			return
		}
		writeJSON(w, http.StatusOK, role)
	}
}

func handleJobRole(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		roleID := chi.URLParam(r, "role_id")
		if !uuidRe.MatchString(roleID) {
			writeError(w, http.StatusBadRequest, "bad role_id")
			return
		}
		var body struct {
			Favorite *bool `json:"favorite"`
			Hidden   *bool `json:"hidden"`
			Seen     *bool `json:"seen"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || (body.Favorite == nil && body.Hidden == nil && body.Seen == nil) {
			writeError(w, http.StatusBadRequest, "send favorite, hidden and/or seen")
			return
		}
		ok, err := store.UpdateJobRole(roleID, body.Favorite, body.Hidden, body.Seen)
		if err != nil {
			log.Printf("[jobs] update %s: %v", roleID, err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "no such role")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
