// Package rvedit implements the rv project's editable overlay over the
// static catalog (assets/map-sources.json -> map.json) and itinerary
// (assets/itinerary.json).
//
// Two tables:
//
//   location_user     — unified override + new-location storage
//   itinerary_override — per-day itinerary overrides + inserted days
//
// See liquibase/rv/changelog/004-itinerary-and-locations.xml for the
// schema and rationale. The frontend layers these over the static JSON
// it loads from the static site; the server is dumb storage + CRUD.
//
// Endpoints (mounted under the rv project's URL prefix, e.g. /api/rv):
//
//   GET    /locations               → public; returns all location_user rows
//   POST   /locations               → auth; create (override or new)
//   PATCH  /locations/{id}          → auth; partial update
//   DELETE /locations/{id}          → auth; soft delete (sets deleted_at)
//
//   GET    /itinerary               → public; returns all itinerary_override rows
//   POST   /itinerary               → auth; create (inserted day)
//   PATCH  /itinerary/{id}          → auth; partial update
//   DELETE /itinerary/{id}          → auth; hard delete (intentional — confirmed in design)
package rvedit

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slackwing/hobby-server/internal/auth"
)

// ============================================================
// location_user
// ============================================================

type Location struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"` // "override" | "new"
	Name          *string    `json:"name"`
	Lat           *float64   `json:"lat"`
	Lon           *float64   `json:"lon"`
	RouteFraction *float64   `json:"route_fraction"`
	Emoji         *string    `json:"emoji"`
	Markdown      *string    `json:"markdown"`
	SleepType     *string    `json:"sleep_type"`
	Activated     bool       `json:"activated"`
	// Shorter / friendlier name used ONLY by itinerary day-card headers
	// and the left-side timeline. Falls through to Name when null/empty.
	// The route map continues to use Name.
	DayCardLabel  *string    `json:"day_card_label"`
	DeletedAt     *time.Time `json:"deleted_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

const locColumns = "id, kind, name, lat, lon, route_fraction, emoji, markdown, sleep_type, activated, day_card_label, deleted_at, created_at, updated_at"

func (s *Store) ListLocations(ctx context.Context) ([]Location, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+locColumns+` FROM location_user ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Location, 0)
	for rows.Next() {
		var l Location
		if err := rows.Scan(&l.ID, &l.Kind, &l.Name, &l.Lat, &l.Lon, &l.RouteFraction, &l.Emoji, &l.Markdown, &l.SleepType, &l.Activated, &l.DayCardLabel, &l.DeletedAt, &l.CreatedAt, &l.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

type createLocReq struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"` // "override" | "new"
	Name          *string  `json:"name"`
	Lat           *float64 `json:"lat"`
	Lon           *float64 `json:"lon"`
	RouteFraction *float64 `json:"route_fraction"`
	Emoji         *string  `json:"emoji"`
	Markdown      *string  `json:"markdown"`
	SleepType     *string  `json:"sleep_type"`
	Activated     *bool    `json:"activated"`
	DayCardLabel  *string  `json:"day_card_label"`
}

func (s *Store) CreateLocation(ctx context.Context, req createLocReq) (Location, error) {
	activated := true
	if req.Activated != nil {
		activated = *req.Activated
	}
	var l Location
	err := s.pool.QueryRow(ctx, `
		INSERT INTO location_user (id, kind, name, lat, lon, route_fraction, emoji, markdown, sleep_type, activated, day_card_label)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING `+locColumns,
		req.ID, req.Kind, req.Name, req.Lat, req.Lon, req.RouteFraction, req.Emoji, req.Markdown, req.SleepType, activated, req.DayCardLabel,
	).Scan(&l.ID, &l.Kind, &l.Name, &l.Lat, &l.Lon, &l.RouteFraction, &l.Emoji, &l.Markdown, &l.SleepType, &l.Activated, &l.DayCardLabel, &l.DeletedAt, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

type patchLocReq struct {
	Name          *string  `json:"name"`
	Lat           *float64 `json:"lat"`
	Lon           *float64 `json:"lon"`
	RouteFraction *float64 `json:"route_fraction"`
	Emoji         *string  `json:"emoji"`
	Markdown      *string  `json:"markdown"`
	SleepType     *string  `json:"sleep_type"`
	Activated     *bool    `json:"activated"`
	DayCardLabel  *string  `json:"day_card_label"`
	// Pass true to clear day_card_label (set NULL). JSON null in the
	// field is harder to detect without a custom unmarshaler.
	ClearDayCardLabel bool `json:"clear_day_card_label"`
	// Pass true to restore a soft-deleted row.
	Restore bool `json:"restore"`
}

func (s *Store) PatchLocation(ctx context.Context, id string, req patchLocReq) (Location, error) {
	sets := []string{"updated_at = NOW()"}
	args := []any{id}
	addArg := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if req.Name != nil {
		addArg("name", *req.Name)
	}
	if req.Lat != nil {
		addArg("lat", *req.Lat)
	}
	if req.Lon != nil {
		addArg("lon", *req.Lon)
	}
	if req.RouteFraction != nil {
		addArg("route_fraction", *req.RouteFraction)
	}
	if req.Emoji != nil {
		addArg("emoji", *req.Emoji)
	}
	if req.Markdown != nil {
		addArg("markdown", *req.Markdown)
	}
	if req.SleepType != nil {
		addArg("sleep_type", *req.SleepType)
	}
	if req.Activated != nil {
		addArg("activated", *req.Activated)
	}
	if req.ClearDayCardLabel {
		sets = append(sets, "day_card_label = NULL")
	} else if req.DayCardLabel != nil {
		addArg("day_card_label", *req.DayCardLabel)
	}
	if req.Restore {
		sets = append(sets, "deleted_at = NULL")
	}

	q := "UPDATE location_user SET " + joinSets(sets) + " WHERE id = $1 RETURNING " + locColumns
	var l Location
	err := s.pool.QueryRow(ctx, q, args...).Scan(&l.ID, &l.Kind, &l.Name, &l.Lat, &l.Lon, &l.RouteFraction, &l.Emoji, &l.Markdown, &l.SleepType, &l.Activated, &l.DayCardLabel, &l.DeletedAt, &l.CreatedAt, &l.UpdatedAt)
	return l, err
}

// SoftDeleteLocation marks the row as deleted (sets deleted_at = NOW()).
// The frontend hides any location whose effective record has
// deleted_at set — both catalog overrides and brand-new user
// locations. Restore via PATCH with restore:true. To reset an
// override (fall back to catalog values), use the ?hard=true DELETE
// variant, which removes the row entirely.
func (s *Store) SoftDeleteLocation(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE location_user SET deleted_at = NOW(), updated_at = NOW() WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// HardDeleteLocation removes the row entirely. Used as "reset" for
// catalog overrides — once removed, the catalog values are canonical
// again with no override layer. For "new" user locations this just
// deletes them permanently (use SoftDelete + restore if you want
// recoverable deletion).
func (s *Store) HardDeleteLocation(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM location_user WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ============================================================
// itinerary_override
// ============================================================

type ItineraryEntry struct {
	DayID          string    `json:"day_id"`
	SleepLocID     *string   `json:"sleep_loc_id"`
	Markdown       *string   `json:"markdown"`
	Excursion      *string   `json:"excursion"`
	ExcursionByCar bool      `json:"excursion_by_car"`
	Ordinal        float64   `json:"ordinal"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

const itinColumns = "day_id, sleep_loc_id, markdown, excursion, excursion_by_car, ordinal, created_at, updated_at"

func (s *Store) ListItinerary(ctx context.Context) ([]ItineraryEntry, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+itinColumns+` FROM itinerary_override ORDER BY ordinal, day_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]ItineraryEntry, 0)
	for rows.Next() {
		var it ItineraryEntry
		if err := rows.Scan(&it.DayID, &it.SleepLocID, &it.Markdown, &it.Excursion, &it.ExcursionByCar, &it.Ordinal, &it.CreatedAt, &it.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

type createItinReq struct {
	DayID          string  `json:"day_id"`
	SleepLocID     *string `json:"sleep_loc_id"`
	Markdown       *string `json:"markdown"`
	Excursion      *string `json:"excursion"`
	ExcursionByCar bool    `json:"excursion_by_car"`
	Ordinal        float64 `json:"ordinal"`
}

// UpsertItinerary upserts by day_id. The first edit of a static day
// (e.g., changing Day 5's sleep) creates an override row keyed by
// "static_5"; subsequent edits update that row. New inserted days are
// created with a UUID day_id.
func (s *Store) UpsertItinerary(ctx context.Context, req createItinReq) (ItineraryEntry, error) {
	var it ItineraryEntry
	err := s.pool.QueryRow(ctx, `
		INSERT INTO itinerary_override (day_id, sleep_loc_id, markdown, excursion, excursion_by_car, ordinal)
		VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT (day_id) DO UPDATE SET
		  sleep_loc_id    = EXCLUDED.sleep_loc_id,
		  markdown        = EXCLUDED.markdown,
		  excursion       = EXCLUDED.excursion,
		  excursion_by_car = EXCLUDED.excursion_by_car,
		  ordinal         = EXCLUDED.ordinal,
		  updated_at      = NOW()
		RETURNING `+itinColumns,
		req.DayID, req.SleepLocID, req.Markdown, req.Excursion, req.ExcursionByCar, req.Ordinal,
	).Scan(&it.DayID, &it.SleepLocID, &it.Markdown, &it.Excursion, &it.ExcursionByCar, &it.Ordinal, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}

type patchItinReq struct {
	SleepLocID     *string  `json:"sleep_loc_id"`
	Markdown       *string  `json:"markdown"`
	Excursion      *string  `json:"excursion"`
	ExcursionByCar *bool    `json:"excursion_by_car"`
	Ordinal        *float64 `json:"ordinal"`
	ClearOverride  bool     `json:"clear_override"` // set to true with a field also nil to explicitly NULL it
}

func (s *Store) PatchItinerary(ctx context.Context, dayID string, req patchItinReq) (ItineraryEntry, error) {
	sets := []string{"updated_at = NOW()"}
	args := []any{dayID}
	addArg := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if req.SleepLocID != nil {
		addArg("sleep_loc_id", *req.SleepLocID)
	}
	if req.Markdown != nil {
		addArg("markdown", *req.Markdown)
	}
	if req.Excursion != nil {
		// Convention: passing empty-string excursion clears the field.
		if *req.Excursion == "" {
			addArg("excursion", nil)
		} else {
			addArg("excursion", *req.Excursion)
		}
	}
	if req.ExcursionByCar != nil {
		addArg("excursion_by_car", *req.ExcursionByCar)
	}
	if req.Ordinal != nil {
		addArg("ordinal", *req.Ordinal)
	}
	q := "UPDATE itinerary_override SET " + joinSets(sets) + " WHERE day_id = $1 RETURNING " + itinColumns
	var it ItineraryEntry
	err := s.pool.QueryRow(ctx, q, args...).Scan(&it.DayID, &it.SleepLocID, &it.Markdown, &it.Excursion, &it.ExcursionByCar, &it.Ordinal, &it.CreatedAt, &it.UpdatedAt)
	return it, err
}

// DeleteItinerary hard-deletes the row. By design — confirmed at design
// time that itinerary days are gone-is-gone. Markdown is precious only
// while the row exists; UI must double-confirm before calling this.
func (s *Store) DeleteItinerary(ctx context.Context, dayID string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM itinerary_override WHERE day_id = $1`, dayID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// ============================================================
// helpers
// ============================================================

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

// ============================================================
// HTTP handlers
// ============================================================

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// ---------- locations ----------

func HandleListLocations(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		locs, err := store.ListLocations(r.Context())
		if err != nil {
			log.Printf("rvedit list locations: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"locations": locs})
	}
}

func HandleCreateLocation(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createLocReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.ID == "" || req.Kind == "" {
			http.Error(w, "id and kind required", http.StatusBadRequest)
			return
		}
		if req.Kind != "override" && req.Kind != "new" {
			http.Error(w, "kind must be 'override' or 'new'", http.StatusBadRequest)
			return
		}
		// For 'new' rows require name + coords (lat/lon OR route_fraction).
		if req.Kind == "new" {
			if req.Name == nil || *req.Name == "" {
				http.Error(w, "name required for new location", http.StatusBadRequest)
				return
			}
			if (req.Lat == nil || req.Lon == nil) && req.RouteFraction == nil {
				http.Error(w, "lat/lon or route_fraction required for new location", http.StatusBadRequest)
				return
			}
		}
		loc, err := store.CreateLocation(r.Context(), req)
		if err != nil {
			log.Printf("rvedit create location: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, loc)
	}
}

func HandlePatchLocation(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		var req patchLocReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		loc, err := store.PatchLocation(r.Context(), id, req)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit patch location: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, loc)
	}
}

func HandleDeleteLocation(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		// ?hard=true → permanently remove the row (used as "reset" for
		// a catalog override; the location returns to its catalog
		// values). Default is soft-delete.
		hard := r.URL.Query().Get("hard") == "true"
		var err error
		if hard {
			err = store.HardDeleteLocation(r.Context(), id)
		} else {
			err = store.SoftDeleteLocation(r.Context(), id)
		}
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit delete location (hard=%v): %v", hard, err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ---------- itinerary ----------

func HandleListItinerary(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, err := store.ListItinerary(r.Context())
		if err != nil {
			log.Printf("rvedit list itinerary: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"days": days})
	}
}

func HandleUpsertItinerary(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createItinReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.DayID == "" {
			http.Error(w, "day_id required", http.StatusBadRequest)
			return
		}
		it, err := store.UpsertItinerary(r.Context(), req)
		if err != nil {
			log.Printf("rvedit upsert itinerary: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, it)
	}
}

func HandlePatchItinerary(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		var req patchItinReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		it, err := store.PatchItinerary(r.Context(), id, req)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit patch itinerary: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, it)
	}
}

func HandleDeleteItinerary(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		err := store.DeleteItinerary(r.Context(), id)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit delete itinerary: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ============================================================
// note (single-row scratchpad)
// ============================================================
//
// Routes:
//   GET /note          → public; returns the "main" note row
//   PUT /note          → auth;   replaces markdown for "main"
//
// Schema seeds a row with id="main" so the PUT path is always an
// UPDATE, never an upsert. We don't expose a list/create surface —
// the project only needs one note.

type Note struct {
	ID        string    `json:"id"`
	Markdown  string    `json:"markdown"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (s *Store) GetNote(ctx context.Context, id string) (Note, error) {
	var n Note
	err := s.pool.QueryRow(ctx,
		`SELECT id, markdown, created_at, updated_at FROM note WHERE id = $1`, id,
	).Scan(&n.ID, &n.Markdown, &n.CreatedAt, &n.UpdatedAt)
	return n, err
}

type putNoteReq struct {
	Markdown string `json:"markdown"`
}

func (s *Store) PutNote(ctx context.Context, id, markdown string) (Note, error) {
	var n Note
	err := s.pool.QueryRow(ctx, `
		UPDATE note SET markdown = $2, updated_at = NOW()
		WHERE id = $1
		RETURNING id, markdown, created_at, updated_at`,
		id, markdown,
	).Scan(&n.ID, &n.Markdown, &n.CreatedAt, &n.UpdatedAt)
	return n, err
}

func HandleGetNote(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		n, err := store.GetNote(r.Context(), "main")
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit get note: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, n)
	}
}

func HandlePutNote(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req putNoteReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		n, err := store.PutNote(r.Context(), "main", req.Markdown)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit put note: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, n)
	}
}

// ============================================================
// checkin (live-location breadcrumbs)
// ============================================================
//
// Routes:
//   GET  /checkins                → public; the most recent check-in
//                                   as {latest: {...}} (or {latest: null})
//   GET  /checkins?history=true   → public; up to 200 recent check-ins
//                                   as {history: [...]}
//   POST /checkins                → auth;  create one
//
// The map renders `latest` as a pulsing blue dot. History is there
// for future "breadcrumb trail" UI.

type Checkin struct {
	ID         int64     `json:"id"`
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	AccuracyM  *float64  `json:"accuracy_m"`
	Note       *string   `json:"note"`
	UserID     *string   `json:"user_id"`
	CreatedAt  time.Time `json:"created_at"`
}

const checkinColumns = "id, lat, lon, accuracy_m, note, user_id, created_at"

func (s *Store) LatestCheckin(ctx context.Context) (*Checkin, error) {
	var c Checkin
	err := s.pool.QueryRow(ctx,
		`SELECT `+checkinColumns+` FROM checkin ORDER BY created_at DESC LIMIT 1`,
	).Scan(&c.ID, &c.Lat, &c.Lon, &c.AccuracyM, &c.Note, &c.UserID, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (s *Store) CheckinHistory(ctx context.Context, limit int) ([]Checkin, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.pool.Query(ctx,
		`SELECT `+checkinColumns+` FROM checkin ORDER BY created_at DESC LIMIT $1`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Checkin, 0)
	for rows.Next() {
		var c Checkin
		if err := rows.Scan(&c.ID, &c.Lat, &c.Lon, &c.AccuracyM, &c.Note, &c.UserID, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

type createCheckinReq struct {
	Lat       float64  `json:"lat"`
	Lon       float64  `json:"lon"`
	AccuracyM *float64 `json:"accuracy_m"`
	Note      *string  `json:"note"`
}

func (s *Store) CreateCheckin(ctx context.Context, req createCheckinReq, userID string) (Checkin, error) {
	var c Checkin
	var uid *string
	if userID != "" {
		uid = &userID
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO checkin (lat, lon, accuracy_m, note, user_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+checkinColumns,
		req.Lat, req.Lon, req.AccuracyM, req.Note, uid,
	).Scan(&c.ID, &c.Lat, &c.Lon, &c.AccuracyM, &c.Note, &c.UserID, &c.CreatedAt)
	return c, err
}

func HandleListCheckins(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("history") == "true" {
			hist, err := store.CheckinHistory(r.Context(), 200)
			if err != nil {
				log.Printf("rvedit checkins history: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"history": hist})
			return
		}
		latest, err := store.LatestCheckin(r.Context())
		if err != nil {
			log.Printf("rvedit checkins latest: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"latest": latest})
	}
}

func HandleCreateCheckin(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createCheckinReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Lat < -90 || req.Lat > 90 || req.Lon < -180 || req.Lon > 180 {
			http.Error(w, "lat/lon out of range", http.StatusBadRequest)
			return
		}
		userID := ""
		if s, ok := auth.GetSession(r); ok && s != nil {
			userID = s.Username
		}
		c, err := store.CreateCheckin(r.Context(), req, userID)
		if err != nil {
			log.Printf("rvedit create checkin: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, c)
	}
}

// ============================================================
// dunkin_log (dunkin' sighting counter — trip bet)
// ============================================================
//
// Routes:
//   GET  /dunkin           → public; all log rows ordered by created_at
//   POST /dunkin           → auth;   append a new log entry
//                                    body: {count int, note string?}
//
// Each row is a running-total snapshot; the frontend does the delta
// math + extrapolation. If a POST omits count, the server auto-picks
// (latest count + 1) — so the header button can just POST {} to
// increment.

type DunkinLog struct {
	ID        int64     `json:"id"`
	Count     int       `json:"count"`
	Note      *string   `json:"note"`
	UserID    *string   `json:"user_id"`
	IsSeed    bool      `json:"is_seed"`
	CreatedAt time.Time `json:"created_at"`
}

const dunkinColumns = "id, count, note, user_id, is_seed, created_at"

func (s *Store) ListDunkin(ctx context.Context) ([]DunkinLog, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+dunkinColumns+` FROM dunkin_log ORDER BY created_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DunkinLog, 0)
	for rows.Next() {
		var l DunkinLog
		if err := rows.Scan(&l.ID, &l.Count, &l.Note, &l.UserID, &l.IsSeed, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LatestDunkinCount returns the highest non-seed count so far, which
// is what the auto-increment path (POST with empty body) uses. Seed
// rows are display-only — they shouldn't inflate the real running
// total when the trip's first genuine sighting is logged.
func (s *Store) LatestDunkinCount(ctx context.Context) (int, error) {
	var c int
	err := s.pool.QueryRow(ctx,
		`SELECT count FROM dunkin_log WHERE is_seed = FALSE ORDER BY created_at DESC LIMIT 1`,
	).Scan(&c)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, nil
	}
	return c, err
}

type createDunkinReq struct {
	Count *int    `json:"count"`
	Note  *string `json:"note"`
	// Optional. Lets an authed client backdate a row — useful for
	// backfilling test data or logging a sighting we forgot about
	// hours ago. When omitted, the DB defaults to NOW().
	CreatedAt *time.Time `json:"created_at"`
	// Optional. Marks the row as demo / sample data. The frontend
	// shows seed rows only when no real (non-seed) rows exist yet.
	IsSeed *bool `json:"is_seed"`
}

func (s *Store) CreateDunkin(ctx context.Context, req createDunkinReq, userID string) (DunkinLog, error) {
	// Auto-increment when count omitted.
	count := 0
	if req.Count != nil {
		count = *req.Count
	} else {
		latest, err := s.LatestDunkinCount(ctx)
		if err != nil {
			return DunkinLog{}, err
		}
		count = latest + 1
	}
	var uid *string
	if userID != "" {
		uid = &userID
	}
	// Dynamic INSERT so each combination of optional fields (created_at,
	// is_seed) doesn't need its own statement.
	cols := []string{"count", "note", "user_id"}
	vals := []any{count, req.Note, uid}
	placeholders := []string{"$1", "$2", "$3"}
	if req.CreatedAt != nil {
		cols = append(cols, "created_at")
		vals = append(vals, *req.CreatedAt)
		placeholders = append(placeholders, "$"+strconv.Itoa(len(vals)))
	}
	if req.IsSeed != nil {
		cols = append(cols, "is_seed")
		vals = append(vals, *req.IsSeed)
		placeholders = append(placeholders, "$"+strconv.Itoa(len(vals)))
	}
	q := "INSERT INTO dunkin_log (" + strings.Join(cols, ", ") + ") VALUES (" +
		strings.Join(placeholders, ", ") + ") RETURNING " + dunkinColumns
	var l DunkinLog
	err := s.pool.QueryRow(ctx, q, vals...).Scan(
		&l.ID, &l.Count, &l.Note, &l.UserID, &l.IsSeed, &l.CreatedAt,
	)
	return l, err
}

func HandleListDunkin(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		logs, err := store.ListDunkin(r.Context())
		if err != nil {
			log.Printf("rvedit list dunkin: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
	}
}

func HandleCreateDunkin(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createDunkinReq
		// Empty body is OK — auto-increment.
		if r.ContentLength > 0 {
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, "bad json", http.StatusBadRequest)
				return
			}
		}
		userID := ""
		if s, ok := auth.GetSession(r); ok && s != nil {
			userID = s.Username
		}
		l, err := store.CreateDunkin(r.Context(), req, userID)
		if err != nil {
			log.Printf("rvedit create dunkin: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, l)
	}
}

// ============================================================
// dunkin_participant — the people making the sighting bet
// ============================================================
//
// Routes:
//   GET    /dunkin/participants          → public
//   POST   /dunkin/participants          → auth (create)
//   PATCH  /dunkin/participants/{id}     → auth (partial update)
//   DELETE /dunkin/participants/{id}     → auth (hard delete)

type DunkinParticipant struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Guess     int       `json:"guess"`
	Color     string    `json:"color"`
	SortOrder float64   `json:"sort_order"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

const dunkinParticipantColumns = "id, name, guess, color, sort_order, created_at, updated_at"

func (s *Store) ListDunkinParticipants(ctx context.Context) ([]DunkinParticipant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT `+dunkinParticipantColumns+` FROM dunkin_participant ORDER BY sort_order, id`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DunkinParticipant, 0)
	for rows.Next() {
		var p DunkinParticipant
		if err := rows.Scan(&p.ID, &p.Name, &p.Guess, &p.Color, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

type createDunkinParticipantReq struct {
	ID        string   `json:"id"`
	Name      string   `json:"name"`
	Guess     int      `json:"guess"`
	Color     *string  `json:"color"`
	SortOrder *float64 `json:"sort_order"`
}

func (s *Store) CreateDunkinParticipant(ctx context.Context, req createDunkinParticipantReq) (DunkinParticipant, error) {
	color := "#4a7fa0"
	if req.Color != nil && *req.Color != "" {
		color = *req.Color
	}
	sortOrder := 100.0
	if req.SortOrder != nil {
		sortOrder = *req.SortOrder
	} else {
		// Default to end of the list: max(sort_order)+10.
		var maxSort *float64
		_ = s.pool.QueryRow(ctx, `SELECT MAX(sort_order) FROM dunkin_participant`).Scan(&maxSort)
		if maxSort != nil {
			sortOrder = *maxSort + 10
		}
	}
	var p DunkinParticipant
	err := s.pool.QueryRow(ctx, `
		INSERT INTO dunkin_participant (id, name, guess, color, sort_order)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING `+dunkinParticipantColumns,
		req.ID, req.Name, req.Guess, color, sortOrder,
	).Scan(&p.ID, &p.Name, &p.Guess, &p.Color, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

type patchDunkinParticipantReq struct {
	Name      *string  `json:"name"`
	Guess     *int     `json:"guess"`
	Color     *string  `json:"color"`
	SortOrder *float64 `json:"sort_order"`
}

func (s *Store) PatchDunkinParticipant(ctx context.Context, id string, req patchDunkinParticipantReq) (DunkinParticipant, error) {
	sets := []string{"updated_at = NOW()"}
	args := []any{id}
	addArg := func(col string, v any) {
		args = append(args, v)
		sets = append(sets, col+" = $"+strconv.Itoa(len(args)))
	}
	if req.Name != nil {
		addArg("name", *req.Name)
	}
	if req.Guess != nil {
		addArg("guess", *req.Guess)
	}
	if req.Color != nil {
		addArg("color", *req.Color)
	}
	if req.SortOrder != nil {
		addArg("sort_order", *req.SortOrder)
	}
	q := "UPDATE dunkin_participant SET " + joinSets(sets) + " WHERE id = $1 RETURNING " + dunkinParticipantColumns
	var p DunkinParticipant
	err := s.pool.QueryRow(ctx, q, args...).Scan(&p.ID, &p.Name, &p.Guess, &p.Color, &p.SortOrder, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (s *Store) DeleteDunkinParticipant(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM dunkin_participant WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

func HandleListDunkinParticipants(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		list, err := store.ListDunkinParticipants(r.Context())
		if err != nil {
			log.Printf("rvedit list dunkin participants: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"participants": list})
	}
}

func HandleCreateDunkinParticipant(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req createDunkinParticipantReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.ID == "" || req.Name == "" {
			http.Error(w, "id and name required", http.StatusBadRequest)
			return
		}
		p, err := store.CreateDunkinParticipant(r.Context(), req)
		if err != nil {
			log.Printf("rvedit create dunkin participant: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusCreated, p)
	}
}

func HandlePatchDunkinParticipant(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		var req patchDunkinParticipantReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		p, err := store.PatchDunkinParticipant(r.Context(), id, req)
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit patch dunkin participant: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, p)
	}
}

func HandleDeleteDunkinParticipant(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		if id == "" {
			http.Error(w, "bad id", http.StatusBadRequest)
			return
		}
		if err := store.DeleteDunkinParticipant(r.Context(), id); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				http.Error(w, "not found", http.StatusNotFound)
				return
			}
			log.Printf("rvedit delete dunkin participant: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ============================================================
// draw — shared "everyone can draw" canvas
// ============================================================
//
// Routes:
//   GET  /draw/canvas               → public; canvas metadata
//                                     {id, width, height}
//   GET  /draw/strokes?since=<ms>   → public; strokes newer than the
//                                     given unix-milli cursor. Client
//                                     polls this at a few-second
//                                     cadence for real-time-ish
//                                     collaboration.
//   POST /draw/strokes              → PUBLIC (no auth); appends one
//                                     stroke. Rate-limited by IP.
//
// A stroke is a JSON object: {tool, color, radius, points:[[x,y]...]}.
// Server treats the JSON as opaque — the client validates + renders.

type DrawCanvas struct {
	ID     string `json:"id"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
}

// DrawStroke has the raw json + a server-issued timestamp. We use
// unix-milli in JSON so clients can pass ?since=<int> back verbatim.
type DrawStroke struct {
	ID           int64           `json:"id"`
	CanvasID     string          `json:"canvas_id"`
	Stroke       json.RawMessage `json:"stroke"`
	CreatedAtMs  int64           `json:"created_at_ms"`
}

const strokeRateWindowMs = 500 // one stroke per 500 ms per IP

func (s *Store) GetDrawCanvas(ctx context.Context, id string) (DrawCanvas, error) {
	var c DrawCanvas
	err := s.pool.QueryRow(ctx,
		`SELECT id, width, height FROM draw_canvas WHERE id = $1`, id,
	).Scan(&c.ID, &c.Width, &c.Height)
	return c, err
}

func (s *Store) ListDrawStrokes(ctx context.Context, canvasID string, sinceMs int64, limit int) ([]DrawStroke, error) {
	if limit <= 0 || limit > 5000 {
		limit = 2000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, canvas_id, stroke,
		       (EXTRACT(EPOCH FROM created_at) * 1000)::BIGINT AS created_at_ms
		FROM draw_stroke
		WHERE canvas_id = $1
		  AND (EXTRACT(EPOCH FROM created_at) * 1000)::BIGINT > $2
		ORDER BY id ASC
		LIMIT $3
	`, canvasID, sinceMs, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]DrawStroke, 0)
	for rows.Next() {
		var d DrawStroke
		if err := rows.Scan(&d.ID, &d.CanvasID, &d.Stroke, &d.CreatedAtMs); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// CheckAndInsertStroke enforces the per-IP rate limit inside a single
// SQL statement so two racing requests can't both slip through. If a
// stroke from this IP landed within the last strokeRateWindowMs ms,
// the INSERT is skipped and we return isRateLimited=true.
func (s *Store) CheckAndInsertStroke(ctx context.Context, canvasID string, stroke []byte, ip string) (DrawStroke, bool, error) {
	// The CTE checks the IP's most recent stroke; if it's newer than
	// the rate window, `insert_ok` is empty and no row is inserted.
	q := `
		WITH last_ip AS (
			SELECT MAX(created_at) AS ts
			FROM draw_stroke
			WHERE client_ip = $3
		),
		insert_ok AS (
			SELECT 1
			WHERE COALESCE(
				EXTRACT(EPOCH FROM (NOW() - (SELECT ts FROM last_ip))) * 1000,
				999999999
			) >= $4
		),
		ins AS (
			INSERT INTO draw_stroke (canvas_id, stroke, client_ip)
			SELECT $1, $2::JSONB, $3
			WHERE EXISTS (SELECT 1 FROM insert_ok)
			RETURNING id, canvas_id, stroke,
			          (EXTRACT(EPOCH FROM created_at) * 1000)::BIGINT AS created_at_ms
		)
		SELECT * FROM ins
	`
	var d DrawStroke
	err := s.pool.QueryRow(ctx, q, canvasID, string(stroke), ip, strokeRateWindowMs).Scan(
		&d.ID, &d.CanvasID, &d.Stroke, &d.CreatedAtMs,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return DrawStroke{}, true, nil // rate-limited
	}
	if err != nil {
		return DrawStroke{}, false, err
	}
	return d, false, nil
}

// clientIP returns the caller's IP for rate-limit accounting. Apache
// puts the real client in X-Forwarded-For; the last comma-separated
// entry is the closest hop. Falls back to RemoteAddr.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		// Trust the first entry — Apache's mod_proxy sets it to the
		// direct client. Trim + strip any port.
		parts := strings.Split(xff, ",")
		return strings.TrimSpace(parts[0])
	}
	// RemoteAddr is host:port; drop the port.
	if idx := strings.LastIndex(r.RemoteAddr, ":"); idx > 0 {
		return r.RemoteAddr[:idx]
	}
	return r.RemoteAddr
}

func HandleGetDrawCanvas(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := store.GetDrawCanvas(r.Context(), "main")
		if errors.Is(err, pgx.ErrNoRows) {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if err != nil {
			log.Printf("rvedit get canvas: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, c)
	}
}

func HandleListDrawStrokes(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		sinceMs := int64(0)
		if s := r.URL.Query().Get("since"); s != "" {
			if v, err := strconv.ParseInt(s, 10, 64); err == nil && v >= 0 {
				sinceMs = v
			}
		}
		list, err := store.ListDrawStrokes(r.Context(), "main", sinceMs, 2000)
		if err != nil {
			log.Printf("rvedit list strokes: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"strokes": list})
	}
}

func HandleCreateDrawStroke(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// Cap the body so bots can't ship enormous strokes. 16 KB is
		// plenty for even a very dense path in JSON.
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "body too large or unreadable", http.StatusBadRequest)
			return
		}
		// Validate that it's at least well-formed JSON so we don't
		// insert junk into JSONB.
		var probe map[string]any
		if err := json.Unmarshal(body, &probe); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		ip := clientIP(r)
		d, limited, err := store.CheckAndInsertStroke(r.Context(), "main", body, ip)
		if err != nil {
			log.Printf("rvedit create stroke: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if limited {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		writeJSON(w, http.StatusCreated, d)
	}
}
