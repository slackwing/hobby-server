// Package bap implements bap-project endpoints (the bongo-cat
// table-bap site's data layer — each user's cup state).
//
// Auth is NOT the per-project floor: bap uses the SHARED auth system
// (internal/shared). Any logged-in shared-auth user may read/write
// THEIR OWN cup state; no role is required (roles exist for future
// admin features and invite skinning).
package bap

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slackwing/hobby-server/internal/shared"
)

// CupState is one user's persisted cup. CupX is a fraction of the
// table span (0 = home position, 1 = right edge) so any client zoom
// renders it consistently.
type CupState struct {
	CupX     float64 `json:"cup_x"`
	Baps     int     `json:"baps"`
	Broken   bool    `json:"broken"`
	Shatters int     `json:"shatters"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func withCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// GetState returns the user's cup state, or the zero state if the
// user has never saved one.
func (s *Store) GetState(username string) (CupState, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var st CupState
	err := s.pool.QueryRow(ctx, `
		SELECT cup_x, baps, broken, shatters FROM bap_cup_state WHERE username = $1
	`, username).Scan(&st.CupX, &st.Baps, &st.Broken, &st.Shatters)
	if err == pgx.ErrNoRows {
		return CupState{}, nil
	}
	return st, err
}

func (s *Store) PutState(username string, st CupState) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO bap_cup_state (username, cup_x, baps, broken, shatters, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (username) DO UPDATE SET
			cup_x = EXCLUDED.cup_x,
			baps = EXCLUDED.baps,
			broken = EXCLUDED.broken,
			shatters = EXCLUDED.shatters,
			updated_at = NOW()
	`, username, st.CupX, st.Baps, st.Broken, st.Shatters)
	return err
}

type ctxKey int

const userKey ctxKey = 0

// Mount wires the bap endpoints. All routes require a valid shared
// session; state is keyed to the session's user.
func Mount(r chi.Router, store *Store, auth *shared.Store) {
	r.Group(func(g chi.Router) {
		g.Use(requireLogin(auth))
		g.Get("/state", handleGetState(store))
		g.Put("/state", handlePutState(store))
	})
}

func requireLogin(auth *shared.Store) func(http.Handler) http.Handler {
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
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), userKey, username)))
		})
	}
}

func handleGetState(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := r.Context().Value(userKey).(string)
		st, err := store.GetState(username)
		if err != nil {
			log.Printf("[bap] get state error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func handlePutState(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		username := r.Context().Value(userKey).(string)
		var st CupState
		if err := json.NewDecoder(r.Body).Decode(&st); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if st.CupX < 0 || st.CupX > 1.5 || st.Baps < 0 || st.Shatters < 0 {
			http.Error(w, "state out of range", http.StatusBadRequest)
			return
		}
		if err := store.PutState(username, st); err != nil {
			log.Printf("[bap] put state error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
