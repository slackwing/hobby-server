// Package shared implements the cross-website auth system ("admin"
// project): one account per person in hobby_server_user, per-website
// access via hobby_server_user_roles, SSO sessions (hobby_session
// cookie at Path=/), and one-time invite/reset-password tokens.
//
// Tables live in the SHARED hobby_server database (schema in
// liquibase/admin/changelog/). Independent of the legacy per-project
// user/session tables that rv uses.
package shared

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SessionTTL / refresh mirror internal/auth (30-day sliding sessions).
const (
	SessionTTL              = 30 * 24 * time.Hour
	sessionRefreshThreshold = 7 * 24 * time.Hour

	InviteTTL = 7 * 24 * time.Hour
	ResetTTL  = 1 * time.Hour
)

var usernameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,49}$`)

func ValidateUsername(username string) error {
	if !usernameRe.MatchString(username) {
		return fmt.Errorf("username must be 1-50 chars: lowercase letters, digits, . _ -")
	}
	return nil
}

type Role struct {
	Website string `json:"website"`
	Role    string `json:"role"`
}

type User struct {
	Username    string    `json:"username"`
	DisplayName string    `json:"display_name"`
	HasPassword bool      `json:"has_password"`
	CreatedAt   time.Time `json:"created_at"`
	Roles       []Role    `json:"roles"`
}

type Website struct {
	Website string   `json:"website"`
	Roles   []string `json:"roles"`
}

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	s := &Store{pool: pool}
	go s.cleanupExpired()
	return s
}

func withCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Second)
}

// ---------- users ----------

// GetUser returns display name and password hash (nil = unset).
func (s *Store) GetUser(username string) (displayName string, hash *string, ok bool, err error) {
	ctx, cancel := withCtx()
	defer cancel()
	err = s.pool.QueryRow(ctx, `
		SELECT display_name, password_hash FROM hobby_server_user WHERE username = $1
	`, username).Scan(&displayName, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, err
	}
	return displayName, hash, true, nil
}

func (s *Store) ListUsers() ([]User, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT username, display_name, password_hash IS NOT NULL, created_at
		FROM hobby_server_user ORDER BY username
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byName := map[string]*User{}
	users := []User{}
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.Username, &u.DisplayName, &u.HasPassword, &u.CreatedAt); err != nil {
			return nil, err
		}
		u.Roles = []Role{}
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range users {
		byName[users[i].Username] = &users[i]
	}
	rrows, err := s.pool.Query(ctx, `
		SELECT username, website, role FROM hobby_server_user_roles
		ORDER BY username, website, role
	`)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var username string
		var r Role
		if err := rrows.Scan(&username, &r.Website, &r.Role); err != nil {
			return nil, err
		}
		if u, ok := byName[username]; ok {
			u.Roles = append(u.Roles, r)
		}
	}
	return users, rrows.Err()
}

func (s *Store) CreateUser(username, displayName string) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO hobby_server_user (username, display_name) VALUES ($1, $2)
	`, username, displayName)
	return err
}

// DeleteUser removes an account; roles, sessions, and password tokens
// cascade (FKs in 001-shared-auth-schema.xml).
func (s *Store) DeleteUser(username string) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
		DELETE FROM hobby_server_user WHERE username = $1
	`, username)
	return tag.RowsAffected() > 0, err
}

func (s *Store) UpdateDisplayName(username, displayName string) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tag, err := s.pool.Exec(ctx, `
		UPDATE hobby_server_user SET display_name = $1 WHERE username = $2
	`, displayName, username)
	return tag.RowsAffected() > 0, err
}

func (s *Store) UserRoles(username string) ([]Role, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `
		SELECT website, role FROM hobby_server_user_roles
		WHERE username = $1 ORDER BY website, role
	`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	roles := []Role{}
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.Website, &r.Role); err != nil {
			return nil, err
		}
		roles = append(roles, r)
	}
	return roles, rows.Err()
}

func (s *Store) HasRole(username, website, role string) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var one int
	err := s.pool.QueryRow(ctx, `
		SELECT 1 FROM hobby_server_user_roles
		WHERE username = $1 AND website = $2 AND role = $3
	`, username, website, role).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (s *Store) AddRole(username, website, role string) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		INSERT INTO hobby_server_user_roles (username, website, role)
		VALUES ($1, $2, $3) ON CONFLICT DO NOTHING
	`, username, website, role)
	return err
}

func (s *Store) RemoveRole(username, website, role string) error {
	ctx, cancel := withCtx()
	defer cancel()
	_, err := s.pool.Exec(ctx, `
		DELETE FROM hobby_server_user_roles
		WHERE username = $1 AND website = $2 AND role = $3
	`, username, website, role)
	return err
}

// ---------- websites ----------

