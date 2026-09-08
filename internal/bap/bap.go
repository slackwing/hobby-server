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
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strings"
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
	// Burst names the break effect the client rolled for this cup
	// ("glass", "hearts", "picks"). Transient — used only to pick the
	// notification's exception name, never stored.
	Burst string `json:"burst,omitempty"`
	// Color is the cup color the client rolled ("blue"/"green"/"pink").
	// Stored as last_color so the next load can exclude it.
	Color string `json:"color"`
}

func validColor(c string) bool {
	return c == "blue" || c == "green" || c == "pink"
}

// exceptionFor maps a burst kind to the fake exception in the alert
// (names per Andrew: blue/pink/green cups).
func exceptionFor(burst string) string {
	switch burst {
	case "hearts":
		return "SupportQueueEvent(R)"
	case "picks":
		return "UnknownException(G)"
	case "glass":
		return "NullPointerException(B)"
	}
	return "UnknownException(?)"
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
		SELECT cup_x, baps, broken, shatters, last_color FROM bap_cup_state WHERE username = $1
	`, username).Scan(&st.CupX, &st.Baps, &st.Broken, &st.Shatters, &st.Color)
	if err == pgx.ErrNoRows {
		return CupState{}, nil
	}
	return st, err
}

// PutState upserts the user's cup and reports whether the cup was
// already broken beforehand (so callers can detect the moment of
// shattering: st.Broken && !wasBroken).
func (s *Store) PutState(username string, st CupState) (wasBroken bool, err error) {
	ctx, cancel := withCtx()
	defer cancel()
	color := st.Color
	if !validColor(color) {
		color = ""
	}
	err = s.pool.QueryRow(ctx, `
		WITH old AS (SELECT broken FROM bap_cup_state WHERE username = $1)
		INSERT INTO bap_cup_state (username, cup_x, baps, broken, shatters, last_color, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (username) DO UPDATE SET
			cup_x = EXCLUDED.cup_x,
			baps = EXCLUDED.baps,
			broken = EXCLUDED.broken,
			shatters = EXCLUDED.shatters,
			last_color = EXCLUDED.last_color,
			updated_at = NOW()
		RETURNING COALESCE((SELECT broken FROM old), false)
	`, username, st.CupX, st.Baps, st.Broken, st.Shatters, color).Scan(&wasBroken)
	return wasBroken, err
}

type ctxKey int

const userKey ctxKey = 0

// Notifier sends the cup-shattered Telegram message. Zero-valued =
// notifications disabled.
type Notifier struct {
	BotToken string
	ChatID   string
}

// Notify fires the Telegram message in the calling goroutine; run it
// with `go`. The bot token must never reach the logs (AGENTS.md N4) —
// errors are logged with the token redacted. The message format is
// exactly as Andrew specified (pager parody).
func (n Notifier) Notify(baps int, exception string) {
	if n.BotToken == "" || n.ChatID == "" {
		return
	}
	msg := fmt.Sprintf("ALRT #%06d on Ads API BAP Service: %s: null. Reply 4: Ack, 6: Resolv",
		baps, exception)
	body := url.Values{"chat_id": {n.ChatID}, "text": {msg}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.telegram.org/bot"+n.BotToken+"/sendMessage",
		strings.NewReader(body.Encode()))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("[bap] telegram notify error: %s",
			strings.ReplaceAll(err.Error(), n.BotToken, "<token>"))
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[bap] telegram notify: status %d", resp.StatusCode)
	}
}

// Mount wires the bap endpoints. All routes require a valid shared
// session; state is keyed to the session's user.
func Mount(r chi.Router, store *Store, auth *shared.Store, notify Notifier) {
	r.Group(func(g chi.Router) {
		g.Use(requireLogin(auth))
		g.Get("/state", handleGetState(store))
		g.Put("/state", handlePutState(store, notify))
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

func handlePutState(store *Store, notify Notifier) http.HandlerFunc {
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
		wasBroken, err := store.PutState(username, st)
		if err != nil {
			log.Printf("[bap] put state error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if st.Broken && !wasBroken {
			// e.g. NullPointerException(B)[an] — first two letters of the user
			tag := username
			if len(tag) > 2 {
				tag = tag[:2]
			}
			go notify.Notify(st.Baps, exceptionFor(st.Burst)+"["+tag+"]")
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}
