package bots

import (
	"encoding/json"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/slackwing/hobby-server/internal/shared"
)

// Mount adds the console's bot endpoints (caller wraps with RequireAdmin):
//
//	GET /bots            → [{name, enabled, last_tick, ticks, hooks}]
//	PUT /bots/{name}     {enabled: bool}
func Mount(r chi.Router, svc *Service, store *shared.Store) {
	r.Get("/bots", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, svc.Statuses())
	})
	r.Put("/bots/{name}", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Enabled bool `json:"enabled"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		ok, err := store.SetBotProgram(chi.URLParam(r, "name"), req.Enabled)
		if err != nil {
			log.Printf("[bots] set program error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if !ok {
			http.Error(w, "no such program", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, svc.Statuses())
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