func (s *Store) ListWebsites() ([]Website, error) {
	ctx, cancel := withCtx()
	defer cancel()
	rows, err := s.pool.Query(ctx, `SELECT website FROM hobby_server_websites ORDER BY website`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sites := []Website{}
	byName := map[string]*Website{}
	for rows.Next() {
		var w Website
		if err := rows.Scan(&w.Website); err != nil {
			return nil, err
		}
		w.Roles = []string{}
		sites = append(sites, w)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range sites {
		byName[sites[i].Website] = &sites[i]
	}
	rrows, err := s.pool.Query(ctx, `
		SELECT website, role FROM hobby_server_website_roles ORDER BY website, role
	`)
	if err != nil {
		return nil, err
	}
	defer rrows.Close()
	for rrows.Next() {
		var website, role string
		if err := rrows.Scan(&website, &role); err != nil {
			return nil, err
		}
		if w, ok := byName[website]; ok {
			w.Roles = append(w.Roles, role)
		}
	}
	return sites, rrows.Err()
}

func (s *Store) WebsiteExists(website string) (bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	var one int
	err := s.pool.QueryRow(ctx, `SELECT 1 FROM hobby_server_websites WHERE website = $1`, website).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ---------- sessions ----------

func (s *Store) CreateSession(username string) (string, error) {
	token, err := randomToken()
	if err != nil {
		return "", err
	}
	now := time.Now().UTC()
	ctx, cancel := withCtx()
	defer cancel()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO hobby_server_session (id, username, created_at, expires_at, last_activity_at)
		VALUES ($1, $2, $3, $4, $3)
	`, token, username, now, now.Add(SessionTTL))
	if err != nil {
		return "", fmt.Errorf("insert session: %w", err)
	}
	return token, nil
}

// GetSession resolves a session token to a username, sliding the
// expiry when it drops under the refresh threshold.
func (s *Store) GetSession(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	ctx, cancel := withCtx()
	defer cancel()
	var username string
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT username, expires_at FROM hobby_server_session WHERE id = $1
	`, token).Scan(&username, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false
	}
	if err != nil {
		log.Printf("[admin] session lookup error: %v", err)
		return "", false
	}
	now := time.Now().UTC()
	if !now.Before(expiresAt) {
		_, _ = s.pool.Exec(ctx, `DELETE FROM hobby_server_session WHERE id = $1`, token)
		return "", false
	}
	newExpires := expiresAt
	if expiresAt.Sub(now) < sessionRefreshThreshold {
		newExpires = now.Add(SessionTTL)
	}
	_, _ = s.pool.Exec(ctx, `
		UPDATE hobby_server_session SET last_activity_at = $1, expires_at = $2 WHERE id = $3
	`, now, newExpires, token)
	return username, true
}

func (s *Store) DeleteSession(token string) {
	if token == "" {
		return
	}
	ctx, cancel := withCtx()
	defer cancel()
	_, _ = s.pool.Exec(ctx, `DELETE FROM hobby_server_session WHERE id = $1`, token)
}

func (s *Store) cleanupExpired() {
	ticker := time.NewTicker(15 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := s.pool.Exec(ctx, `DELETE FROM hobby_server_session WHERE expires_at < NOW()`)
		if err == nil {
			_, err = s.pool.Exec(ctx, `DELETE FROM hobby_server_password_token WHERE expires_at < NOW()`)
		}
		cancel()
		if err != nil {
			log.Printf("[admin] cleanup error: %v", err)
		}
	}
}

// ---------- invite / reset tokens ----------

// CreateToken mints a one-time set-password code for a user. Only the
// SHA-256 of the code is stored; the code itself goes into the link and
// is never persisted or logged. website selects the invite skin
// ("admin" for plain resets).
func (s *Store) CreateToken(username, website string, ttl time.Duration) (code string, expiresAt time.Time, err error) {
	code, err = randomToken()
	if err != nil {
		return "", time.Time{}, err
	}
	expiresAt = time.Now().UTC().Add(ttl)
	ctx, cancel := withCtx()
	defer cancel()
	_, err = s.pool.Exec(ctx, `
		INSERT INTO hobby_server_password_token (token_hash, username, website, expires_at)
		VALUES ($1, $2, $3, $4)
	`, hashCode(code), username, website, expiresAt)
	if err != nil {
		return "", time.Time{}, err
	}
	return code, expiresAt, nil
}

// LookupToken returns the token's user/website if it is valid (exists,
// unexpired, unused).
func (s *Store) LookupToken(code string) (username, website string, expiresAt time.Time, ok bool, err error) {
	ctx, cancel := withCtx()
	defer cancel()
	err = s.pool.QueryRow(ctx, `
		SELECT username, website, expires_at FROM hobby_server_password_token
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW()
	`, hashCode(code)).Scan(&username, &website, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", time.Time{}, false, nil
	}
	if err != nil {
		return "", "", time.Time{}, false, err
	}
	return username, website, expiresAt, true, nil
}

// ConsumeToken atomically marks the token used and sets the user's
// password hash. Returns the username, or ok=false if the token was
// invalid (already used, expired, unknown).
func (s *Store) ConsumeToken(code, passwordHash string) (string, bool, error) {
	ctx, cancel := withCtx()
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var username string
	err = tx.QueryRow(ctx, `
		UPDATE hobby_server_password_token SET used_at = NOW()
		WHERE token_hash = $1 AND used_at IS NULL AND expires_at > NOW()
		RETURNING username
	`, hashCode(code)).Scan(&username)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE hobby_server_user SET password_hash = $1 WHERE username = $2
	`, passwordHash, username); err != nil {
		return "", false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", false, err
	}
	return username, true, nil
}

func randomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashCode(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}
