// Package auth handles password hashing and DB-backed cookie sessions.
//
// Each project has its own database with `user` and `session` tables —
// SessionStore is instantiated per-project against that project's pool.
// Cookie names are project-scoped so multiple projects don't share an
// auth cookie surface.
//
// Mirrors github.com/slackwing/manuscript-studio/internal/auth (trimmed:
// no CSRF tokens for now since we have no state-changing endpoints).
package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/crypto/bcrypt"
)

type contextKey string

const sessionContextKey contextKey = "session"

// SessionTTL is how long a fresh or just-touched session stays valid.
// 30 days, sliding (per Andrew's choice in initial design).
const SessionTTL = 30 * 24 * time.Hour

// sessionRefreshThreshold: when a session has less than this left,
// Get() bumps expires_at by SessionTTL. Avoids a DB write per request.
const sessionRefreshThreshold = 7 * 24 * time.Hour

type Session struct {
	Token     string // the cookie value (also the row's id)
	Username  string
	CreatedAt time.Time
	ExpiresAt time.Time
}

type SessionStore struct {
	pool *pgxpool.Pool
}

func NewSessionStore(pool *pgxpool.Pool) *SessionStore {
	s := &SessionStore{pool: pool}
	go s.cleanupExpired()
	return s
}

func (s *SessionStore) Create(username string) (string, error) {
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	expires := now.Add(SessionTTL)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err = s.pool.Exec(ctx, `
		INSERT INTO session (id, username, created_at, expires_at, last_activity_at)
		VALUES ($1, $2, $3, $4, $3)
	`, token, username, now, expires)
	if err != nil {
		return "", fmt.Errorf("failed to insert session: %w", err)
	}
	return token, nil
}

func (s *SessionStore) Get(token string) (*Session, bool) {
	if token == "" {
		return nil, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var (
		username             string
		createdAt, expiresAt time.Time
	)
	err := s.pool.QueryRow(ctx, `
		SELECT username, created_at, expires_at
		FROM session WHERE id = $1
	`, token).Scan(&username, &createdAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		log.Printf("session lookup error: %v", err)
		return nil, false
	}

	now := time.Now().UTC()
	if !now.Before(expiresAt) {
		_, _ = s.pool.Exec(ctx, `DELETE FROM session WHERE id = $1`, token)
		return nil, false
	}

	newExpires := expiresAt
	if expiresAt.Sub(now) < sessionRefreshThreshold {
		newExpires = now.Add(SessionTTL)
	}
	_, _ = s.pool.Exec(ctx, `
		UPDATE session SET last_activity_at = $1, expires_at = $2 WHERE id = $3
	`, now, newExpires, token)

	return &Session{
		Token:     token,
		Username:  username,
		CreatedAt: createdAt,
		ExpiresAt: newExpires,
	}, true
}

func (s *SessionStore) Delete(token string) {
	if token == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = s.pool.Exec(ctx, `DELETE FROM session WHERE id = $1`, token)
}

func (s *SessionStore) cleanupExpired() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := s.pool.Exec(ctx, `DELETE FROM session WHERE expires_at < NOW()`)
		cancel()
		if err != nil {
			log.Printf("session cleanup error: %v", err)
		}
	}
}

func generateToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
}

func HashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(hash), nil
}

func VerifyPassword(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}

func ValidatePassword(password string) error {
	if len(password) < 4 {
		return fmt.Errorf("password must be at least 4 characters")
	}
	return nil
}

// LookupUser returns the password hash for a username, or "" if not found.
func LookupUser(pool *pgxpool.Pool, username string) (string, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var hash string
	err := pool.QueryRow(ctx, `SELECT password_hash FROM "user" WHERE username = $1`, username).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// Middleware requires a valid session cookie. Returns 401 otherwise.
// cookieName is project-scoped (e.g. "rv_session", "next_session") so
// projects don't share a cookie surface.
func Middleware(store *SessionStore, cookieName string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			cookie, err := r.Cookie(cookieName)
			if err != nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			session, valid := store.Get(cookie.Value)
			if !valid {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := context.WithValue(r.Context(), sessionContextKey, session)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func GetSession(r *http.Request) (*Session, bool) {
	s, ok := r.Context().Value(sessionContextKey).(*Session)
	return s, ok
}
