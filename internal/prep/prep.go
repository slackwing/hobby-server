// Package prep implements the rv project's "prep checklist" feature.
//
// This is project-specific (rv only) rather than a generic hobby-server
// capability. It's wired into cmd/server/main.go via an rv-only branch.
// If a second project ever needs its own state tables, that's the point
// to refactor into a plugin pattern.
//
// Endpoints (mounted under the rv project's URL prefix, e.g. /api/rv):
//
//   GET    /prep         → public; returns all items sorted by section + sort_order
//   POST   /prep         → auth; create
//   PATCH  /prep/{id}    → auth; partial update (done, text, date_label, sort_order, section)
//   DELETE /prep/{id}    → auth; delete
//
// Schema lives in liquibase/rv/changelog/002-prep-checklist.xml.
package prep

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Item struct {
	ID         int64     `json:"id"`
	Section    string    `json:"section"`
	Text       string    `json:"text"`
	DateLabel  *string   `json:"date_label"`
	Done       bool      `json:"done"`
	SortOrder  float64   `json:"sort_order"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) List(ctx context.Context) ([]Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, section, text, date_label, done, sort_order, created_at, updated_at
		FROM prep_item
		ORDER BY section, sort_order, id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Item, 0)
	for rows.Next() {
		var it Item
		if err := rows.Scan(&it.ID, &it.Section, &it.Text, &it.DateLabel, &it.Done, &it.SortOrder, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

type createReq struct {
	Section   string  `json:"section"`
	Text      string  `json:"text"`
	DateLabel *string `json:"date_label"`
	SortOrder float64 `json:"sort_order"`
}

func (s *Store) Create(ctx context.Context, req createReq) (Item, error) {
	var it Item
	err := s.pool.QueryRow(ctx, `
		INSERT INTO prep_item (section, text, date_label, sort_order)
		VALUES ($1, $2, $3, $4)
		RETURNING id, section, text, date_label, done, sort_order, created_at, updated_at
	`, req.Section, req.Text, req.DateLabel, req.SortOrder).
		Scan(&it.ID, &it.Section, &it.Text, &it.DateLabel, &it.Done, &it.SortOrder, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}

// patchReq uses pointers so we can distinguish "field absent" from
// "field explicitly set to zero value". Only non-nil fields get updated.
type patchReq struct {
	Done      *bool    `json:"done"`
	Text      *string  `json:"text"`
	DateLabel *string  `json:"date_label"`
	SortOrder *float64 `json:"sort_order"`
	Section   *string  `json:"section"`
	// ClearDateLabel: explicit way to set date_label to NULL. JSON `null`
	// in DateLabel would also work but is harder to detect without a
	// custom unmarshaler. Set this true with DateLabel nil/absent to clear.
	ClearDateLabel bool `json:"clear_date_label"`
}

func (s *Store) Patch(ctx context.Context, id int64, req patchReq) (Item, error) {
	// Build dynamic SET clause. Postgres lets us COALESCE for non-null
	// updates but mixed null/non-null gets ugly; just build the SET list.
	sets := []string{"updated_at = NOW()"}
	args := []any{id}
	addArg := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if req.Done != nil {
		addArg("done", *req.Done)
	}
	if req.Text != nil {
		addArg("text", *req.Text)
	}
	if req.SortOrder != nil {
		addArg("sort_order", *req.SortOrder)
	}
	if req.Section != nil {
		addArg("section", *req.Section)
	}
	if req.ClearDateLabel {
		sets = append(sets, "date_label = NULL")
	} else if req.DateLabel != nil {
		addArg("date_label", *req.DateLabel)
	}

	q := "UPDATE prep_item SET " + joinSets(sets) + " WHERE id = $1 RETURNING id, section, text, date_label, done, sort_order, created_at, updated_at"
	var it Item
	err := s.pool.QueryRow(ctx, q, args...).
		Scan(&it.ID, &it.Section, &it.Text, &it.DateLabel, &it.Done, &it.SortOrder, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}

func joinSets(sets []string) string {
	out := ""
	for i, s := range sets {
		if i > 0 {
			out += ", "
		}
		out += s
	}
	return out
}

func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM prep_item WHERE id = $1`, id)
	return err
}

// ---------- HTTP handlers ----------

// HandleList is public (no auth middleware).
func HandleList(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		items, err := store.List(r.Context())
		if err != nil {
			log.Printf("prep list error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": items})
	}
}

func HandleCreate(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Section == "" || req.Text == "" {
			http.Error(w, "section and text required", http.StatusBadRequest)
			return
		}
		it, err := store.Create(r.Context(), req)
		if err != nil {
			log.Printf("prep create error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, it)
	}
}

func HandlePatch(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		var req patchReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		it, err := store.Patch(r.Context(), id, req)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("prep patch error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, it)
	}
}

func HandleDelete(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		idStr := chi.URLParam(r, "id")
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if err := store.Delete(r.Context(), id); err != nil {
			log.Printf("prep delete error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
