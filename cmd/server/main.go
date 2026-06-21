// rv-server: authentication backend for the RV trip site.
//
// Frontend lives at github.com/slackwing/feathers under
// foundry/website/html/rv/. Apache reverse-proxies /rv/api/* to this
// server (default port 5002).
//
// Endpoints (all under /api/):
//   POST /api/login   {username, password}  → sets rv_session cookie
//   POST /api/logout                         → clears cookie + deletes session
//   GET  /api/me                             → {username} when logged in, 401 otherwise
package main

import (
	"context"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/slackwing/rv-server/internal/auth"
	"github.com/slackwing/rv-server/internal/config"
	"github.com/slackwing/rv-server/internal/database"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", defaultConfigPath(), "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	log.Printf("rv-server starting on :%d (env=%s)", cfg.Server.Port, cfg.Server.Env)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool, err := database.NewPool(ctx, cfg.PostgresDSN())
	if err != nil {
		log.Fatalf("connect db: %v", err)
	}
	defer pool.Close()

	store := auth.NewSessionStore(pool)

	r := chi.NewRouter()
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(15 * time.Second))

	r.Route("/api", func(r chi.Router) {
		r.Post("/login", handleLogin(pool, store, cfg))
		r.Post("/logout", handleLogout(store, cfg))
		r.Get("/me", handleMe(store))
	})

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Server.Port),
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("server error: %v", err)
		}
	}()
	<-stop
	log.Printf("shutting down...")
	shutdownCtx, c2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer c2()
	_ = srv.Shutdown(shutdownCtx)
}

func defaultConfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "rv-server", "config.yaml")
}

// ---------- handlers ----------

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func handleLogin(pool *pgxpool.Pool, store *auth.SessionStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req loginReq
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json", http.StatusBadRequest)
			return
		}
		if req.Username == "" || req.Password == "" {
			http.Error(w, "username and password required", http.StatusBadRequest)
			return
		}
		hash, ok, err := auth.LookupUser(pool, req.Username)
		if err != nil {
			log.Printf("login lookup error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Constant-time-ish: always do a bcrypt comparison to avoid leaking
		// user existence via timing. If the user doesn't exist, compare
		// against a known dummy hash.
		if !ok {
			auth.VerifyPassword(req.Password, dummyHash)
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		if !auth.VerifyPassword(req.Password, hash) {
			http.Error(w, "invalid credentials", http.StatusUnauthorized)
			return
		}
		token, err := store.Create(req.Username)
		if err != nil {
			log.Printf("session create error: %v", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		setSessionCookie(w, token, cfg)
		writeJSON(w, http.StatusOK, map[string]string{"username": req.Username})
	}
}

func handleLogout(store *auth.SessionStore, cfg *config.Config) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if cookie, err := r.Cookie("rv_session"); err == nil {
			store.Delete(cookie.Value)
		}
		clearSessionCookie(w, cfg)
		w.WriteHeader(http.StatusNoContent)
	}
}

func handleMe(store *auth.SessionStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("rv_session")
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		s, ok := store.Get(cookie.Value)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"username": s.Username})
	}
}

// ---------- cookie + json helpers ----------

func setSessionCookie(w http.ResponseWriter, token string, cfg *config.Config) {
	secure := cfg.Server.Env == "production"
	http.SetCookie(w, &http.Cookie{
		Name:     "rv_session",
		Value:    token,
		Path:     "/rv/", // scoped under /rv/ so we don't bleed into other apps on the domain
		Expires:  time.Now().Add(auth.SessionTTL),
		MaxAge:   int(auth.SessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, cfg *config.Config) {
	secure := cfg.Server.Env == "production"
	http.SetCookie(w, &http.Cookie{
		Name:     "rv_session",
		Value:    "",
		Path:     "/rv/",
		Expires:  time.Unix(0, 0),
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   secure,
		SameSite: http.SameSiteLaxMode,
	})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// dummyHash is a precomputed bcrypt hash used only to keep failed-login
// timing roughly constant whether or not the username exists.
const dummyHash = "$2a$10$abcdefghijklmnopqrstuuQHHpJUcD3JE9DqJxqkpAjW3kjjVeIQK"
