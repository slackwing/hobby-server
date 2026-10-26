// Package sxiva serves the status dashboard (andrewcheong.com/status/):
// one row per day of Andrew's .sxiva minutes files, uploaded by the sxiva
// CLI, and the rolling series the dashboard charts.
//
// No login: the reads are public (the page has no users), and the two
// endpoints the CLI uses are gated by a bearer token from the project's
// config block (sync_token). Ported from a standalone Flask app
// (feathers 11.sxiv/dashboard) with the same paths and JSON shapes.
package sxiva

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Defaults for the read endpoints' query parameters (as the Flask API had).
const (
	defaultLimit         = 60
	defaultCategoryLimit = 30
	defaultWindow        = 7
	defaultLambda        = 0.2
	maxLambda            = 100 // e^(λ·age) must stay finite
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func withCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// Days returns every row, oldest first.
func (s *Store) Days() ([]Day, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT date, category_minutes, meet, alc, exe, dep_avg, sleep_score,
		       drc_triggers, drc_resisted, drc_indulged
		FROM sxiva_daily_summary
		ORDER BY date
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var days []Day
	for rows.Next() {
		var d Day
		var cats []byte
		if err := rows.Scan(&d.Date, &cats, &d.Meet, &d.Alc, &d.Exe, &d.DepAvg, &d.SleepScore,
			&d.DRCTriggers, &d.DRCResisted, &d.DRCIndulged); err != nil {
			return nil, err
		}
		var raw map[string]float64
		if err := json.Unmarshal(cats, &raw); err != nil {
			return nil, err
		}
		d.CategoryMinutes = make(map[string]int64, len(raw))
		for k, v := range raw {
			d.CategoryMinutes[k] = int64(math.Round(v))
		}
		days = append(days, d)
	}
	return days, rows.Err()
}

// DailySync is the body of POST /sync/daily: one parsed .sxiva file. Only
// date and day_of_week are required; absent numbers are stored as NULL.
type DailySync struct {
	Date            string          `json:"date"`
	DayOfWeek       string          `json:"day_of_week"`
	CategoryMinutes json.RawMessage `json:"category_minutes"`
	SleepScore      *float64        `json:"sleep_score"`
	SleepHours      *float64        `json:"sleep_hours"`
	DepMin          *float64        `json:"dep_min"`
	DepMax          *float64        `json:"dep_max"`
	DepAvg          *float64        `json:"dep_avg"`
	Dist            *float64        `json:"dist"`
	Soc             *float64        `json:"soc"`
	Out             *float64        `json:"out"`
	Exe             *float64        `json:"exe"`
	Alc             *float64        `json:"alc"`
	Xmx             *float64        `json:"xmx"`
	Wea             *float64        `json:"wea"`
	Meet            *float64        `json:"meet"`
	Abi             *float64        `json:"abi"`
	Save            *float64        `json:"save"`
	DRCTriggers     *float64        `json:"drc_triggers"`
	DRCResisted     *float64        `json:"drc_resisted"`
	DRCIndulged     *float64        `json:"drc_indulged"`
}

// Upsert stores one day (replacing it if present) and stamps the sync
// metadata, in one transaction. Numbers go in as float8 and Postgres casts
// them to each column's type (rounding to its scale).
func (s *Store) Upsert(d DailySync) error {
	ctx, cancel := withCtx()
	defer cancel()
	cats := string(d.CategoryMinutes)
	if t := strings.TrimSpace(cats); t == "" || t == "null" {
		cats = "{}"
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `
		INSERT INTO sxiva_daily_summary (
			date, day_of_week, category_minutes,
			sleep_score, sleep_hours, dep_min, dep_max, dep_avg,
			dist, soc, out, exe, alc, xmx, wea, meet, abi, save,
			drc_triggers, drc_resisted, drc_indulged,
			created_at, updated_at
		) VALUES (
			$1::date, $2, $3::jsonb,
			$4::float8, $5::float8, $6::float8, $7::float8, $8::float8,
			$9::float8, $10::float8, $11::float8, $12::float8, $13::float8,
			$14::float8, $15::float8, $16::float8, $17::float8, $18::float8,
			$19::float8, $20::float8, $21::float8,
			NOW(), NOW()
		)
		ON CONFLICT (date) DO UPDATE SET
			day_of_week = EXCLUDED.day_of_week,
			category_minutes = EXCLUDED.category_minutes,
			sleep_score = EXCLUDED.sleep_score,
			sleep_hours = EXCLUDED.sleep_hours,
			dep_min = EXCLUDED.dep_min,
			dep_max = EXCLUDED.dep_max,
			dep_avg = EXCLUDED.dep_avg,
			dist = EXCLUDED.dist,
			soc = EXCLUDED.soc,
			out = EXCLUDED.out,
			exe = EXCLUDED.exe,
			alc = EXCLUDED.alc,
			xmx = EXCLUDED.xmx,
			wea = EXCLUDED.wea,
			meet = EXCLUDED.meet,
			abi = EXCLUDED.abi,
			save = EXCLUDED.save,
			drc_triggers = EXCLUDED.drc_triggers,
			drc_resisted = EXCLUDED.drc_resisted,
			drc_indulged = EXCLUDED.drc_indulged,
			updated_at = NOW()
	`, d.Date, d.DayOfWeek, cats,
		d.SleepScore, d.SleepHours, d.DepMin, d.DepMax, d.DepAvg,
		d.Dist, d.Soc, d.Out, d.Exe, d.Alc, d.Xmx, d.Wea, d.Meet, d.Abi, d.Save,
		d.DRCTriggers, d.DRCResisted, d.DRCIndulged); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE sxiva_sync_metadata
		SET last_sync_timestamp = NOW(),
		    last_sync_file_count = last_sync_file_count + 1
		WHERE id = 1
	`); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// LastSync returns when the CLI last uploaded a file, and how many files
// it has uploaded in all.
func (s *Store) LastSync() (time.Time, int64, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var at time.Time
	var count *int64
	err := s.pool.QueryRow(ctx, `
		SELECT last_sync_timestamp, last_sync_file_count
		FROM sxiva_sync_metadata WHERE id = 1
	`).Scan(&at, &count)
	if count == nil {
		return at, 0, err
	}
	return at, *count, err
}

func (s *Store) Ping() error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `SELECT 1`)
	return err
}

// Mount wires the sxiva endpoints. token gates the CLI's two endpoints;
// writing (nil = not configured) feeds the Sabbatical '27 writing squares.
func Mount(r chi.Router, store *Store, token string, writing *WritingSource) {
	r.Get("/health", handleHealth(store))
	r.Get("/sabbatical", handleSabbatical(store, writing))
	r.Get("/category-rolling-sum", handleCategoryRollingSum(store))
	r.Get("/alcohol-depression", handleSeries(store, AlcoholMood))
	r.Get("/sleep-score", handleSeries(store, Sleep))
	r.Get("/drc", handleSeries(store, DRC))
	r.Group(func(g chi.Router) {
		g.Use(requireToken(token))
		g.Post("/sync/daily", handleSyncDaily(store))
		g.Get("/status/last-sync", handleLastSync(store))
	})
}

// requireToken checks "Authorization: Bearer <token>" in constant time.
// With no token configured the gated endpoints are off (503).
func requireToken(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				writeError(w, http.StatusServiceUnavailable, "sync not configured")
				return
			}
			parts := strings.Fields(r.Header.Get("Authorization"))
			if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") ||
				subtle.ConstantTimeCompare([]byte(parts[1]), []byte(token)) != 1 {
				writeError(w, http.StatusUnauthorized, "Unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func handleHealth(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		if err := store.Ping(); err != nil {
			log.Printf("[sxiva] health: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"status": "unhealthy"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"status": "healthy", "database": "connected"})
	}
}

// handleSeries serves a per-day series: ?limit=N most recent days (default
// 60), oldest first.
func handleSeries[T any](store *Store, compute func([]Day) []T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		limit, err := intParam(r, "limit", defaultLimit)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid numeric parameter")
			return
		}
		if limit < 1 {
			writeError(w, http.StatusBadRequest, "limit must be a positive integer")
			return
		}
		days, err := store.Days()
		if err != nil {
			log.Printf("[sxiva] read days: %v", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": tail(compute(days), limit)})
	}
}

type categoryResponse struct {
	Data            []CategoryRow `json:"data"`
	HobbyCategories []string      `json:"hobby_categories"`
	WorkCategories  []string      `json:"work_categories"`
	StudyCategories []string      `json:"study_categories"`
	OtherCategories []string      `json:"other_categories"`
	WindowDays      int           `json:"window_days"`
	DecayLambda     float64       `json:"decay_lambda"`
}

// handleCategoryRollingSum: ?hobby=a,b&work=c&study=d,e&days=7&limit=30&lambda=0.2
// (study optional).
func handleCategoryRollingSum(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("hobby") == "" && q.Get("work") == "" {
			writeError(w, http.StatusBadRequest, "At least one of hobby or work parameters required")
			return
		}
		groups := Groups{Hobby: splitCodes(q.Get("hobby")), Work: splitCodes(q.Get("work")), Study: splitCodes(q.Get("study"))}
		window, err1 := intParam(r, "days", defaultWindow)
		limit, err2 := intParam(r, "limit", defaultCategoryLimit)
		lambda, err3 := floatParam(r, "lambda", defaultLambda)
		if err1 != nil || err2 != nil || err3 != nil {
			writeError(w, http.StatusBadRequest, "Invalid numeric parameter")
			return
		}
		if window < 1 || limit < 1 {
			writeError(w, http.StatusBadRequest, "days and limit must be positive integers")
			return
		}
		if math.IsNaN(lambda) || math.Abs(lambda) > maxLambda {
			writeError(w, http.StatusBadRequest, "lambda out of range")
			return
		}
		days, err := store.Days()
		if err != nil {
			log.Printf("[sxiva] read days: %v", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		lists := Categories(days, groups)
		writeJSON(w, http.StatusOK, categoryResponse{
			Data:            tail(RollingCategory(days, groups, window, lambda), limit),
			HobbyCategories: lists.Hobby,
			WorkCategories:  lists.Work,
			StudyCategories: lists.Study,
			OtherCategories: lists.Other,
			WindowDays:      window,
			DecayLambda:     lambda,
		})
	}
}

// handleSabbatical: per-day square counts for the Sabbatical '27 grid.
// Writing comes from manuscript-studio; if that fails the other types are
// still served, with writing_available false.
func handleSabbatical(store *Store, writing *WritingSource) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		days, err := store.Days()
		if err != nil {
			log.Printf("[sxiva] read days: %v", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		var points map[string]int
		if writing != nil {
			if points, err = writing.Points(r.Context()); err != nil {
				log.Printf("[sxiva] sabbatical: %v", err)
				points = nil
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"writing_available": points != nil,
			"days":              Sabbatical(days, points),
		})
	}
}

func handleSyncDaily(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var d DailySync
		if err := json.NewDecoder(r.Body).Decode(&d); err != nil {
			writeError(w, http.StatusBadRequest, "Invalid JSON")
			return
		}
		if d.Date == "" {
			writeError(w, http.StatusBadRequest, "Missing required field: date")
			return
		}
		if d.DayOfWeek == "" {
			writeError(w, http.StatusBadRequest, "Missing required field: day_of_week")
			return
		}
		if _, err := time.Parse(dateLayout, d.Date); err != nil {
			writeError(w, http.StatusBadRequest, "date must be YYYY-MM-DD")
			return
		}
		if c := strings.TrimSpace(string(d.CategoryMinutes)); c != "" && c != "null" && !strings.HasPrefix(c, "{") {
			writeError(w, http.StatusBadRequest, "category_minutes must be an object")
			return
		}
		if err := store.Upsert(d); err != nil {
			// Bad values (a number too big for its column…) are the client's
			// to fix: Postgres "data exception" class 22.
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && strings.HasPrefix(pgErr.Code, "22") {
				writeError(w, http.StatusBadRequest, pgErr.Message)
				return
			}
			log.Printf("[sxiva] sync %s: %v", d.Date, err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{
			"status":  "success",
			"date":    d.Date,
			"message": "Data synced successfully",
		})
	}
}

func handleLastSync(store *Store) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		at, count, err := store.LastSync()
		if err != nil {
			log.Printf("[sxiva] last sync: %v", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			// Python's isoformat(), which the CLI parses back
			"last_sync_timestamp":  at.UTC().Format("2006-01-02T15:04:05.000000-07:00"),
			"last_sync_file_count": count,
		})
	}
}

// tail returns the last n rows (all of them if there are fewer).
func tail[T any](rows []T, n int) []T {
	if len(rows) <= n {
		return rows
	}
	return rows[len(rows)-n:]
}

// splitCodes parses "a, b,,c" into [a b c].
func splitCodes(s string) []string {
	var out []string
	for _, c := range strings.Split(s, ",") {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

func intParam(r *http.Request, name string, def int) (int, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, nil
	}
	return strconv.Atoi(strings.TrimSpace(s))
}

func floatParam(r *http.Request, name string, def float64) (float64, error) {
	s := r.URL.Query().Get(name)
	if s == "" {
		return def, nil
	}
	return strconv.ParseFloat(strings.TrimSpace(s), 64)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
