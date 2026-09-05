// Package hxh implements hxh-project endpoints (the party site's data
// layer — currently the admin-curated character roster).
//
// Auth is NOT the per-project floor: hxh uses the SHARED auth system
// (internal/shared). Every endpoint here resolves the hobby_session
// cookie against the shared store and requires role "admin" on
// website "hxh".
//
// Wire format uses arrays (nen_types, weapons, arcs, images); the DB
// stores comma-separated slug strings (per Andrew's simplicity
// preference) except images, which is JSONB.
package hxh

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slackwing/hobby-server/internal/shared"
)

type Arc struct {
	Slug      string `json:"slug"`
	Name      string `json:"name"`
	SortOrder int    `json:"sort_order"`
}

type Character struct {
	Slug        string   `json:"slug"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	NenTypes    []string `json:"nen_types"`
	Weapons     []string `json:"weapons"`
	Arcs        []string `json:"arcs"`
	Images      []string `json:"images"`
	SortOrder   int      `json:"sort_order"`
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

func joinSlugs(s []string) string { return strings.Join(s, ",") }

func splitSlugs(s string) []string {
	if s == "" {
		return []string{}
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Store) ListArcs() ([]Arc, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT slug, name, sort_order FROM hxh_arcs ORDER BY sort_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	arcs := []Arc{}
	for rows.Next() {
		var a Arc
		if err := rows.Scan(&a.Slug, &a.Name, &a.SortOrder); err != nil {
			return nil, err
		}
		arcs = append(arcs, a)
	}
	return arcs, rows.Err()
}

func (s *Store) ListCharacters() ([]Character, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT slug, name, description, nen_types, weapons, arcs, images, sort_order
		FROM hxh_characters ORDER BY sort_order, slug
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	chars := []Character{}
	for rows.Next() {
		var c Character
		var nen, weapons, arcs string
		var images []byte
		if err := rows.Scan(&c.Slug, &c.Name, &c.Description, &nen, &weapons, &arcs, &images, &c.SortOrder); err != nil {
			return nil, err
		}
		c.NenTypes = splitSlugs(nen)
		c.Weapons = splitSlugs(weapons)
		c.Arcs = splitSlugs(arcs)
		c.Images = []string{}
		if err := json.Unmarshal(images, &c.Images); err != nil {
			return nil, err
		}
		chars = append(chars, c)
	}
	return chars, rows.Err()
}

// UpsertCharacters writes the given characters by slug. With replace,
// rows whose slug is absent from the payload are deleted first.
func (s *Store) UpsertCharacters(chars []Character, replace bool) error {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if replace {
		slugs := make([]string, len(chars))
		for i, c := range chars {
			slugs[i] = c.Slug
		}
		if _, err := tx.Exec(ctx, `DELETE FROM hxh_characters WHERE NOT (slug = ANY($1))`, slugs); err != nil {
			return err
		}
	}
	for _, c := range chars {
		images, err := json.Marshal(c.Images)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO hxh_characters (slug, name, description, nen_types, weapons, arcs, images, sort_order, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NOW())
			ON CONFLICT (slug) DO UPDATE SET
				name = EXCLUDED.name,
				description = EXCLUDED.description,
				nen_types = EXCLUDED.nen_types,
				weapons = EXCLUDED.weapons,
				arcs = EXCLUDED.arcs,
				images = EXCLUDED.images,
				sort_order = EXCLUDED.sort_order,
				updated_at = NOW()
		`, c.Slug, c.Name, c.Description, joinSlugs(c.NenTypes), joinSlugs(c.Weapons),
			joinSlugs(c.Arcs), images, c.SortOrder); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// Mount wires the hxh endpoints. All routes require role "admin" on
// website "hxh" in the shared auth system.
func Mount(r chi.Router, store *Store, auth *shared.Store) {
	r.Group(func(g chi.Router) {
		g.Use(requireHxhAdmin(auth))
		g.Get("/roster", handleRoster(store))
		g.Put("/roster/characters", handlePutCharacters(store))
	})
}

func requireHxhAdmin(auth *shared.Store) func(http.Handler) http.Handler {
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
			isAdmin, err := auth.HasRole(username, "hxh", "admin")
			if err != nil {
				log.Printf("[hxh] role check error: %v", err)
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

func handleRoster(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		arcs, err := store.ListArcs()
		if err != nil {
			log.Printf("[hxh] list arcs error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		chars, err := store.ListCharacters()
		if err != nil {
			log.Printf("[hxh] list characters error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"arcs": arcs, "characters": chars})
	}
}

func handlePutCharacters(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var chars []Character
		if err := json.NewDecoder(r.Body).Decode(&chars); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		for _, c := range chars {
			if c.Slug == "" || c.Name == "" {
				http.Error(w, "every character needs slug and name", http.StatusBadRequest)
				return
			}
		}
		replace := r.URL.Query().Get("replace") == "1"
		if err := store.UpsertCharacters(chars, replace); err != nil {
			if errors.Is(err, pgx.ErrTxClosed) {
				log.Printf("[hxh] upsert tx closed: %v", err)
			}
			log.Printf("[hxh] upsert error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"count": len(chars), "replace": replace})
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
