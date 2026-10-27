package sxiva

// Netflix roles watcher endpoints — admin-only (shared auth; the
// hobby_session cookie is SameSite=Lax, so other sites can't ride it into
// the writes):
//
//	GET   /jobs                    every role (live, gone, hidden), sources combined
//	PATCH /jobs/roles/{role_id}    {favorite?, hidden?}
//	POST  /jobs/refresh            check the sources now

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
	Admin   *shared.Store
}

var uuidRe = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

func mountJobs(r chi.Router, store *Store, jobs *Jobs) {
	r.Group(func(g chi.Router) {
		g.Use(shared.RequireAdmin(jobs.Admin))
		g.Get("/jobs", handleJobs(store))
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
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&body); err != nil || (body.Favorite == nil && body.Hidden == nil) {
			writeError(w, http.StatusBadRequest, "send favorite and/or hidden")
			return
		}
		ok, err := store.UpdateJobRole(roleID, body.Favorite, body.Hidden)
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
